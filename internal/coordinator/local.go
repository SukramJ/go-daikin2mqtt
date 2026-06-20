// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

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
var localOnlyTopics = []string{
	"econo_mode", "streamer", "outdoor_silent", "demand_control",
	// Telemetry the cloud does not expose, read straight off the Faikin state.
	"energy_total", "heating_energy_total", "cooling_energy_total",
	"power_consumption", "compressor_frequency", "fan_frequency",
	"refrigerant_temperature",
}

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

// applyFaikinConfigURLs points each locally-controlled device's HA configuration
// URL at its Faikin module web UI (from the module's reported IP), mirroring
// Faikin's own discovery. Cloud-only devices keep the default (ONECTA) link.
func (c *Coordinator) applyFaikinConfigURLs(infos map[string]hass.DeviceInfo) {
	if !c.deps.Cfg.LocalEnabled() {
		return
	}
	for id := range infos {
		if !c.localActiveFor(id) {
			continue
		}
		c.mu.Lock()
		st := c.lastLocal[id]
		c.mu.Unlock()
		if st == nil {
			continue
		}
		if u := st.WebURL(); u != "" {
			info := infos[id]
			info.ConfigurationURL = u
			infos[id] = info
		}
	}
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
		// `state/<host>` is the firmware's canonical state topic — the one its own
		// HA discovery points every entity at (app form: mode "cool", temp = room
		// temperature, target = setpoint). It is retained and published on change.
		// Non-AC (OS heartbeat) messages lacking `power` are ignored downstream.
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
	// Remember the latest AC state so it can be (re)published once the embeddedID
	// is known — the retained Faikin state often arrives at subscribe time,
	// before the first cloud poll has populated the embeddedID cache.
	c.mu.Lock()
	c.lastLocal[deviceID] = st
	// Hold each per-unit energy counter at its highest seen value, so an idle
	// unit reporting 0 does not drop the summed outdoor total.
	e := c.lastEnergy[deviceID]
	e.total = max(e.total, st.Energy)
	e.heat = max(e.heat, st.EnergyHeat)
	e.cool = max(e.cool, st.EnergyCool)
	c.lastEnergy[deviceID] = e
	c.mu.Unlock()

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
	c.publishOutdoorShared(ctx, deviceID)
}

// publishOutdoorShared publishes the group-aggregated outdoor-shared settings to
// every member of the device's outdoor unit. On a multi-split only the active
// indoor unit applies an outdoor-unit setting (outdoor silent, demand), so the
// aggregate is what's actually in effect: outdoor silent is on if ANY member
// reports it, demand is the most restrictive (lowest) value. Publishing to all
// members means the single HA entity (which reads one fixed member) reflects it.
func (c *Coordinator) publishOutdoorShared(ctx context.Context, deviceID string) {
	agg := c.localOutdoorAgg(deviceID)
	group := c.groupKey(deviceID)
	vals := map[string]string{
		"outdoor_silent":       c.heldOutdoorValue(group, "outdoor_silent", onOff(agg.quiet)),
		"demand_control":       c.heldOutdoorValue(group, "demand_control", strconv.Itoa(agg.demand)),
		"power_consumption":    strconv.Itoa(agg.powerW),
		"compressor_frequency": c.fmtFloat("compressor_frequency", agg.compHz),
	}
	// Energy is a lifetime total (total_increasing); never publish 0 — when no
	// member currently reports it (all idle), keep the retained last value
	// instead of resetting the counter.
	if agg.energyWh > 0 {
		vals["energy_total"] = c.fmtFloat("energy_total", float64(agg.energyWh)/1000)
	}
	if agg.energyHeatWh > 0 {
		vals["heating_energy_total"] = c.fmtFloat("heating_energy_total", float64(agg.energyHeatWh)/1000)
	}
	if agg.energyCoolWh > 0 {
		vals["cooling_energy_total"] = c.fmtFloat("cooling_energy_total", float64(agg.energyCoolWh)/1000)
	}
	for _, member := range c.groupMembers(deviceID) {
		emb, ok := c.climateEmbeddedID(member)
		if !ok {
			continue
		}
		for suffix, payload := range vals {
			topic := fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, member, emb, suffix)
			if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
				c.deps.Logger.Warn("coordinator.local_publish_failed",
					slog.String("topic", topic), slog.String("err", err.Error()))
			}
		}
	}
}

