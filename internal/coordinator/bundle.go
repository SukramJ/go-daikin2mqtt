// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// ADR 0070 phase 8 step 6 — the discovery plane moves to the device bundle.
//
// Where step 5 left this daemon: state, commands, availability and the Last
// Will on go-hamqtt, discovery still 264 retained per-entity configs from the
// hand-written builders. This file replaces those with 31 retained device
// documents, one per Home Assistant device, each carrying that device's whole
// component set.
//
// # What the operator sees
//
// Nothing, if it works: every entity keeps its unique_id, so Home Assistant's
// registry entry — the entity_id, any rename, any custom icon, the history —
// survives the move. What changes is where the definition lives.
//
// # The order, and why it is not negotiable
//
// A per-entity config retained for a unique_id and a device document carrying
// the same id cannot coexist: Home Assistant refuses the second, symmetrically,
// with a single `WARNING [mqtt.entity] Received a conflicting MQTT discovery
// message` and no entities at all. So every per-entity config must be retracted
// BEFORE the document lands, and the retraction must be complete.
//
// publisher.Runtime.PublishBundle owns that order (dedup check, then supersede,
// then publish; a failed retraction aborts before the document). Two things
// this daemon owns:
//
//   - The legacy FORM. publisher.Config.LegacyEntityTopics replaces the
//     library's default rather than extending it, and the default reproduces 0
//     of this bridge's 264 config topics. It is stated once, in RuntimeConfig,
//     and a runtime built any other way is a boot panic (see newBundleRuntime's
//     check in New).
//   - The runtime's AGE. Its memo of what it has superseded is a statement
//     about a broker, made per process, while a QoS 0 "success" is a statement
//     about one connection. [Coordinator.resetHAPlane] therefore replaces the
//     whole object on every (re)connect — built at step 5 for exactly this
//     step — and this file adds the piece step 5 could not: the signature gate
//     above it is cleared with it (F16).

// bundlePublishOverhead is the byte margin [Coordinator.bundleFits] adds to a
// document's own length to approximate the MQTT PUBLISH packet it becomes.
//
// The packet is: fixed header (1) + remaining-length varint (up to 4) + topic
// length prefix (2) + topic + the v5 property block (1 for none) + payload.
// This bridge publishes discovery at QoS 0, so there is no packet identifier.
// 64 bytes covers all of it several times over on a topic this bridge can
// produce (the longest measured is 71 bytes), and being generous here is the
// safe direction: an over-estimate withholds a migration that would have
// worked, which is recoverable and loud; an under-estimate performs half of one.
const bundlePublishOverhead = 64

// maybePublishDiscovery (re)publishes the discovery plane when the entity set
// changed — as one device document per Home Assistant device.
//
// The signature gate above it is the same one the per-entity form had, with one
// difference that is a fix rather than a carry-over: [Coordinator.PublishOnline]
// clears it on every (re)connect (F16). Retained configs are not a durable fact
// about a broker — a broker restarted without its retained store holds none —
// and a gate that outlives the connection it was computed on is a fleet that
// never comes back. The library has its own byte-level gate underneath
// (PublishBundle returns written=false for an identical document), so a
// reconnect of an unchanged fleet costs one render and no broker writes.
func (c *Coordinator) maybePublishDiscovery(
	ctx context.Context,
	points []process.Point,
	infos map[string]hass.DeviceInfo,
	climateInfos map[string]hass.ClimateInfo,
) {
	rt := c.ha()
	if c.deps.HASS == nil || rt == nil {
		return
	}
	sig := discoverySignature(points) + c.scheduleSignature()
	c.mu.Lock()
	changed := sig != c.lastDiscSig
	c.mu.Unlock()
	if !changed {
		return
	}

	gen := c.discoveryGen.Load()
	bundles, ok := c.buildBundles(rt, points, infos, climateInfos)
	if !ok || len(bundles) == 0 {
		return
	}
	// The prior documents are read back BEFORE anything is published, because
	// that is the only moment they still exist: the publish below overwrites
	// them. See [Coordinator.loadPriorComponents].
	c.loadPriorComponents(ctx, rt)
	c.tombstone(bundles)

	published, allSent := c.publishBundles(ctx, rt, bundles)
	if !allSent {
		// Do not commit the signature: the next poll retries, so a transient
		// broker failure cannot permanently suppress discovery. And do not
		// sweep: see [Coordinator.sweepOrphans].
		return
	}
	if c.discoveryGen.Load() != gen {
		// A reconnect landed while this batch was on the wire. The fresh
		// runtime has published nothing, so committing the signature here
		// would suppress the republish that connection is owed.
		c.deps.Logger.Info("coordinator.discovery_generation_raced")
		return
	}
	c.mu.Lock()
	c.lastDiscSig = sig
	c.mu.Unlock()
	c.deps.Logger.Info("coordinator.discovery_published",
		slog.Int("devices", len(bundles)), slog.Int("entities", len(points)))
	c.sweepOrphans(ctx, published)
}

