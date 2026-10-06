// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package layout composes every MQTT topic this bridge publishes to or
// subscribes to, so the string exists in exactly one place.
//
// # Why this package exists
//
// The state topic used to be composed in twelve separate fmt.Sprintf
// expressions across four packages, and nothing compared them — the cloud
// poll, the Faikin read path, the weekly scheduler, the composite climate's
// five synthetic slots and the discovery payloads that name all of them. A
// drift in any one leaves entities pointing at a topic nobody writes:
// permanently `unknown` in Home Assistant, nothing in the log. That is F3 of
// the ADR 0070 phase 8 measurement, and TestStateTopicBuildersAgree in
// internal/coordinator is the pin that catches it.
//
// # The grammar
//
// Since 0.14.0 the tree follows mqtt-smarthome 2.0 (openccu-loom ADR 0083),
// rendered by go-hamqtt's [hatopic.SmartHome]:
//
//	<name>/status/<deviceID>/<embeddedID>/<leaf>             status item
//	<name>/status/<deviceID>/<embeddedID>/<leaf>/attributes  data_source document
//	<name>/set/<deviceID>/<embeddedID>/<leaf>                command, same item path
//	<name>/status/<deviceID>/online                          device reachability
//	<name>/{status,set}/scheduler/<scheduleID>/enabled       schedule switch
//	<name>/connected                                         0/1/2, the Last Will
//	<name>/info, <name>/maintenance/…                        instance topics
//
// The function sits at the second level instead of the old `/state` and
// `/set` suffixes, so every item is three levels below it and one wildcard
// filter, `<name>/set/+/+/+`, covers every command this bridge advertises.
//
// [Legacy] is the 0.13 layout, kept for exactly two readers: the start-up
// sweep that clears what an older release left retained, which must match the
// old shapes exactly, and the frozen per-entity discovery builders the
// migration's retraction contract is pinned against.
//
// Everything here is a pure function of a name and the path segments. The
// identity-bearing discovery *config* topic (which embeds a unique_id) stays in
// internal/hass, because it is an identity question rather than a layout one.
//
// The package is named layout rather than topic because `topic` is the obvious
// name for a local string in almost every publish signature in this repository,
// and gocritic's importShadow flags the collision.
package layout

import (
	"strings"

	hatopic "github.com/SukramJ/go-hamqtt/topic"
)

// SchedulerDeviceID is the reserved first-level item the weekly scheduler's
// own entities live under, beside the device UUIDs. It is not a Daikin device:
// the schedule enable switches belong to the daemon. An ONECTA UUID cannot
// spell it, nor any function name.
const SchedulerDeviceID = "scheduler"

// EnabledTopic is the leaf segment of a schedule's enable switch, so that
// "<name>/status/scheduler/<scheduleID>/enabled" needs no special case: it is
// an ordinary three-level item whose device is the scheduler and whose
// embedded id is the schedule.
const EnabledTopic = "enabled"

// ClimateTopic is the leaf segment the composite climate entity's attributes
// document hangs off. The entity itself has no state topic of its own — it
// reads the topics of the points it composes.
const ClimateTopic = "climate"

// AttributesLeaf is the item level below a slot that carries its
// json_attributes document (the data_source of the value).
const AttributesLeaf = "attributes"

// OnlineLeaf is the item level below a device that carries its reachability.
const OnlineLeaf = "online"

// Root is the MQTT instance name this daemon owns (MQTT_TOPIC, default
// "daikin") and every topic derived from it. Changing it moves the whole tree
// and orphans nothing in Home Assistant's registries — the identity strings are
// built in internal/hass and do not contain it.
type Root struct{ sh hatopic.SmartHome }

// New returns the layout for name. A name that is not a valid topic level
// sequence — empty, a wildcard, NUL, a leading `$` — panics: config.Validate
// refuses it before anything reaches here. A multi-level name (`haus/klima`)
// is accepted for installations that already run with one; [Root.Conformant]
// reports that it is outside spec §3.
func New(name string) Root {
	sh, err := hatopic.NewSmartHomeMultiLevel(name)
	if err != nil {
		panic("layout: " + err.Error())
	}
	return Root{sh: sh}
}

// String returns the name itself.
func (r Root) String() string { return r.sh.Name() }

// SmartHome is the go-hamqtt layout this root renders through.
func (r Root) SmartHome() hatopic.SmartHome { return r.sh }

// Conformant reports whether the name is a single topic level, as spec §3
// requires; a `+/info` scan cannot see an instance whose name is not.
func (r Root) Conformant() bool { return r.sh.Conformant() }

// Connected is `<name>/connected`: the Last Will ("0"), and the 1/2 the
// daemon publishes on every connect and on every change of its upstream.
func (r Root) Connected() string { return r.sh.Connected() }

// Info is `<name>/info`.
func (r Root) Info() string { return r.sh.Info() }

// CommandFilter is the single wildcard subscription that covers every command
// topic this bridge advertises: "<name>/set/+/+/+". Its three item levels are
// why Slot has exactly the shape it has.
func (r Root) CommandFilter() string { return r.sh.Name() + "/" + hatopic.FunctionSet + "/+/+/+" }

