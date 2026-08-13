// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// Topics of the scheduler's own entities. They are language-independent like
// every other topic; only the display names are localized (via the catalog).
const (
	// ScheduleStateTopic carries the block in force for a device, as free text
	// ("<schedule> · <block>") or the localized idle label.
	ScheduleStateTopic = "schedule_state"
	// ScheduleNextTopic carries the next switch point as RFC 3339, so Home
	// Assistant renders it in the viewer's language rather than the daemon's.
	ScheduleNextTopic = "schedule_next_change"
	// OutdoorScheduleStateTopic / OutdoorScheduleNextTopic are the same two
	// sensors for an outdoor schedule. They are separate topics because their
	// catalog entries are scope: outdoor, which collapses them into one pair on
	// the outdoor unit instead of one per indoor unit.
	OutdoorScheduleStateTopic = "outdoor_schedule_state"
	OutdoorScheduleNextTopic  = "outdoor_schedule_next_change"
	// ScheduleEnabledTopic is the per-schedule enable switch, published under
	// the reserved device id "scheduler".
	ScheduleEnabledTopic = "enabled"
	// scheduleIdleValue is the catalog enum value used when no block applies.
	scheduleIdleValue = "idle"
)

// scheduler is the slice of the schedule engine the coordinator needs. It is
// an interface so the coordinator stays testable with a stub and so main can
// resolve the construction cycle (the engine needs the coordinator as its
// applier, the coordinator needs the engine for the enable switch).
type scheduler interface {
	SetEnabled(id string, on bool) error
	Document() *schedule.Document
	Wake()
}

// AttachScheduler wires the schedule engine into the coordinator. Call it
// before Run; a nil engine leaves the scheduler disabled.
func (c *Coordinator) AttachScheduler(s scheduler) {
	c.mu.Lock()
	c.schedule = s
	c.mu.Unlock()
}

// scheduleEngine returns the attached engine, or nil.
func (c *Coordinator) scheduleEngine() scheduler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.schedule
}

// ApplySchedule applies a block's target state. It implements
// schedule.Applier by producing the same write requests an inbound MQTT /set
// produces, so the scheduled write inherits the whole existing path:
// sequential drain, cloud lock, multi-split mode sync, the outdoor fan-out and
// the backend choice between the local Faikin interface and the cloud.
func (c *Coordinator) ApplySchedule(_ context.Context, target schedule.Target, a schedule.Action) error {
	if target.IsOutdoor() {
		return c.applyOutdoorSchedule(target.OutdoorSerial, a)
	}
	return c.applyIndoorSchedule(target, a)
}

// applyIndoorSchedule writes power, mode and setpoint to one indoor unit.
//
// The order is not arbitrary: the setpoint's PATCH path contains {mode}, which
// handleWrite resolves from modeCache. Writing the mode first means noteWrite
// has updated that cache by the time the setpoint request is drained.
func (c *Coordinator) applyIndoorSchedule(target schedule.Target, a schedule.Action) error {
	deviceID := target.DeviceID
	emb := target.EmbeddedID
	if emb == "" {
		var ok bool
		emb, ok = c.climateEmbeddedID(deviceID)
		if !ok {
			// No poll has resolved this device yet, so neither its management
			// point nor its current mode is known. Not an error: the poll wakes
			// the engine, and the catch-up window covers the delay.
			return schedule.ErrDeviceUnknown
		}
	}

	if !c.enqueueWrite(writeReq{
		deviceID:   deviceID,
		embeddedID: emb,
		topic:      hass.HVACModeTopic,
		payload:    a.HVACPayload(),
	}) {
		return fmt.Errorf("schedule: write queue full for %s", deviceID)
	}

	// An "off" block leaves mode and temperature alone; a setpoint is only
	// meaningful for a running unit.
	if a.Power == schedule.PowerOn && a.Setpoint != nil {
		if !c.enqueueWrite(writeReq{
			deviceID:   deviceID,
			embeddedID: emb,
			topic:      setpointTopic,
			payload:    a.SetpointPayload(),
		}) {
			return fmt.Errorf("schedule: write queue full for %s", deviceID)
		}
	}
	return nil
}

// applyOutdoorSchedule writes the outdoor-shared settings of one outdoor unit.
//
// These are `scope: outdoor` catalog topics, so handleWrite already fans each
// write out to every indoor unit of the group and holds the optimistic value
// until a status confirms it. That is why this addresses a single member
// rather than looping itself: doing both would write each setting twice.
func (c *Coordinator) applyOutdoorSchedule(serial string, a schedule.Action) error {
	member, emb, ok := c.outdoorGroupMember(serial)
	if !ok {
		// The outdoor group is only known after a poll has resolved the
		// devices' serials — same "not yet" as an unknown device.
		return schedule.ErrDeviceUnknown
	}

	writes := []struct {
		topic   string
		payload string
		set     bool
	}{
		{outdoorSilentTopic, onOff(boolValue(a.OutdoorSilent)), a.OutdoorSilent != nil},
		{econoTopic, onOff(boolValue(a.Econo)), a.Econo != nil},
		{demandTopic, a.DemandPayload(), a.Demand != nil},
	}
	for _, w := range writes {
		if !w.set {
			continue // "leave this alone"
		}
		if !c.enqueueWrite(writeReq{
			deviceID:   member,
			embeddedID: emb,
			topic:      w.topic,
			payload:    w.payload,
		}) {
			return fmt.Errorf("schedule: write queue full for outdoor unit %s", serial)
		}
	}
	return nil
}

