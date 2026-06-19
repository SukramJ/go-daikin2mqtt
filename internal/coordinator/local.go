// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// localOnlyTopics are catalog topics the local Faikin interface provides but
// that the ONECTA cloud does not expose for some units (econo/streamer/outdoor
// silent/demand on the FTXA range, read off the serial bus locally). In local
// mode the daemon synthesizes discovery points for them so the entities appear
// in Home Assistant even though the cloud poll cannot resolve them.
var localOnlyTopics = []string{"econo_mode", "streamer", "outdoor_silent", "demand_control"}

// faikinToDaikinMode is the inverse of [daikinToFaikinMode]: it maps a Faikin
// app mode back to the ONECTA operationMode code, so a local state update can
// reuse the catalog's localized select label.
var faikinToDaikinMode = map[string]string{
	"cool": "cooling",
	"heat": "heating",
	"auto": "auto",
	"dry":  "dry",
	"fan":  "fanOnly",
}

// localTopics is the set of per-unit state topics the local Faikin path
// publishes. In local mode the cloud poll skips these for mapped devices to
// avoid redundant publishes (the synthetic hvac_mode is handled separately).
var localTopics = map[string]bool{
	"power":                true,
	"operation_mode":       true,
	"room_temperature":     true,
	"room_humidity":        true,
	"outdoor_temperature":  true,
	"temperature_setpoint": true,
	"powerful_mode":        true,
	"econo_mode":           true,
	"streamer":             true,
	"outdoor_silent":       true,
	"demand_control":       true,
}

// localActiveFor reports whether reads/writes for a device run through the
// local Faikin interface.
func (c *Coordinator) localActiveFor(deviceID string) bool {
	_, ok := c.localHost(deviceID)
	return ok
}

// localOnlyPoints synthesizes discovery points for the local-only topics of
// every mapped device, skipping any the cloud already resolved (so cloud-backed
// units are unaffected). These points seed HA discovery only — their live state
// comes from the Faikin read path, and the cloud poll skips them via
// [localTopics]. resolved is the current cloud-resolved point set.
func (c *Coordinator) localOnlyPoints(devices []model.Device, resolved []process.Point) []process.Point {
	have := make(map[string]bool, len(resolved))
	for i := range resolved {
		have[resolved[i].DeviceID+"|"+resolved[i].Topic] = true
	}
	var out []process.Point
	for _, d := range devices {
		if _, ok := c.localHost(d.ID); !ok {
			continue
		}
		emb, ok := c.climateEmbeddedID(d.ID)
		if !ok {
			continue
		}
		for _, topic := range localOnlyTopics {
			if have[d.ID+"|"+topic] {
				continue
			}
			entry, ok := c.deps.Catalog.ByTopic(topic)
			if !ok {
				continue
			}
			p := process.Point{
				DeviceID:   d.ID,
				EmbeddedID: emb,
				MPType:     "climateControl",
				Topic:      topic,
				Entry:      *entry,
				Settable:   entry.Settable,
				Unit:       entry.Unit,
			}
			// demand_control is a number; give HA sensible bounds (Faikin
			// reports a 0..100 % limit, settable in 5 % steps from 40 %).
			if entry.Platform == "number" {
				mn, mx, st := 40.0, 100.0, 5.0
				p.Min, p.Max, p.Step = &mn, &mx, &st
			}
			out = append(out, p)
		}
	}
	return out
}

// subscribeLocal subscribes to the `state/<host>` topic of every mapped Faikin
// module and republishes each update to the daemon's per-unit state topics.
// No-op when local mode is off or no Faikin connection is configured.
func (c *Coordinator) subscribeLocal(ctx context.Context) {
	if c.deps.FaikinMQTT == nil || !c.deps.Cfg.LocalEnabled() {
		return
	}
	for deviceID, host := range c.deps.Cfg.LocalDeviceMap {
		// The S21 /status topic carries the AC state reliably on every poll; the
		// app-level state topic is sparse and heartbeat-heavy but parsed too for
		// robustness across firmware variants. Both feed publishLocalState,
		// which ignores non-AC (OS heartbeat) messages.
		c.subscribeFaikin(ctx, deviceID, host, faikin.StatusTopic(host), faikin.ParseStatus)
		c.subscribeFaikin(ctx, deviceID, host, faikin.StateTopic(host), faikin.ParseState)
		c.deps.Logger.Info("coordinator.local_subscribed",
			slog.String("device", deviceID), slog.String("host", host))
	}
}

