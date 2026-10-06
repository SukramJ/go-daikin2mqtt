// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
)

// The retained sweep of openccu-loom ADR 0083 ("Migration").
//
// 0.14.0 moved every topic from `<root>/<device>/<emb>/<leaf>/state` to
// `<name>/status/<device>/<emb>/<leaf>` with no compatibility switch, so the
// broker still holds what 0.13 left retained: every value, every attributes
// document, the scheduler switches and `<root>/bridge/status` = online. Nothing
// would ever overwrite those again. On every start of this release the daemon
// clears what it can prove is its own, and nothing else:
//
//  1. It listens, retained messages only, for a bounded window, to the
//     subtrees that can hold its own topics — `<root>/<device>/#` for each
//     device the first poll resolved, `<root>/scheduler/#`, `<root>/bridge/#`
//     and `<name>/status/<device>/#` — never `<root>/#`, which would also
//     overlap the live `<name>/set/+/+/+` command subscription.
//  2. A topic whose second level is a function name is new and is never
//     treated as old. A device id, `scheduler` and `bridge` cannot spell one.
//  3. Anything else is cleared only if it is an EXACT 0.13 shape for an
//     identifier this instance owns: a device the last poll resolved, a
//     schedule its own engine holds, or `<root>/bridge/status`. Never a prefix
//     match — ADR 0070's homeconnect review measured a prefix rule deleting
//     510 live components of a sibling instance.
//  4. The same read-back serves spec §3.2's steady-state rule: a status item
//     of an owned device that this run's first complete picture no longer
//     contains — a characteristic dropped from the catalogue, LOCAL_MODE
//     turned off — is cleared too.
//
// The Faikin firmware's own topics (`state/<host>`, `command/<host>/<suffix>`)
// live outside `<root>/` and are not subscribed; a root named like one of their
// first levels still could not match, because a Faikin host is no ONECTA
// device id. The sweep is idempotent — a second start finds nothing — and it
// also cleans up after a rollback and re-upgrade. It goes in the next breaking
// release.

// sweepVerdict is what the sweep does with one retained topic.
type sweepVerdict int

const (
	// sweepKeep leaves the topic alone.
	sweepKeep sweepVerdict = iota
	// sweepLegacy clears a 0.13 topic this instance owns.
	sweepLegacy
	// sweepStale clears a current-layout status item of an owned device that
	// this run does not publish.
	sweepStale
)

// classifyRetained decides one retained topic. owned is the set of ONECTA
// device ids the first poll resolved, schedules the ids the attached engine
// holds, current the status items this process has published.
//
// A pure function so that what the sweep may touch is assertable topic by
// topic, without a broker.
func classifyRetained(root, topic string, owned, schedules, current map[string]bool) sweepVerdict {
	rest, ok := strings.CutPrefix(topic, root+"/")
	if !ok {
		return sweepKeep
	}
	segs := strings.Split(rest, "/")
	if hatopic.IsFunction(segs[0]) {
		if segs[0] == hatopic.FunctionStatus && len(segs) >= 3 && owned[segs[1]] && !current[topic] {
			return sweepStale
		}
		return sweepKeep
	}
	switch {
	case len(segs) == 2 && segs[0] == layout.LegacyBridgeDevice && segs[1] == layout.LegacyBridgeLeaf:
		return sweepLegacy
	case len(segs) == 4 && segs[0] == layout.SchedulerDeviceID && schedules[segs[1]] &&
		segs[2] == layout.EnabledTopic && (segs[3] == layout.LegacyStateSuffix || segs[3] == layout.LegacySetSuffix):
		return sweepLegacy
	case len(segs) == 4 && owned[segs[0]] && segs[1] != "" && segs[2] != "" &&
		layout.IsLegacyLeaf(segs[3]):
		return sweepLegacy
	}
	return sweepKeep
}

// maybeSweepLegacy runs the sweep once per process, after the first poll that
// resolved any device: the first complete picture exists then, and with it the
// device ids this instance owns — the one moment the 0.13 layout's leftovers
// can be told from a sibling's. It runs on the poll loop, which costs that loop
// one collect window once; writes drain on their own goroutine meanwhile.
func (c *Coordinator) maybeSweepLegacy(ctx context.Context) {
	c.mu.Lock()
	devices := c.lastDevices
	c.mu.Unlock()
	if len(devices) == 0 || !c.swept.CompareAndSwap(false, true) {
		return
	}
	c.sweepLegacyTopics(ctx, devices)
}

