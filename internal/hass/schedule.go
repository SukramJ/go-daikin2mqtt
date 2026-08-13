// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SukramJ/go-mqtt"
)

// SchedulerDeviceID is the reserved device id the schedule switches live
// under, mirroring schedule.SchedulerDeviceID. It is duplicated rather than
// imported so this package keeps depending only on process/mqtt.
const SchedulerDeviceID = "scheduler"

// schedulerIdentifier is the HA device the schedule switches are grouped
// under. It is the daemon's own device, not a Daikin one.
const schedulerIdentifier = "daikin_scheduler"

// ScheduleInfo is one schedule as Home Assistant needs to see it. Name is the
// operator's own text and is published verbatim in every language — a schedule
// is user data, so there is nothing to localize.
type ScheduleInfo struct {
	ID   string
	Name string
}

// schedulerDevice builds the HA device block for the daemon's scheduler.
// configURL points at the web UI when known, so the device page links to the
// calendar that owns these switches.
func schedulerDevice(configURL string) device {
	return device{
		Identifiers:      []string{schedulerIdentifier},
		Name:             "daikin2mqtt Scheduler",
		Manufacturer:     "go-daikin2mqtt",
		ConfigurationURL: configURL,
	}
}

// ScheduleStateTopic returns the retained enable-state topic of a schedule.
func (d *Discovery) ScheduleStateTopic(scheduleID string) string {
	return fmt.Sprintf("%s/%s/%s/enabled/state", d.stateRoot, SchedulerDeviceID, scheduleID)
}

// ScheduleCommandTopic returns the enable command topic of a schedule. It fits
// the coordinator's existing <root>/+/+/+/set subscription, so toggling a
// schedule from Home Assistant needs no extra subscription.
func (d *Discovery) ScheduleCommandTopic(scheduleID string) string {
	return fmt.Sprintf("%s/%s/%s/enabled/set", d.stateRoot, SchedulerDeviceID, scheduleID)
}

// PublishSchedules emits a retained switch config per schedule and returns the
// set of config topics it published, so the caller can fold them into the
// orphan reconcile — a deleted schedule's config is then cleared like any
// other entity that stopped being published.
func (d *Discovery) PublishSchedules(ctx context.Context, schedules []ScheduleInfo, configURL string) (map[string]bool, error) {
	published := map[string]bool{}
	var firstErr error
	dev := schedulerDevice(configURL)

	for _, s := range schedules {
		topic, payload, ok := d.buildScheduleConfig(s, dev)
		if !ok {
			continue
		}
		published[topic] = true
		if err := d.pub.Publish(ctx, topic, payload, mqtt.QoS0, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return published, firstErr
}

// buildScheduleConfig renders one schedule's switch config.
//
// The entity id is seeded from the schedule's slug, not from its display name:
// the slug is frozen at creation, so renaming a schedule leaves
// switch.daikin_schedule_<slug> untouched. HA never renames a registered
// entity, so a slug that tracked the name would strand the old one.
func (d *Discovery) buildScheduleConfig(s ScheduleInfo, dev device) (topic string, payload []byte, ok bool) {
	if s.ID == "" {
		return "", nil, false
	}
	uid := sanitize("daikin_schedule_" + s.ID)
	cfg := configPayload{
		Name:                s.Name,
		ObjectID:            uid,
		DefaultEntityID:     "switch." + uid,
		UniqueID:            uid,
		Icon:                "mdi:calendar-check",
		EntityCategory:      "config",
		StateTopic:          d.ScheduleStateTopic(s.ID),
		CommandTopic:        d.ScheduleCommandTopic(s.ID),
		PayloadOn:           "on",
		PayloadOff:          "off",
		StateOn:             "on",
		StateOff:            "off",
		AvailabilityTopic:   d.BridgeStatusTopic(),
		PayloadAvailable:    "online",
		PayloadNotAvailable: "offline",
		Device:              dev,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, false
	}
	return fmt.Sprintf("%s/switch/%s/config", d.baseTopic, uid), b, true
}