// subscribeFaikin subscribes to one Faikin topic, parsing each message with the
// given parser and republishing the AC state.
func (c *Coordinator) subscribeFaikin(ctx context.Context, deviceID, host, topic string,
	parse func(host string, payload []byte) (*faikin.State, error),
) {
	err := c.deps.FaikinMQTT.Subscribe(ctx, topic, mqtt.QoS0, func(_ string, payload []byte) {
		st, err := parse(host, payload)
		if err != nil {
			c.deps.Logger.Warn("coordinator.local_parse_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
			return
		}
		c.publishLocalState(ctx, deviceID, st)
	})
	if err != nil {
		c.deps.Logger.Warn("coordinator.local_subscribe_failed",
			slog.String("topic", topic), slog.String("err", err.Error()))
	}
}

// publishLocalState renders a Faikin state document onto the daemon's per-unit
// state topics. The embeddedID is taken from the cloud-derived cache, so a
// device must have been seen by at least one cloud poll (which also publishes
// HA discovery) before its local state can be routed.
func (c *Coordinator) publishLocalState(ctx context.Context, deviceID string, st *faikin.State) {
	// Faikin interleaves OS/heartbeat documents (no AC fields) on state/<host>;
	// processing them would reset every entity to its zero value, so skip them.
	if !st.HasAC {
		return
	}
	emb, ok := c.climateEmbeddedID(deviceID)
	if !ok {
		c.deps.Logger.Debug("coordinator.local_no_embedded_id",
			slog.String("device", deviceID), slog.String("hint", "awaiting first cloud poll"))
		return
	}
	for suffix, payload := range c.localStateMessages(st) {
		topic := fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, deviceID, emb, suffix)
		if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
			c.deps.Logger.Warn("coordinator.local_publish_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
		}
	}
}

// localStateMessages maps a Faikin state to {topic-suffix: payload}, matching
// the cloud path's topics and value formats so HA sees identical entities
// whichever backend is active.
func (c *Coordinator) localStateMessages(st *faikin.State) map[string]string {
	out := map[string]string{
		"power":                onOff(st.Power),
		hass.HVACModeTopic:     st.HAMode(),
		"room_temperature":     c.fmtFloat("room_temperature", st.Temp),
		"room_humidity":        c.fmtFloat("room_humidity", st.Hum),
		"outdoor_temperature":  c.fmtFloat("outdoor_temperature", st.Outside),
		"temperature_setpoint": c.fmtFloat("temperature_setpoint", st.Target),
		"powerful_mode":        onOff(st.Powerful),
		"econo_mode":           onOff(st.Econo),
		"streamer":             onOff(st.Streamer),
		"outdoor_silent":       onOff(st.Quiet),
		"demand_control":       strconv.Itoa(st.Demand),
	}
	if label, ok := c.localOperationModeLabel(st.Mode); ok {
		out["operation_mode"] = label
	}
	return out
}

// localOperationModeLabel maps a Faikin mode to the localized select label the
// cloud path publishes for the operation_mode entity (e.g. "cool" → "Kühlen").
func (c *Coordinator) localOperationModeLabel(faikinMode string) (string, bool) {
	dk, ok := faikinToDaikinMode[faikinMode]
	if !ok {
		return "", false
	}
	if e, ok := c.deps.Catalog.ByTopic("operation_mode"); ok {
		return e.LocalizedLabel(dk, c.deps.Cfg.Language), true
	}
	return dk, true
}

// fmtFloat formats v with the precision the catalog entry for topic declares,
// so local values render exactly like the cloud path's.
func (c *Coordinator) fmtFloat(topic string, v float64) string {
	prec := 1
	if e, ok := c.deps.Catalog.ByTopic(topic); ok {
		prec = e.Precision
	}
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// climateEmbeddedID returns the cached climateControl embeddedID for a device.
func (c *Coordinator) climateEmbeddedID(deviceID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.climateEmbedded[deviceID]
	return id, ok
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
