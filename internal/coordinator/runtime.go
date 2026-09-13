// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
)

// ha returns the runtime of the CURRENT connection.
//
// Never store the result across a call that can block on the broker: a
// reconnect replaces it wholesale (see [Coordinator.resetHAPlane]) and a
// goroutine holding the old one would be writing bookkeeping for a connection
// that no longer exists.
func (c *Coordinator) ha() *publisher.Runtime { return c.haRuntime.Load() }

// resetHAPlane builds a fresh publisher.Runtime for the connection that has
// just come up and discards the old one.
//
// See [RuntimeFactory] for why this is a whole-object replacement rather than
// three fields being cleared: the runtime's memo of what it has superseded,
// declared and announced is a statement about a broker, made per process, and a
// QoS 0 "success" is a statement about one connection. Step 5 supersedes
// nothing — this is here so that step 6, which does, cannot inherit
// go-mtec2mqtt's shipped defect (its PR #54, F1).
func (c *Coordinator) resetHAPlane() {
	if c.deps.NewHARuntime == nil {
		return
	}
	fresh := c.deps.NewHARuntime()
	if fresh == nil {
		c.deps.Logger.Error("coordinator.ha_runtime_reset_failed",
			slog.String("hint", "the runtime factory returned nil; the discovery plane keeps the previous connection's memo"))
		return
	}
	if old := c.haRuntime.Swap(fresh); old != nil {
		old.Close()
	}
}

// publishState writes one retained state or attributes payload through the
// go-hamqtt state plane.
//
// Every retained topic this daemon owns except the bridge availability marker
// goes through here — the per-point cloud publish, the synthetic hvac/fan/swing/
// preset slots, the Faikin read path, the optimistic write-through, the
// scheduler's own plane and the data_source attributes documents. Before this
// step those were nine separate mqtt.Publish calls with four different log
// keys; the delivery guarantee was stated nine times.
//
// written reports that the payload actually reached the broker; ok reports that
// the plane accepted it. The two differ because of the library's dedup gate: a
// byte-identical repeat of a value already on the topic is suppressed, which on
// this bridge is most of a poll — every sensor that did not move. That is the
// gap the coordinator.published log line now reports, and it is the one
// behaviour of this step a broker capture can see: strictly fewer messages,
// never different ones. [publisher.StatePublisher.Reset] re-opens the gate on
// every reconnect, because a broker back without its retained store holds
// nothing the cache still believes is there.
//
// An empty payload routes to Evict rather than Publish. The two produce the
// same three wire values — no bytes, retained, QoS 0, which is how MQTT clears
// a retained topic — but Publish refuses zero bytes with ErrEmptyStatePayload
// on purpose, and Evict also drops the topic from the dedup index so the next
// real value is not compared against a retraction. This bridge does publish
// empty state: an unscheduled device's schedule_next_change is "" in four of
// the twelve pinned scenarios.
func (c *Coordinator) publishState(ctx context.Context, topic, payload string) (written, ok bool) {
	if c.deps.StatePlane == nil {
		return false, false
	}
	if payload == "" {
		if err := c.deps.StatePlane.Evict(ctx, topic); err != nil {
			c.deps.Logger.Warn("coordinator.state_publish_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
			return false, false
		}
		return true, true
	}
	written, err := c.deps.StatePlane.Publish(ctx, topic, []byte(payload))
	if err != nil {
		c.deps.Logger.Warn("coordinator.state_publish_failed",
			slog.String("topic", topic), slog.String("err", err.Error()))
		return false, false
	}
	return written, true
}

// sweepFanOut is why each retained config a report-only pass saw was left
// standing — the thing that makes the predicate reviewable rather than a count.
type sweepFanOut struct {
	// Claimed: this instance published it in the batch that just went out, or
	// the runtime still declares it.
	Claimed int
	// Sibling: the PAYLOAD carries a `daikin_` unique_id but names topics under
	// a device this instance does not poll (or under another MQTT root) —
	// another go-daikin2mqtt instance's config. NEVER retracted.
	Sibling int
	// Foreign: the topic sat in this bridge's namespace but the payload does
	// not carry a unique_id of ours at all — somebody else's config on a topic
	// that happens to look like one of ours. NEVER retracted.
	Foreign int
	// Orphan: ours, unclaimed — what a retracting pass would have cleared.
	Orphan []string
}

// sweepReport opens one publisher.Runtime.Sweep window in ReportOnly mode and
// returns the verdict, so the fan-out can be asserted rather than read out of a
// log line — and so the RETRACTION is [Coordinator.sweepOrphans]', taken on the
// payload predicate, rather than the library's, taken on the topic alone.
func (c *Coordinator) sweepReport(
	ctx context.Context, rt *publisher.Runtime, published map[string]bool,
) (sweepFanOut, publisher.SweepResult) {
	// published is keyed by the PER-ENTITY topics the batch's documents
	// supersede (plus the document topics themselves) — which is the form the
	// Inspect below looks a config up by. Keyed by document topics alone, as it
	// was, the claimed branch could never be reached (F-B).
	claimed := make(map[string]bool, len(published))
	for topic := range published {
		claimed[topic] = true
	}
	// The runtime's own declarations as well as this boot's batch: a topic this
	// process published earlier and did not publish again is still not an
	// orphan of somebody else's making.
	for _, topic := range rt.Declared() {
		claimed[topic] = true
	}

	var (
		mu  sync.Mutex
		fan sweepFanOut
	)
	res, err := rt.Sweep(ctx, publisher.SweepRequest{
		ReportOnly: true,
		Window:     c.collectWindow,
		Owns:       OwnsConfigTopic,
		// Inspect runs on the transport's read loop, so it parses and counts
		// and publishes nothing. The retraction — when step 6 arms one — must
		// happen after Sweep returns.
		Inspect: func(t publisher.ConfigTopic, body []byte) {
			topic := publisher.LegacyTopicByUniqueID(publisher.LegacyEntity{
				Prefix: rt.Prefix(), Platform: t.Platform, UniqueID: t.ObjectID,
			})
			if topic == "" {
				return
			}
			own := c.deps.HASS.IsOwnConfig(body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case claimed[topic]:
				fan.Claimed++
			case !own:
				// A config in this bridge's own namespace whose payload names a
				// device this instance does not poll is another instance's, and
				// this is the whole of F14's fix: neither instance can tell the
				// other's retained configs from its own by TOPIC — the legacy
				// form is keyed on unique_id and the bundle node id is
				// sanitize(dev.UID()), both identical between two instances —
				// so the payload is the only thing that can, and it is not
				// consulted optionally.
				if strings.HasPrefix(hass.ConfigUniqueID(body), hass.UniqueIDPrefix) {
					fan.Sibling++
				} else {
					fan.Foreign++
				}
			default:
				fan.Orphan = append(fan.Orphan, topic)
			}
		},
	})
	if err != nil {
		c.deps.Logger.Warn("coordinator.discovery_sweep_failed", slog.String("err", err.Error()))
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(fan.Orphan)
	return fan, res
}