// buildBundles renders every device document and refuses, per device, anything
// that must not be published.
//
// Both refusals happen HERE and not on the publish path, and that is the whole
// point: publisher.Runtime.PublishBundle retracts the per-entity configs before
// it writes the document, so a document that fails at that moment costs the
// device its entire entity set — the configs are gone and nothing replaced
// them. Refusing early leaves the per-entity configs retained and the device's
// entities working, on the old form, with a loud log line.
//
//   - discovery.Validate is Home Assistant's own schemas. A blocking issue
//     means HA discards the document without a line in its log, so publishing
//     it would trade a working fleet for silence. (Measured at step 4: all 31
//     bundles carrying all 264 components validate clean, which is why this
//     gate has never fired here. It is the gate, not the measurement, that
//     makes that safe to rely on.)
//   - The broker's advertised Maximum Packet Size, see [Coordinator.bundleFits].
//
// One withheld device withholds the SWEEP for the whole batch, not just itself:
// a device still on the per-entity form has retained configs that this daemon
// no longer claims, which is precisely the shape the sweep exists to delete.
func (c *Coordinator) buildBundles(
	rt *publisher.Runtime,
	points []process.Point,
	infos map[string]hass.DeviceInfo,
	climateInfos map[string]hass.ClimateInfo,
) ([]hass.Bundle, bool) {
	prefix := rt.Prefix()
	bundles, err := c.deps.HASS.RenderBundles(prefix, version.Version, points, infos, climateInfos)
	if err != nil {
		c.deps.Logger.Error("coordinator.discovery_bundle_render_failed", slog.String("err", err.Error()))
		return nil, false
	}
	scheds, err := c.deps.HASS.RenderScheduleBundles(prefix, version.Version, c.scheduleInfos(), c.webConfigURL())
	if err != nil {
		c.deps.Logger.Error("coordinator.discovery_bundle_render_failed", slog.String("err", err.Error()))
		return nil, false
	}
	bundles = append(bundles, scheds...)

	out := make([]hass.Bundle, 0, len(bundles))
	allOK := true
	for _, b := range bundles {
		if !c.bundleValidates(b) || !c.bundleFits(b) {
			allOK = false
			continue
		}
		out = append(out, b)
	}
	return out, allOK
}

// bundleValidates runs Home Assistant's own discovery schemas over one document.
// An advisory finding is logged and published; a blocking one is withheld.
func (c *Coordinator) bundleValidates(b hass.Bundle) bool {
	err := discovery.Validate(b.Bundle)
	if err == nil {
		return true
	}
	var ve *discovery.ValidationError
	if errors.As(err, &ve) && !ve.Blocking() {
		c.deps.Logger.Warn("coordinator.discovery_bundle_advisory",
			slog.String("topic", b.Topic), slog.Any("warnings", ve.Warnings))
		return true
	}
	c.deps.Logger.Error("coordinator.discovery_bundle_invalid",
		slog.String("topic", b.Topic), slog.String("err", err.Error()),
		slog.String("consequence",
			"the device document is withheld and this device's per-entity configs are left in place"))
	return false
}

// bundleFits preflights one document against the Maximum Packet Size the broker
// advertised in its CONNACK — BEFORE the retraction, which is the only place
// the check is worth anything.
//
// go-mqtt raises mqtt.ErrPacketTooLarge from its own write path, by which time
// publisher.Runtime.PublishBundle has already superseded every per-entity
// config the document carries. The device would be left with no discovery
// config at all.
//
// The limit checked is the broker's ADVERTISED OUTBOUND one
// (mqtt.ConnectResult.MaximumPacketSize, MQTT 5.0 property 0x27) and not
// mqtt.TCPConfig.MaximumPacketSize, which is this client's own INBOUND cap and
// has nothing to do with what the broker will accept.
//
// Unknown is treated as unknown, not as small: no hook wired, a client that has
// not connected, an MQTT 3.1.1 link and a broker that set no limit all skip the
// check. The spec's meaning of an absent property IS "no limit", so refusing on
// unknown would withhold the migration from every v3.1.1 installation for
// nothing. The guard can therefore only ever prevent a publish that would
// genuinely have failed.
//
// Measured for this bridge: 31 documents across the twelve pinned scenarios,
// the largest 13 951 bytes of payload on a 71-byte topic — see
// TestBundleDocumentsFitCommonBrokerLimits.
func (c *Coordinator) bundleFits(b hass.Bundle) bool {
	if c.deps.BrokerMaxPacketSize == nil {
		return true
	}
	maxPacket, known := c.deps.BrokerMaxPacketSize()
	if !known || maxPacket == 0 {
		return true
	}
	payload, err := json.Marshal(b.Bundle)
	if err != nil {
		c.deps.Logger.Error("coordinator.discovery_bundle_marshal_failed",
			slog.String("topic", b.Topic), slog.String("err", err.Error()))
		return false
	}
	size := uint64(len(payload)) + uint64(len(b.Topic)) + bundlePublishOverhead
	if size <= uint64(maxPacket) {
		return true
	}
	c.deps.Logger.Error("coordinator.discovery_bundle_too_large",
		slog.String("topic", b.Topic),
		slog.Uint64("bytes", size),
		slog.Uint64("broker_maximum", uint64(maxPacket)),
		slog.String("consequence",
			"the device document is withheld and this device's per-entity configs are left in place; "+
				"raise the broker's maximum packet size"))
	return false
}