// outdoorGroupMember picks the indoor unit an outdoor-scoped write is
// addressed to: the first member (sorted, so the choice is stable across
// polls) whose management point is known. Which member wins does not matter —
// the fan-out reaches all of them.
func (c *Coordinator) outdoorGroupMember(serial string) (deviceID, embeddedID string, ok bool) {
	for _, dev := range c.OutdoorGroups()[serial] {
		if emb, known := c.climateEmbeddedID(dev); known {
			return dev, emb, true
		}
	}
	return "", "", false
}

// Catalog topics of the settings that act on the shared outdoor unit.
const (
	setpointTopic      = "temperature_setpoint"
	outdoorSilentTopic = "outdoor_silent"
	econoTopic         = "econo_mode"
	demandTopic        = "demand_control"
)

// boolValue dereferences an optional bool, treating nil as false. Callers
// check for nil separately; this only keeps the write table readable.
func boolValue(v *bool) bool { return v != nil && *v }

// enqueueWrite queues a write request, reporting whether it was accepted.
func (c *Coordinator) enqueueWrite(req writeReq) bool {
	select {
	case c.writes <- req:
		return true
	default:
		c.deps.Logger.Warn("coordinator.write_queue_full",
			slog.String("device", req.deviceID), slog.String("topic", req.topic))
		return false
	}
}

// handleSchedulerWrite applies a command addressed to the reserved "scheduler"
// device — currently only the per-schedule enable switch. The topic fits the
// existing <root>/+/+/+/set filter, so no second subscription is needed.
func (c *Coordinator) handleSchedulerWrite(req writeReq) {
	eng := c.scheduleEngine()
	if eng == nil {
		c.deps.Logger.Warn("coordinator.schedule_disabled", slog.String("topic", req.topic))
		return
	}
	if req.topic != ScheduleEnabledTopic {
		c.deps.Logger.Warn("coordinator.schedule_unknown_topic", slog.String("topic", req.topic))
		return
	}
	if err := eng.SetEnabled(req.embeddedID, truthy(req.payload)); err != nil {
		c.deps.Logger.Warn("coordinator.schedule_toggle_failed",
			slog.String("schedule", req.embeddedID), slog.String("err", err.Error()))
		return
	}
	c.deps.Logger.Info("coordinator.schedule_toggled",
		slog.String("schedule", req.embeddedID), slog.String("enabled", req.payload))
}

// PublishScheduleState publishes the two status sensors of one target. It
// reports intent — what the schedule says should be in force — not a device
// reading.
func (c *Coordinator) PublishScheduleState(ctx context.Context, target schedule.Target, st schedule.DeviceState) {
	state := c.scheduleStateLabel(st)
	next := ""
	if !st.NextChange.IsZero() {
		next = st.NextChange.Format(time.RFC3339)
	}

	if target.IsOutdoor() {
		c.publishOutdoorScheduleState(ctx, target.OutdoorSerial, state, next)
		return
	}

	emb, ok := c.climateEmbeddedID(target.DeviceID)
	if !ok {
		return // device not resolved yet; the next poll will publish it
	}
	c.publishRetained(ctx, fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, target.DeviceID, emb, ScheduleStateTopic), state)
	c.publishRetained(ctx, fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, target.DeviceID, emb, ScheduleNextTopic), next)
}

// publishOutdoorScheduleState publishes an outdoor schedule's status on every
// member of the group. The entities are `scope: outdoor`, so discovery
// collapses them into one pair on the outdoor unit — but which member's topic
// that entity reads from depends on the discovery order, so every member has
// to carry the value. This mirrors how the outdoor telemetry is published.
func (c *Coordinator) publishOutdoorScheduleState(ctx context.Context, serial, state, next string) {
	for _, member := range c.OutdoorGroups()[serial] {
		emb, ok := c.climateEmbeddedID(member)
		if !ok {
			continue
		}
		c.publishRetained(ctx,
			fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, member, emb, OutdoorScheduleStateTopic), state)
		c.publishRetained(ctx,
			fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, member, emb, OutdoorScheduleNextTopic), next)
	}
}

// scheduleStateLabel renders the active-block sensor value. The schedule and
// block names are the operator's own words and are published verbatim; only
// the idle state is a string the daemon produces, and it comes from the
// catalog entry's values so it follows LANGUAGE like every other label.
func (c *Coordinator) scheduleStateLabel(st schedule.DeviceState) string {
	if st.Active == nil {
		if entry, ok := c.deps.Catalog.ByTopic(ScheduleStateTopic); ok {
			return entry.LocalizedLabel(scheduleIdleValue, c.deps.Cfg.Language)
		}
		return scheduleIdleValue
	}
	if st.Active.Label == "" {
		return st.Active.ScheduleName
	}
	return st.Active.ScheduleName + " · " + st.Active.Label
}

