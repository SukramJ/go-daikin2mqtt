// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
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

// ApplySchedule applies a block's target state to one device. It implements
// schedule.Applier by producing the same write requests an inbound MQTT /set
// produces, so the scheduled write inherits the whole existing path:
// sequential drain, cloud lock, multi-split mode sync, and the backend choice
// between the local Faikin interface and the cloud.
//
// The order is not arbitrary: the setpoint's PATCH path contains {mode}, which
// handleWrite resolves from modeCache. Writing the mode first means noteWrite
// has updated that cache by the time the setpoint request is drained.
func (c *Coordinator) ApplySchedule(_ context.Context, deviceID, embeddedID string, a schedule.Action) error {
	emb := embeddedID
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

// setpointTopic is the catalog topic of the room-temperature setpoint.
const setpointTopic = "temperature_setpoint"

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

// PublishScheduleState publishes the two per-device status sensors. It reports
// intent — what the schedule says should be in force — not a device reading.
func (c *Coordinator) PublishScheduleState(ctx context.Context, deviceID string, st schedule.DeviceState) {
	emb, ok := c.climateEmbeddedID(deviceID)
	if !ok {
		return // device not resolved yet; the next poll will publish it
	}

	state := c.scheduleStateLabel(st)
	c.publishRetained(ctx, fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, deviceID, emb, ScheduleStateTopic), state)

	next := ""
	if !st.NextChange.IsZero() {
		next = st.NextChange.Format(time.RFC3339)
	}
	c.publishRetained(ctx, fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, deviceID, emb, ScheduleNextTopic), next)
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
	out := make([]process.Point, 0, 2*len(devices))
	for _, d := range devices {
		emb, ok := c.climateEmbeddedID(d.ID)
		if !ok {
			continue
		}
		for _, topic := range []string{ScheduleStateTopic, ScheduleNextTopic} {
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