// publishBundles writes each device document, retracting its per-entity configs
// first (inside publisher.Runtime.PublishBundle).
//
// It returns the claim set the sweep subtracts — which names every document of
// this batch INCLUDING one whose publish failed, so a transient broker error
// can never make the sweep clear a config this daemon still intends to
// publish — and whether all of them reached the socket.
func (c *Coordinator) publishBundles(
	ctx context.Context, rt *publisher.Runtime, bundles []hass.Bundle,
) (map[string]bool, bool) {
	published := make(map[string]bool, len(bundles))
	allSent := true
	for _, b := range bundles {
		published[b.Topic] = true
		written, err := rt.PublishBundle(ctx, b.Bundle)
		if err != nil {
			allSent = false
			c.deps.Logger.Warn("coordinator.discovery_bundle_failed",
				slog.String("topic", b.Topic), slog.String("err", err.Error()))
			continue
		}
		c.recordPublished(b)
		c.deps.Logger.Info("coordinator.discovery_bundle_published",
			// The topic is logged because it is the ONE spelling of it a
			// rolling-back operator can trust: the node id is sanitize()d, so
			// composing it from a device id by hand is how a documented
			// downgrade command ends up clearing nothing.
			slog.String("topic", b.Topic),
			slog.Int("components", len(b.Bundle.Components)),
			slog.Bool("written", written))
	}
	return published, allSent
}

// --- tombstones -------------------------------------------------------------

// loadPriorComponents reads back the device documents this bridge already has
// retained, once per connection, before the first publish of that connection
// overwrites them.
//
// It exists because an omitted component is NOT a removed entity (see
// [hass.ApplyTombstones]) and because the orphan sweep cannot take over the job:
// its ownership predicate declines device documents on purpose
// ([OwnsConfigTopic]).
//
// Every failure direction here produces FEWER tombstones, never different ones:
// a window that sees nothing, a document that does not parse, a component with
// no platform or a unique_id outside this bridge's namespace all simply do not
// become prior state, and the publish proceeds exactly as it would have without
// this step. That is what makes a read-back safe to put in front of the one
// publish that cannot be undone — the failure mode is the behaviour of not
// having it.
func (c *Coordinator) loadPriorComponents(ctx context.Context, rt *publisher.Runtime) {
	c.mu.Lock()
	done := c.priorLoaded
	c.mu.Unlock()
	if done {
		return
	}

	prior := map[string]map[string]discovery.Component{}
	var mu sync.Mutex
	_, err := rt.Sweep(ctx, publisher.SweepRequest{
		// A look without a touch: the pass that is safe before the first
		// publish, and the only kind this daemon ever runs.
		ReportOnly: true,
		Window:     c.collectWindow,
		Owns:       func(t publisher.ConfigTopic) bool { return t.Bundle && t.NodeID != "" },
		Inspect: func(t publisher.ConfigTopic, body []byte) {
			// The payload predicate, not the topic: a sibling instance's
			// document sits on a topic this one cannot distinguish from its
			// own (F14), and tombstoning from a sibling's component set would
			// delete entities this instance never published.
			if !c.deps.HASS.BundleIsOwnConfig(body) {
				return
			}
			comps := hass.BundleComponents(body)
			if len(comps) == 0 {
				return
			}
			mu.Lock()
			prior[t.NodeID] = comps
			mu.Unlock()
		},
	})
	if err != nil {
		c.deps.Logger.Warn("coordinator.discovery_prior_read_failed", slog.String("err", err.Error()),
			slog.String("consequence", "a component removed since the last run stays as a phantom entity"))
	}
	mu.Lock()
	defer mu.Unlock()
	c.mu.Lock()
	c.priorComponents = prior
	c.priorLoaded = true
	c.mu.Unlock()
	c.deps.Logger.Debug("coordinator.discovery_prior_read", slog.Int("documents", len(prior)))
}