// publishOptimistic immediately reflects a just-written value on every member of
// the device's outdoor unit, so the single HA entity updates at once instead of
// snapping back while waiting for the sparse Faikin status.
func (c *Coordinator) publishOptimistic(ctx context.Context, deviceID, topic, value string) {
	for _, member := range c.groupMembers(deviceID) {
		emb, ok := c.climateEmbeddedID(member)
		if !ok {
			continue
		}
		t := fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, member, emb, topic)
		if err := c.deps.MQTT.Publish(ctx, t, []byte(value), mqtt.QoS0, true); err != nil {
			c.deps.Logger.Warn("coordinator.local_publish_failed",
				slog.String("topic", t), slog.String("err", err.Error()))
		}
	}
}

// outdoorHoldDuration is how long a just-written outdoor-shared value is held
// before a status report may revert it (the active indoor unit reports the
// change only on its next, sparse status publish).
const outdoorHoldDuration = 2 * time.Minute

// groupKey returns the cache key for a device's outdoor group: its outdoor
// serial, or the device id when it has none.
func (c *Coordinator) groupKey(deviceID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.outdoorSerial[deviceID]; s != "" {
		return s
	}
	return deviceID
}

// holdOutdoor records a just-written outdoor-shared value so heldOutdoorValue
// keeps returning it until a status confirms it (or the hold expires).
func (c *Coordinator) holdOutdoor(deviceID, suffix, value string) {
	group := c.groupKey(deviceID)
	c.mu.Lock()
	c.pendingOutdoor[group+"|"+suffix] = outdoorHold{value: value, until: c.deps.Clock().Add(outdoorHoldDuration)}
	c.mu.Unlock()
}

// heldOutdoorValue returns the value to publish for an outdoor-shared topic: the
// held (just-written) value while a write is pending and unconfirmed, otherwise
// the raw aggregate. The hold clears once the aggregate matches it or it expires.
func (c *Coordinator) heldOutdoorValue(group, suffix, raw string) string {
	key := group + "|" + suffix
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.pendingOutdoor[key]
	if !ok {
		return raw
	}
	if raw == h.value || c.deps.Clock().After(h.until) {
		delete(c.pendingOutdoor, key) // confirmed or expired
		return raw
	}
	return h.value
}

// localOutdoorAgg aggregates the outdoor-shared values across a device's outdoor
// group from the cached Faikin states: outdoor silent is on if any member is on;
// demand is the lowest (most restrictive) reported value.
// outdoorAggregate holds the outdoor-unit values combined across a multi-split
// group. Settings (quiet/demand) and telemetry (power/compressor/energy) are all
// reported per indoor unit but describe the one outdoor unit.
type outdoorAggregate struct {
	quiet                                bool
	demand                               int
	powerW                               int
	compHz                               float64
	energyWh, energyHeatWh, energyCoolWh int64
}