// sweepLegacyTopics runs the sweep over the given owned device ids.
func (c *Coordinator) sweepLegacyTopics(ctx context.Context, devices []string) {
	if c.deps.MQTT == nil || len(devices) == 0 {
		return
	}
	root := c.topicRoot.String()
	owned := make(map[string]bool, len(devices))
	filters := []string{
		root + "/" + layout.LegacyBridgeDevice + "/#",
		root + "/" + layout.SchedulerDeviceID + "/#",
	}
	for _, id := range devices {
		// A device id that is not one clean topic level cannot be the
		// segment of anything this daemon wrote, and as a filter it could
		// widen the subscription.
		if id == "" || hatopic.Safe(id) != id || hatopic.IsFunction(id) {
			continue
		}
		owned[id] = true
		filters = append(filters, root+"/"+id+"/#", root+"/"+hatopic.FunctionStatus+"/"+id+"/#")
	}
	schedules := c.scheduleIDs()

	seen := c.collectRetained(ctx, filters)

	current := map[string]bool{}
	if c.deps.StatePlane != nil {
		for _, t := range c.deps.StatePlane.Published() {
			current[t] = true
		}
	}
	var legacy, stale []string
	for _, t := range seen {
		switch classifyRetained(root, t, owned, schedules, current) {
		case sweepLegacy:
			legacy = append(legacy, t)
		case sweepStale:
			stale = append(stale, t)
		case sweepKeep:
		}
	}
	for _, t := range legacy {
		if err := c.deps.MQTT.Publish(ctx, t, nil, mqtt.QoS0, true); err != nil {
			c.deps.Logger.Warn("coordinator.legacy_sweep_clear_failed",
				slog.String("topic", t), slog.String("err", err.Error()))
		}
	}
	if len(stale) > 0 && c.deps.StatePlane != nil {
		if err := c.deps.StatePlane.Evict(ctx, stale...); err != nil {
			c.deps.Logger.Warn("coordinator.legacy_sweep_clear_failed", slog.String("err", err.Error()))
		}
	}
	c.deps.Logger.Info("coordinator.legacy_sweep",
		slog.Int("inspected", len(seen)),
		slog.Int("legacy_cleared", len(legacy)),
		slog.Int("stale_cleared", len(stale)))
}

// collectRetained subscribes to filters for one collect window and returns the
// retained, non-empty topics delivered on them, sorted. The handler runs on the
// transport's read loop, so it only records.
func (c *Coordinator) collectRetained(ctx context.Context, filters []string) []string {
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	handler := func(msg *mqtt.Message) {
		if !msg.Retain || len(msg.Payload) == 0 {
			return
		}
		mu.Lock()
		seen[msg.Topic] = true
		mu.Unlock()
	}
	var subscribed []string
	for _, f := range filters {
		if _, err := c.deps.MQTT.Subscribe(ctx, f, mqtt.QoS0, handler); err != nil {
			c.deps.Logger.Warn("coordinator.legacy_sweep_subscribe_failed",
				slog.String("filter", f), slog.String("err", err.Error()))
			continue
		}
		subscribed = append(subscribed, f)
	}
	timer := time.NewTimer(c.collectWindow)
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	for _, f := range subscribed {
		if err := c.deps.MQTT.Unsubscribe(ctx, f); err != nil {
			c.deps.Logger.Debug("coordinator.legacy_sweep_unsubscribe_failed",
				slog.String("filter", f), slog.String("err", err.Error()))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// scheduleIDs is the set of schedule ids the attached engine holds, or none.
func (c *Coordinator) scheduleIDs() map[string]bool {
	out := map[string]bool{}
	eng := c.scheduleEngine()
	if eng == nil {
		return out
	}
	doc := eng.Document()
	for i := range doc.Schedules {
		out[doc.Schedules[i].ID] = true
	}
	return out
}