// tombstone marks, in each document, every component the PREVIOUS document for
// that device carried and this one does not.
func (c *Coordinator) tombstone(bundles []hass.Bundle) {
	c.mu.Lock()
	prior := c.priorComponents
	c.mu.Unlock()
	if len(prior) == 0 {
		return
	}
	for _, b := range bundles {
		gone := hass.ApplyTombstones(b.Bundle, prior[b.Bundle.NodeID])
		if len(gone) == 0 {
			continue
		}
		c.deps.Logger.Info("coordinator.discovery_components_removed",
			slog.String("topic", b.Topic), slog.Any("components", gone))
	}
}

// recordPublished makes the document just written the prior state the next
// publish diffs against, so a component removed while this process is running
// is tombstoned exactly once rather than on every publish forever.
func (c *Coordinator) recordPublished(b hass.Bundle) {
	live := make(map[string]discovery.Component, len(b.Bundle.Components))
	for _, key := range b.Bundle.Keys() {
		// A tombstone is not prior state: it is a component that is already
		// gone. Carrying it forward would keep re-marking a dead key in every
		// future document.
		if b.Bundle.Components[key].UniqueID == "" {
			continue
		}
		live[key] = b.Bundle.Components[key]
	}
	c.mu.Lock()
	if c.priorComponents == nil {
		c.priorComponents = map[string]map[string]discovery.Component{}
	}
	c.priorComponents[b.Bundle.NodeID] = live
	c.mu.Unlock()
}

// --- the sweep --------------------------------------------------------------

// sweepOrphans retracts this instance's own retained PER-ENTITY discovery
// configs that the device documents did not supersede.
//
// Armed, as of this step, and the arming is what step 5 could not do. Its
// argument then was arithmetic: the runtime published no config at all, so its
// claim set was empty and any acting pass would have judged this bridge's whole
// retained fleet an orphan. Now the documents go out through that same runtime,
// so the claim set is exactly the batch just published plus what the runtime
// still declares — established, not assumed, by the allSent guard above and by
// TestTheSweepIsArmedOnlyOverAPopulatedClaimSet.
//
// What it actually clears after the migration: a per-entity config for an
// entity that no longer exists anywhere. publisher.SupersededTopics retracts
// only what a document CARRIES, so an entity dropped from the catalogue between
// releases leaves a config the migration itself cannot reach.
//
// Three things it deliberately does not do:
//
//   - It does not use SweepResult.Unclaimed, now or ever. That list is
//     topic-only, and every string publisher.ConfigTopic offers is byte-
//     identical between two go-daikin2mqtt instances seeing one ONECTA device
//     (F14). Acting on it would delete a sibling's fleet — it is the
//     composition that cleared 29 live configs in a sibling repo. The verdict
//     is taken in Inspect, against the retained PAYLOAD, and nothing else.
//   - It does not claim device documents ([OwnsConfigTopic] declines them), so
//     it can never retract a bundle — a sibling's or its own.
//   - It does not run at all unless every document of the batch was PUBLISHED,
//     not merely built. The two come apart exactly where it matters: a valid
//     document whose publish failed (an open circuit breaker, a broker
//     brownout) leaves the per-entity configs that are still carrying the
//     fleet, and a sweep would then delete them and put nothing back.
func (c *Coordinator) sweepOrphans(ctx context.Context, published map[string]bool) {
	if c.deps.HASS == nil || !c.reconcileGate.TryLock() {
		return
	}
	go func() {
		defer c.reconcileGate.Unlock()
		rt := c.ha()
		if rt == nil {
			return
		}
		if len(published) == 0 {
			c.deps.Logger.Warn("coordinator.discovery_sweep_skipped",
				slog.String("reason", "no device document was published"))
			return
		}
		fan, res := c.sweepReport(ctx, rt, published)
		c.deps.Logger.Info("coordinator.discovery_sweep",
			slog.Int("inspected", res.Inspected),
			slog.Int("claimed", fan.Claimed),
			slog.Int("sibling", fan.Sibling),
			slog.Int("foreign", fan.Foreign),
			slog.Int("retracting", len(fan.Orphan)),
			slog.Any("orphans", fan.Orphan))
		if len(fan.Orphan) == 0 {
			return
		}
		if err := rt.Retract(ctx, fan.Orphan...); err != nil {
			c.deps.Logger.Warn("coordinator.discovery_sweep_retract_failed", slog.String("err", err.Error()))
		}
	}()
}