// PublishScheduleSwitches publishes the retained state of every schedule's
// enable switch, so Home Assistant reflects a change made in the web UI.
func (c *Coordinator) PublishScheduleSwitches(ctx context.Context, doc *schedule.Document) {
	if doc == nil {
		return
	}
	for i := range doc.Schedules {
		s := &doc.Schedules[i]
		topic := fmt.Sprintf("%s/%s/%s/%s/state",
			c.topicRoot, schedule.SchedulerDeviceID, s.ID, ScheduleEnabledTopic)
		c.publishRetained(ctx, topic, onOff(s.Enabled))
	}
}

// publishRetained publishes a retained QoS0 payload, logging a failure.
func (c *Coordinator) publishRetained(ctx context.Context, topic, payload string) {
	if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
		c.deps.Logger.Warn("coordinator.publish_failed",
			slog.String("topic", topic), slog.String("err", err.Error()))
	}
}

// schedulePoints synthesizes the discovery points for the two per-device
// status sensors, one pair per device. Like the refresh button they are daemon
// state rather than device characteristics: their catalog entries match a
// characteristic the cloud never reports, so they only ever reach Home
// Assistant through here.
func (c *Coordinator) schedulePoints(devices []model.Device) []process.Point {
	if c.scheduleEngine() == nil {
		return nil
	}
	// The outdoor pair is only published when an outdoor schedule exists —
	// otherwise every installation would grow two empty sensors on the outdoor
	// unit. The indoor pair is unconditional: an indoor schedule can be added
	// at any time, and the sensor then reports "no block" rather than nothing.
	topics := []string{ScheduleStateTopic, ScheduleNextTopic}
	if c.hasOutdoorSchedule() {
		topics = append(topics, OutdoorScheduleStateTopic, OutdoorScheduleNextTopic)
	}

	out := make([]process.Point, 0, len(topics)*len(devices))
	for _, d := range devices {
		emb, ok := c.climateEmbeddedID(d.ID)
		if !ok {
			continue
		}
		for _, topic := range topics {
			entry, ok := c.deps.Catalog.ByTopic(topic)
			if !ok {
				continue
			}
			out = append(out, process.Point{
				DeviceID:   d.ID,
				EmbeddedID: emb,
				MPType:     "climateControl",
				Topic:      topic,
				Entry:      *entry,
			})
		}
	}
	return out
}

// hasOutdoorSchedule reports whether any schedule drives an outdoor unit.
func (c *Coordinator) hasOutdoorSchedule() bool {
	eng := c.scheduleEngine()
	if eng == nil {
		return false
	}
	doc := eng.Document()
	for i := range doc.Schedules {
		if doc.Schedules[i].Kind() == schedule.TypeOutdoor {
			return true
		}
	}
	return false
}

// OutdoorGroups returns the outdoor-unit groups as serial → member device ids.
// The scheduler's conflict check needs to know who shares a compressor, and
// only the coordinator learns that from the poll. Devices with no known
// outdoor serial are omitted: a single-member group cannot conflict.
func (c *Coordinator) OutdoorGroups() map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	for dev, serial := range c.outdoorSerial {
		if serial == "" {
			continue
		}
		out[serial] = append(out[serial], dev)
	}
	for serial := range out {
		sort.Strings(out[serial])
	}
	return out
}

// publishScheduleDiscovery publishes the per-schedule switch configs and adds
// their topics to published, so the orphan reconcile treats them like every
// other entity this daemon owns.
func (c *Coordinator) publishScheduleDiscovery(ctx context.Context, published map[string]bool) error {
	eng := c.scheduleEngine()
	if eng == nil || c.deps.HASS == nil {
		return nil
	}
	doc := eng.Document()
	infos := make([]hass.ScheduleInfo, 0, len(doc.Schedules))
	for i := range doc.Schedules {
		s := &doc.Schedules[i]
		infos = append(infos, hass.ScheduleInfo{ID: s.ID, Name: s.Name})
	}
	topics, err := c.deps.HASS.PublishSchedules(ctx, infos, c.webConfigURL())
	for t := range topics {
		published[t] = true
	}
	return err
}

// webConfigURL returns the URL of the diagnostic web UI for the scheduler's HA
// device page, or "" when the UI is disabled. A wildcard bind has no single
// reachable address, so no link is offered rather than a wrong one.
func (c *Coordinator) webConfigURL() string {
	cfg := c.deps.Cfg
	if !cfg.WebEnable || cfg.WebBind == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(cfg.WebBind)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return ""
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// scheduleSignature contributes the scheduler's entity set to the discovery
// signature, so adding, renaming or deleting a schedule republishes discovery
// (and lets the orphan reconcile clear a deleted schedule's config).
func (c *Coordinator) scheduleSignature() string {
	eng := c.scheduleEngine()
	if eng == nil {
		return ""
	}
	doc := eng.Document()
	sig := "sched:"
	for i := range doc.Schedules {
		s := &doc.Schedules[i]
		sig += s.ID + "=" + s.Name + ";"
	}
	return sig
}