// Online is a device's reachability item, `<name>/status/<deviceID>/online`.
func (r Root) Online(deviceID string) string { return r.sh.Status(deviceID, OnlineLeaf) }

// Slot addresses one entity's item: the device, the management point and the
// leaf name. The leaf is a catalogue `topic:` for a real characteristic and a
// synthetic name (hvac_mode, fan_mode, schedule_state, …) for a point the
// coordinator makes up; the layout does not care which.
func (r Root) Slot(deviceID, embeddedID, topic string) Slot {
	return Slot{sh: r.sh, item: []string{deviceID, embeddedID, topic}}
}

// Schedule addresses the enable switch of one weekly schedule, which lives on
// the daemon's own scheduler item.
func (r Root) Schedule(scheduleID string) Slot {
	return r.Slot(SchedulerDeviceID, scheduleID, EnabledTopic)
}

// Climate addresses the composite climate entity's own item (only its
// attributes sibling is written; the entity reads the slots it composes).
func (r Root) Climate(deviceID, embeddedID string) Slot {
	return r.Slot(deviceID, embeddedID, ClimateTopic)
}

// Slot is one entity's item. Its three topics are the whole vocabulary: the
// status the daemon publishes, the set Home Assistant sends, and a
// JSON-attributes item below it.
type Slot struct {
	sh   hatopic.SmartHome
	item []string
}

// Item is the slot's item path, "<device>/<embedded>/<topic>".
func (s Slot) Item() string { return strings.Join(s.item, "/") }

// State is the retained status item the daemon publishes the slot's value to,
// and the topic a discovery payload names as its state_topic.
func (s Slot) State() string { return s.sh.Status(s.item...) }

// Command is the set item Home Assistant writes to, matched by
// [Root.CommandFilter].
func (s Slot) Command() string { return s.sh.Set(s.item...) }

// Attributes is the slot's JSON-attributes status item, carrying the
// data_source document (cloud vs local Faikin).
func (s Slot) Attributes() string {
	return s.sh.Status(s.item[0], s.item[1], s.item[2], AttributesLeaf)
}

// --- the 0.13 layout ---------------------------------------------------------

// The old suffixes and the bridge status topic, named so the sweep that clears
// them matches exact shapes rather than anything that merely looks similar.
const (
	LegacyStateSuffix   = "state"
	LegacySetSuffix     = "set"
	LegacyBridgeDevice  = "bridge"
	LegacyBridgeLeaf    = "status"
	legacyAttributesSfx = AttributesLeaf
)

// Legacy is the topic layout every release up to 0.13 published:
//
//	<root>/<deviceID>/<embeddedID>/<topic>/state       retained value
//	<root>/<deviceID>/<embeddedID>/<topic>/set         command
//	<root>/<deviceID>/<embeddedID>/<topic>/attributes  data_source document
//	<root>/scheduler/<scheduleID>/enabled/{state,set}  schedule switch
//	<root>/bridge/status                               online/offline LWT
//
// Nothing publishes to it any more. It is what the start-up sweep recognises
// and what the frozen per-entity discovery builders still render, because
// those are pinned as the retraction contract of an older installed base.
type Legacy struct{ root string }

// NewLegacy returns the 0.13 layout rooted at root.
func NewLegacy(root string) Legacy { return Legacy{root: root} }

// String returns the root itself.
func (l Legacy) String() string { return l.root }

// BridgeStatus is the old availability topic, "<root>/bridge/status".
func (l Legacy) BridgeStatus() string {
	return l.root + "/" + LegacyBridgeDevice + "/" + LegacyBridgeLeaf
}

// Slot addresses one entity's old topic family.
func (l Legacy) Slot(deviceID, embeddedID, topic string) LegacySlot {
	return LegacySlot{base: l.root + "/" + deviceID + "/" + embeddedID + "/" + topic}
}

// Schedule addresses a schedule switch's old topic family.
func (l Legacy) Schedule(scheduleID string) LegacySlot {
	return l.Slot(SchedulerDeviceID, scheduleID, EnabledTopic)
}

// Climate addresses the composite climate's old topic family.
func (l Legacy) Climate(deviceID, embeddedID string) LegacySlot {
	return l.Slot(deviceID, embeddedID, ClimateTopic)
}

// LegacySlot is one entity's 0.13 topic family.
type LegacySlot struct{ base string }

// Base is the slot without a suffix, "<root>/<device>/<embedded>/<topic>".
func (s LegacySlot) Base() string { return s.base }

// State is "<base>/state".
func (s LegacySlot) State() string { return s.base + "/" + LegacyStateSuffix }

// Command is "<base>/set".
func (s LegacySlot) Command() string { return s.base + "/" + LegacySetSuffix }

// Attributes is "<base>/attributes".
func (s LegacySlot) Attributes() string { return s.base + "/" + legacyAttributesSfx }

// IsLegacyLeaf reports whether s is one of the three suffixes a 0.13 slot
// topic ended in.
func IsLegacyLeaf(s string) bool {
	return s == LegacyStateSuffix || s == LegacySetSuffix || s == legacyAttributesSfx
}