// localOutdoorAgg combines the outdoor values across a device's outdoor group
// from the cached Faikin states. The aggregation rule differs by physics:
//   - Settings: outdoor silent is on if any member reports it; demand is the
//     most restrictive (lowest).
//   - Power and energy are reported per indoor unit, so the outdoor (system)
//     total is the SUM across members. Power is instantaneous (an idle unit
//     reports ~0, which is correct); energy uses the per-unit held value so an
//     idle unit reporting 0 does not drop the monotonic total. Energy uses
//     [aggregateEnergy], which falls back to a shared value when every member
//     reports the same counter (a single shared meter rather than per-unit).
//   - Compressor frequency is a single outdoor value every member reports
//     identically, so it is the MAX (any reporting member).
func (c *Coordinator) localOutdoorAgg(deviceID string) outdoorAggregate {
	members := c.groupMembers(deviceID)
	c.mu.Lock()
	defer c.mu.Unlock()
	agg := outdoorAggregate{demand: 100}
	var totals, heats, cools []int64
	for _, m := range members {
		st, ok := c.lastLocal[m]
		if !ok {
			continue
		}
		if st.Quiet {
			agg.quiet = true
		}
		if st.Demand < agg.demand {
			agg.demand = st.Demand
		}
		agg.powerW += st.Consumption
		agg.compHz = max(agg.compHz, st.Comp)
		e := c.lastEnergy[m]
		totals = append(totals, e.total)
		heats = append(heats, e.heat)
		cools = append(cools, e.cool)
	}
	agg.energyWh = aggregateEnergy(totals)
	agg.energyHeatWh = aggregateEnergy(heats)
	agg.energyCoolWh = aggregateEnergy(cools)
	return agg
}

// aggregateEnergy combines per-member lifetime energy counters into the outdoor
// total. Per-unit meters (the common case) carry different values and sum to the
// system total. But the Faikin `energy` field is the outside power meter, which
// on some hardware is a single shared counter every indoor unit reports
// identically — summing that would multiply it by the unit count. So when every
// reporting member shows the *same* value, it is treated as one shared counter
// and returned as-is. Zero (non-reporting) members are ignored.
func aggregateEnergy(vals []int64) int64 {
	var sum, shared int64
	seen, differ := false, false
	for _, v := range vals {
		if v == 0 {
			continue
		}
		sum += v
		switch {
		case !seen:
			shared, seen = v, true
		case v != shared:
			differ = true
		}
	}
	switch {
	case !seen:
		return 0
	case differ:
		return sum // per-unit meters → system total
	default:
		return shared // all members identical → one shared meter
	}
}

// flushLocalStates republishes the last AC state received for each mapped
// device. Called after a cloud poll has populated the embeddedID cache so the
// retained Faikin state (received at subscribe time, before any poll) and any
// state seen between the sparse cloud polls reaches Home Assistant.
func (c *Coordinator) flushLocalStates(ctx context.Context) {
	c.mu.Lock()
	pending := make(map[string]*faikin.State, len(c.lastLocal))
	for dev, st := range c.lastLocal {
		pending[dev] = st
	}
	c.mu.Unlock()
	for deviceID, st := range pending {
		c.publishLocalState(ctx, deviceID, st)
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
		// Per-indoor-unit telemetry (no cloud equivalent).
		"fan_frequency":           c.fmtFloat("fan_frequency", st.FanFreq),
		"refrigerant_temperature": c.fmtFloat("refrigerant_temperature", st.Liquid),
	}
	// outdoor_silent, demand_control and the outdoor-unit telemetry (power,
	// compressor, energy totals) are outdoor-shared (scope: outdoor) and
	// published group-aggregated by publishOutdoorShared, not per unit.
	if label, ok := c.localOperationModeLabel(st.Mode); ok {
		out["operation_mode"] = label
	}
	// Synthetic climate fan/swing topics (localized labels, like publishClimateAux),
	// translated from Faikin's vocabulary to the cloud values HA's lists use.
	lang := c.deps.Cfg.Language
	if cloud, ok := faikinFanToCloud[st.Fan]; ok {
		out[hass.FanModeTopic] = localizeAux(cloud, lang, fanModeDE)
	}
	v, h := faikinSwingAxes(st.Swing)
	out[hass.SwingModeTopic] = localizeAux(v, lang, swingModeDE)
	out[hass.SwingHModeTopic] = localizeAux(h, lang, swingModeDE)
	// Climate preset mirrors powerful (boost) from the local state, so the
	// preset stays in sync with the powerful switch instead of the stale cloud.
	preset := "none"
	if st.Powerful {
		preset = "boost"
	}
	out[hass.PresetModeTopic] = localizeAux(preset, lang, presetModeDE)
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
