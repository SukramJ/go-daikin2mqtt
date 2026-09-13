// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package layout composes every MQTT topic this bridge publishes to or
// subscribes to, so the string exists in exactly one place.
//
// # Why this package exists
//
// The state topic — "<root>/<deviceID>/<embeddedID>/<topic>/state" — used to be
// composed in twelve separate fmt.Sprintf expressions across four packages,
// and nothing compared them:
//
//	internal/hass/discovery.go:278       Discovery.StateTopic — what goes INTO the retained config
//	internal/hass/climate.go:190         auxBase, the composite climate's five synthetic slots
//	internal/hass/schedule.go:45         Discovery.ScheduleStateTopic
//	internal/coordinator/coordinator.go:261  the per-point cloud publish
//	internal/coordinator/coordinator.go:324  the synthetic hvac_mode
//	internal/coordinator/climate.go:305      the synthetic fan/swing/preset
//	internal/coordinator/local.go:272        the Faikin read path, per unit
//	internal/coordinator/local.go:324        the Faikin read path, outdoor aggregate
//	internal/coordinator/local.go:342        the optimistic write-through
//	internal/coordinator/schedule.go:241     schedule_state / schedule_next_change
//	internal/coordinator/schedule.go:257     the outdoor schedule pair
//	internal/coordinator/schedule.go:288     the per-schedule enable switch
//
// plus the attributes plane (discovery.go:273, climate.go:112), the command
// plane (discovery.go:283, schedule.go:52, climate.go's auxBase, and the
// coordinator's subscribe filter) and the bridge status topic, which
// cmd/daikin2mqtt/main.go composed for the Last Will while
// Discovery.BridgeStatusTopic composed it for all 264 discovery payloads.
//
// The reason the count is this high is structural, not accidental: three
// independent writers (the cloud poll, the Faikin read path, the weekly
// scheduler) share one topic tree, and the composite climate entity's five
// synthetic slots are backed by no catalogue entry and composed inline in two
// of them.
//
// They agreed. Nothing enforced it. A drift in any one of them leaves entities
// pointing at a topic nobody writes: permanently `unknown` in Home Assistant,
// nothing in the log, nothing in the registry to notice. That is F3 of the
// ADR 0070 phase 8 measurement, and TestStateTopicBuildersAgree in
// internal/coordinator is the pin that would now catch it.
//
// # Scope
//
// Everything here is a pure function of a root and the path segments. Nothing
// is slugified or sanitized: the identity-bearing discovery *config* topic
// (which embeds a unique_id) deliberately stays in internal/hass, because it is
// an identity question rather than a layout one.
//
// The package is named layout rather than topic because `topic` is the obvious
// name for a local string in almost every publish signature in this repository,
// and gocritic's importShadow flags the collision.
//
// At ADR 0070 phase 8 step 4 this package is what a go-hamqtt topic.Layout will
// be written against.
package layout

// SchedulerDeviceID is the reserved device-id segment the weekly scheduler's
// own entities live under. It is not a Daikin device: the schedule enable
// switches belong to the daemon.
//
// It was declared three times before F3 — here, in internal/hass and in
// internal/schedule — and the one in internal/hass was paired with a bare
// "enabled" literal while the coordinator used a named constant for the same
// segment. Both halves now come from here.
const SchedulerDeviceID = "scheduler"

// EnabledTopic is the leaf segment of a schedule's enable switch, so that
// "<root>/scheduler/<scheduleID>/enabled/state" needs no special case: it is
// an ordinary four-segment slot whose device is the scheduler and whose
// embedded id is the schedule.
const EnabledTopic = "enabled"

// ClimateTopic is the leaf segment the composite climate entity's attributes
// document hangs off. The entity itself has no state topic of its own — it
// reads the topics of the points it composes.
const ClimateTopic = "climate"

// BridgeStatusDevice / BridgeStatusLeaf are the two segments of the bridge's
// availability topic, "<root>/bridge/status". Named so the one topic every
// discovery payload in this bridge points at is greppable.
const (
	BridgeStatusDevice = "bridge"
	BridgeStatusLeaf   = "status"
)

// Root is the MQTT topic root this daemon owns (MQTT_TOPIC, default "daikin").
// Every topic below is derived from it, so changing the root moves the whole
// state plane and orphans nothing in Home Assistant's registries — the identity
// strings are built in internal/hass and do not contain it.
type Root struct{ root string }

// New returns the layout rooted at root.
func New(root string) Root { return Root{root: root} }

// String returns the root itself.
func (r Root) String() string { return r.root }

// BridgeStatus is the bridge's availability topic: the retained "online" the
// daemon publishes on connect, the "offline" the broker publishes as the Last
// Will, and the availability_topic named by every discovery payload.
//
// Before F3 the daemon's will and the discovery payloads composed this
// independently, in different packages. A drift between them is the failure
// mtec2mqtt shipped: every entity naming an availability topic nothing writes
// is permanently unavailable, silently.
func (r Root) BridgeStatus() string {
	return r.root + "/" + BridgeStatusDevice + "/" + BridgeStatusLeaf
}

// CommandFilter is the single wildcard subscription that covers every command
// topic this bridge advertises: "<root>/+/+/+/set". Its four levels are why
// Slot has exactly the shape it has.
func (r Root) CommandFilter() string { return r.root + "/+/+/+/set" }

// Slot addresses one entity's topic family: the device, the management point
// and the leaf name. The leaf is a catalogue `topic:` for a real characteristic
// and a synthetic name (hvac_mode, fan_mode, schedule_state, …) for a point the
// coordinator makes up; the layout does not care which.
func (r Root) Slot(deviceID, embeddedID, topic string) Slot {
	return Slot{base: r.root + "/" + deviceID + "/" + embeddedID + "/" + topic}
}

// Schedule addresses the enable switch of one weekly schedule, which lives on
// the daemon's own scheduler device.
func (r Root) Schedule(scheduleID string) Slot {
	return r.Slot(SchedulerDeviceID, scheduleID, EnabledTopic)
}

// Climate addresses the composite climate entity's own topic family (only its
// attributes sibling is written; the entity reads the slots it composes).
func (r Root) Climate(deviceID, embeddedID string) Slot {
	return r.Slot(deviceID, embeddedID, ClimateTopic)
}

// Slot is one entity's topic family. Its three suffixes are the whole
// vocabulary: a value the daemon publishes, a command Home Assistant sends, and
// a JSON-attributes sibling.
type Slot struct{ base string }

// Base is the slot without a suffix, "<root>/<device>/<embedded>/<topic>".
func (s Slot) Base() string { return s.base }

// State is the retained topic the daemon publishes the slot's value to, and the
// topic a discovery payload names as its state_topic.
func (s Slot) State() string { return s.base + "/state" }

// Command is the topic Home Assistant writes to, matched by [Root.CommandFilter].
func (s Slot) Command() string { return s.base + "/set" }

// Attributes is the slot's JSON-attributes sibling, carrying the data_source
// document (cloud vs local Faikin).
func (s Slot) Attributes() string { return s.base + "/attributes" }
