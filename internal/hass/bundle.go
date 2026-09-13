// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// ADR 0070 phase 8 step 6: the device bundle.
//
// This file renders what step 4 proved and step 5 plumbed — one retained
// `<prefix>/device/<node_id>/config` document per Home Assistant device,
// carrying that device's entire component set — instead of the 264 separate
// per-entity configs this bridge has published since 0.1.
//
// # The one ordering that cannot be got wrong
//
// A per-entity config retained for a unique_id and a device document carrying
// the same id CANNOT coexist. Home Assistant refuses the second, symmetrically,
// with exactly one line in its own log —
// `WARNING [mqtt.entity] Received a conflicting MQTT discovery message` — and
// no entities. Nothing on the wire says why.
//
// So the retraction comes first and must be COMPLETE before the document lands.
// The ordering itself lives in publisher.Runtime.PublishBundle (dedup check,
// then supersede, then publish, aborting before the document if any retraction
// fails); this bridge's job is only to state the legacy form
// ([LegacyConfigTopicForms], measured 264 of 264 at step 4) and to make sure the
// runtime that remembers the retraction is never older than the connection it
// was made on (coordinator.RuntimeFactory).

// OriginName and OriginURL are the `origin` block a device document must carry:
// discovery.Validate makes origin.name a BLOCKING issue on a bundle, and a
// blocking bundle publishes nothing at all. On the per-entity form it was
// optional, which is why F12 of the phase 8 measurement deferred it to here.
const (
	OriginName = "go-daikin2mqtt"
	OriginURL  = "https://github.com/SukramJ/go-daikin2mqtt"
)

// BundleOrigin is the origin block every device document of this bridge
// carries.
//
// sw is a PARAMETER rather than a read of internal/version, so that cutting a
// tag does not stale the pinned bundle artefact. The wiring to the real build
// version is asserted separately (TestBundleOriginIsWiredFromTheBuildVersion),
// which is the split that keeps both facts checkable.
func BundleOrigin(sw string) discovery.Origin {
	return discovery.Origin{Name: OriginName, SW: sw, URL: OriginURL}
}

// Bundle is one rendered device document plus the topic it is published to.
type Bundle struct {
	Topic  string
	Bundle *discovery.Bundle
}

// Components returns the component keys, sorted — for a log line and for the
// tests that compare a document against the per-entity fleet it replaces.
func (b Bundle) Components() []string {
	if b.Bundle == nil {
		return nil
	}
	return b.Bundle.Keys()
}

// RenderBundles renders one device document per Home Assistant device, from
// exactly the inputs [Discovery.Publish] used to take.
//
// prefix is the discovery prefix the documents will be published under. It is
// passed in rather than read off d.baseTopic so the topic this function
// returns and the topic publisher.Runtime.PublishBundle actually writes are the
// same string: the library normalises a prefix written with a trailing slash
// and this package does not (F15).
func (d *Discovery) RenderBundles(
	prefix, sw string,
	points []process.Point,
	infos map[string]DeviceInfo,
	climateInfos map[string]ClimateInfo,
) ([]Bundle, error) {
	devs, err := d.HamqttModel(points, infos, climateInfos)
	if err != nil {
		return nil, err
	}
	return d.bundleDevices(prefix, sw, devs)
}

// RenderScheduleBundles is [Discovery.RenderBundles] for the weekly scheduler's
// switches, which live on the daemon's own Home Assistant device.
func (d *Discovery) RenderScheduleBundles(prefix, sw string, schedules []ScheduleInfo, configURL string) ([]Bundle, error) {
	devs, err := d.HamqttScheduleModel(schedules, configURL)
	if err != nil {
		return nil, err
	}
	return d.bundleDevices(prefix, sw, devs)
}

// bundleDevices renders each modelled device as one discovery.Bundle.
//
// discovery.Render applies [model.ApplySuppression] itself, so the composite
// climate's three consumed controls are absent from the document for the same
// reason and by the same mechanism as they are absent from the per-entity
// fleet — one decision, not two.
func (d *Discovery) bundleDevices(prefix, sw string, devs []HamqttDevice) ([]Bundle, error) {
	ctx := d.HamqttContext()
	origin := BundleOrigin(sw)
	out := make([]Bundle, 0, len(devs))
	for _, hd := range devs {
		b, err := discovery.Render(ctx, hd.Device, hd.Entities, origin)
		if err != nil {
			return nil, fmt.Errorf("hass: render bundle for %s: %w", hd.Device.UID(), err)
		}
		out = append(out, Bundle{
			// publisher.BundleConfigTopic rather than discovery.Bundle.Topic,
			// because that is the call PublishBundle makes: it normalises the
			// prefix, and Bundle.Topic concatenates. A prefix written with a
			// trailing slash would otherwise make this function name a topic
			// the runtime never writes.
			Topic:  publisher.BundleConfigTopic(prefix, b.NodeID),
			Bundle: b,
		})
	}
	return out, nil
}

// BundleComponents decodes the `components` map of a retained device document.
//
// Only the identity keys survive the round trip (`platform` and `unique_id`
// are ordinary struct fields; Fields and Extra are json:"-"), and those are
// exactly the two [discovery.Bundle.RemoveComponents] needs to write a
// tombstone: the platform goes into the platform-only entry Home Assistant
// reads as a deletion, and the unique_id is remembered OUTSIDE the payload so
// publisher.SupersededTopics can still retract the removed entity's legacy
// per-entity config.
//
// It returns nil for anything that is not a well-formed document with a
// non-empty component map, and that direction is deliberate: the caller turns
// this into tombstones, and a tombstone for a component that is actually alive
// deletes a working entity. A read that cannot be trusted must produce FEWER
// tombstones, never different ones.
func BundleComponents(payload []byte) map[string]discovery.Component {
	var doc struct {
		Components map[string]discovery.Component `json:"components"`
	}
	if json.Unmarshal(payload, &doc) != nil || len(doc.Components) == 0 {
		return nil
	}
	out := make(map[string]discovery.Component, len(doc.Components))
	for key := range doc.Components {
		// A component with no platform cannot be tombstoned (the library skips
		// it), and one with no unique_id of ours is not evidence of anything
		// this instance published. An already-tombstoned entry — platform only,
		// no unique_id — is dropped here for the same reason: re-tombstoning it
		// forever would keep a dead key in every future document.
		if doc.Components[key].Platform == "" || !strings.HasPrefix(doc.Components[key].UniqueID, UniqueIDPrefix) {
			continue
		}
		out[key] = doc.Components[key]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ApplyTombstones marks every component that the previous document for this
// node carried and the new one does not.
//
// An omitted component is NOT removed from Home Assistant: the entity stays in
// the registry, keeps its history, and — because this bridge's availability is
// bridge-level and the bridge is online — reads AVAILABLE rather than
// unavailable, so nothing on screen says it is dead. Removal has to be
// expressed: an entry present in `components` carrying ONLY a platform, plus
// the removed component's unique_id remembered in Bundle.Tombstones (json:"-",
// outside the payload — putting it back in would un-remove the entity it exists
// to finish removing), which is what lets publisher.SupersededTopics also clear
// the removed entity's retained per-entity config.
//
// Under the per-entity form the orphan sweep did this job. On a bundle it
// cannot: the sweep's ownership predicate declines device documents (see
// coordinator.OwnsConfigTopic), so without this the capability is simply lost —
// which is a regression this bridge would feel, because deleting a weekly
// schedule, turning LOCAL_MODE off and shipping a characteristics.yaml edit all
// remove components from a device that is otherwise unchanged.
//
// It returns the keys it tombstoned, sorted.
func ApplyTombstones(b *discovery.Bundle, prior map[string]discovery.Component) []string {
	if b == nil || len(prior) == 0 {
		return nil
	}
	live := map[string]bool{}
	for _, k := range b.Keys() {
		live[k] = true
	}
	var gone []string
	for key := range prior {
		if !live[key] {
			gone = append(gone, key)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	sort.Strings(gone)
	b.RemoveComponents(prior, gone...)
	return gone
}

// BundleIsOwnConfig is [Discovery.IsOwnConfig] for a retained device document.
//
// A bundle carries no top-level unique_id and no top-level `*_topic` keys — the
// topics sit one level down, inside each component — so the per-entity
// predicate declines every bundle, in both directions. That is safe (nothing is
// retracted that should not be) but it is not an ANSWER, and step 6 needs one:
// the sweep has to be able to say "that device document is a sibling
// instance's" rather than "I cannot tell".
//
// The rule is the per-entity rule applied component by component, unchanged in
// substance: every topic key of every component, bar the bridge-level
// availability_topic, must sit under this instance's own MQTT root AND under a
// device segment this instance actually polls, and at least one component must
// carry a unique_id in this bridge's namespace.
func (d *Discovery) BundleIsOwnConfig(payload []byte) bool {
	var doc struct {
		Components map[string]json.RawMessage `json:"components"`
	}
	if json.Unmarshal(payload, &doc) != nil || len(doc.Components) == 0 {
		return false
	}
	owned := d.ownedDevices()
	if len(owned) == 0 {
		// Ownership that cannot be proven is not claimed. Before the first
		// resolved poll this instance knows of no device, and a decision taken
		// on state the daemon has not learned yet cannot be undone.
		//
		// Redundant today, and stated anyway: an empty claim set makes
		// ownsTopic decline every topic, and a document naming none is refused
		// by the `named > 0` test below. Removing this line changes no
		// behaviour (it is the one mutation of this step that survives, and it
		// survives because it is equivalent). It is here because the rule is
		// easier to read at the top than to reconstruct from two conditions
		// further down.
		return false
	}
	ours, named := false, 0
	for _, raw := range doc.Components {
		var comp map[string]any
		if json.Unmarshal(raw, &comp) != nil {
			return false
		}
		if uid, _ := comp["unique_id"].(string); strings.HasPrefix(uid, UniqueIDPrefix) {
			ours = true
		}
		ok, n := d.ownsNamedTopics(owned, comp)
		if !ok {
			return false
		}
		named += n
	}
	// The namespace alone is not instance-specific — every go-daikin2mqtt
	// instance shares it — so a document that named no topic of ours has
	// proved nothing, exactly as on the per-entity form (F18).
	return ours && named > 0
}
