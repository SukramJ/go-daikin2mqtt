// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
)

// climateAux holds the fan / swing / preset option lists and current values
// extracted from a climateControl management point's fanControl (and
// powerfulMode) for the active operation mode.
type climateAux struct {
	fanMode     string
	fanModes    []string
	swing       string
	swingModes  []string
	swingH      string
	swingHModes []string
	preset      string
	presetModes []string
}

// info converts the option lists into the discovery-facing [hass.ClimateInfo].
func (a climateAux) info() hass.ClimateInfo {
	return hass.ClimateInfo{
		FanModes:             a.fanModes,
		SwingModes:           a.swingModes,
		SwingHorizontalModes: a.swingHModes,
		PresetModes:          a.presetModes,
	}
}

// parseClimateAux extracts fan/swing/preset state and options for the active
// operation mode. mode is the current operationMode value.
func parseClimateAux(mp model.ManagementPoint, mode string) climateAux {
	var a climateAux

	if fc, ok := mp.Characteristics["fanControl"]; ok && mode != "" {
		modeObj, ok := jsonPath(fc.Value, "operationModes", mode)
		if ok {
			parseFanSpeed(modeObj, &a)
			parseFanDirection(modeObj, &a)
		}
	}

	// Presets: boost (powerfulMode). "none" is implicit in Home Assistant and
	// MUST NOT appear in preset_modes (HA rejects the whole climate config
	// otherwise), but it is still a valid current state. holidayMode (away) is
	// not yet settable through this bridge, so it is not advertised.
	if pm, ok := mp.Characteristics["powerfulMode"]; ok {
		a.presetModes = []string{"boost"}
		if s, _ := pm.String(); s == "on" {
			a.preset = "boost"
		} else {
			a.preset = "none"
		}
	}
	return a
}

// parseFanSpeed fills the fan-mode fields. "fixed" is expanded into the
// numeric speed range (e.g. 1..5).
func parseFanSpeed(modeObj json.RawMessage, a *climateAux) {
	fs, ok := jsonGet(modeObj, "fanSpeed")
	if !ok {
		return
	}
	cur, ok := jsonGet(fs, "currentMode")
	if !ok {
		return
	}
	curVal := jsonString(cur, "value")
	values := jsonStringSlice(cur, "values")

	var lo, hi float64 = 1, 5
	if fixed, ok := jsonPath(fs, "modes", "fixed"); ok {
		if v, ok := jsonFloat(fixed, "minValue"); ok {
			lo = v
		}
		if v, ok := jsonFloat(fixed, "maxValue"); ok {
			hi = v
		}
	}
	for _, v := range values {
		if v == "fixed" {
			for n := int(lo); n <= int(hi); n++ {
				a.fanModes = append(a.fanModes, strconv.Itoa(n))
			}
			continue
		}
		a.fanModes = append(a.fanModes, v)
	}

	if curVal == "fixed" {
		if fixed, ok := jsonPath(fs, "modes", "fixed"); ok {
			if v, ok := jsonFloat(fixed, "value"); ok {
				a.fanMode = strconv.Itoa(int(v))
			}
		}
	} else {
		a.fanMode = curVal
	}
}

// parseFanDirection fills the swing fields (lower-cased for HA).
func parseFanDirection(modeObj json.RawMessage, a *climateAux) {
	fd, ok := jsonGet(modeObj, "fanDirection")
	if !ok {
		return
	}
	if v, ok := jsonPath(fd, "vertical", "currentMode"); ok {
		a.swing = strings.ToLower(jsonString(v, "value"))
		for _, s := range jsonStringSlice(v, "values") {
			a.swingModes = append(a.swingModes, strings.ToLower(s))
		}
	}
	if h, ok := jsonPath(fd, "horizontal", "currentMode"); ok {
		a.swingH = strings.ToLower(jsonString(h, "value"))
		for _, s := range jsonStringSlice(h, "values") {
			a.swingHModes = append(a.swingHModes, strings.ToLower(s))
		}
	}
}

// climateInfos builds the discovery climate option lists keyed by
// deviceID|embeddedID.
func climateInfos(devices []model.Device) map[string]hass.ClimateInfo {
	out := map[string]hass.ClimateInfo{}
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			if mp.Type != "climateControl" {
				continue
			}
			a := parseClimateAux(mp, currentMode(mp))
			out[d.ID+"|"+mp.EmbeddedID] = a.info()
		}
	}
	return out
}

// publishClimateAux publishes the synthetic fan/swing/preset state topics.
func (c *Coordinator) publishClimateAux(ctx context.Context, devices []model.Device) {
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			if mp.Type != "climateControl" {
				continue
			}
			a := parseClimateAux(mp, currentMode(mp))
			base := fmt.Sprintf("%s/%s/%s", c.topicRoot, d.ID, mp.EmbeddedID)
			pub := func(suffix, val string) {
				_ = c.deps.MQTT.Publish(ctx, base+"/"+suffix+"/state", []byte(val), mqtt.QoS0, true)
			}
			if len(a.fanModes) > 0 {
				pub(hass.FanModeTopic, a.fanMode)
			}
			if len(a.swingModes) > 0 {
				pub(hass.SwingModeTopic, a.swing)
			}
			if len(a.swingHModes) > 0 {
				pub(hass.SwingHModeTopic, a.swingH)
			}
			if len(a.presetModes) > 0 {
				pub(hass.PresetModeTopic, a.preset)
			}
		}
	}
}

// currentMode returns the climateControl active operationMode value.
func currentMode(mp model.ManagementPoint) string {
	if ch, ok := mp.Characteristics["operationMode"]; ok {
		if s, ok := ch.String(); ok {
			return s
		}
	}
	return ""
}

// --- climate aux write handlers --------------------------------------------

// handleFanModeWrite sets the fan speed. A numeric mode switches fanSpeed to
// "fixed" and sets the fixed value; named modes (auto/quiet) set currentMode.
func (c *Coordinator) handleFanModeWrite(ctx context.Context, req writeReq) {
	mode := c.mode(req)
	if mode == "" {
		return
	}
	base := "/operationModes/" + mode + "/fanSpeed"
	payload := strings.TrimSpace(req.payload)
	if n, err := strconv.Atoi(payload); err == nil {
		c.patchClimate(ctx, req, "fanControl", base+"/currentMode", "fixed")
		c.patchClimate(ctx, req, "fanControl", base+"/modes/fixed", n)
		return
	}
	c.patchClimate(ctx, req, "fanControl", base+"/currentMode", payload)
}

// handleSwingWrite sets a swing direction (vertical / horizontal).
func (c *Coordinator) handleSwingWrite(ctx context.Context, req writeReq, direction string) {
	mode := c.mode(req)
	if mode == "" {
		return
	}
	// HA uses lower-case modes; map the known mixed-case Daikin value back.
	daikinVal := req.payload
	if req.payload == "windnice" {
		daikinVal = "windNice"
	}
	path := "/operationModes/" + mode + "/fanDirection/" + direction + "/currentMode"
	c.patchClimate(ctx, req, "fanControl", path, daikinVal)
}

// handlePresetWrite maps the HA preset to powerfulMode (boost/none).
func (c *Coordinator) handlePresetWrite(ctx context.Context, req writeReq) {
	switch strings.TrimSpace(req.payload) {
	case "boost":
		c.patchClimate(ctx, req, "powerfulMode", "", "on")
	case "none":
		c.patchClimate(ctx, req, "powerfulMode", "", "off")
	default:
		c.deps.Logger.Warn("coordinator.write_bad_preset",
			slog.String("topic", req.topic), slog.String("payload", req.payload))
	}
}

// mode returns the cached operationMode for the request's management point.
func (c *Coordinator) mode(req writeReq) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.modeCache[req.deviceID+"/"+req.embeddedID]
	if m == "" {
		c.deps.Logger.Warn("coordinator.write_no_mode", slog.String("topic", req.topic))
	}
	return m
}

// patchClimate issues a PATCH and logs the outcome.
func (c *Coordinator) patchClimate(ctx context.Context, req writeReq, characteristic, path string, value any) {
	if err := c.deps.Client.Patch(ctx, req.deviceID, req.embeddedID, characteristic, value, path); err != nil {
		c.deps.Logger.Warn("coordinator.patch_failed",
			slog.String("topic", req.topic), slog.String("characteristic", characteristic),
			slog.String("err", err.Error()))
		return
	}
	c.deps.Logger.Info("coordinator.patched",
		slog.String("device", req.deviceID), slog.String("characteristic", characteristic),
		slog.String("path", path), slog.String("value", req.payload))
}

// --- small JSON helpers ----------------------------------------------------

func jsonGet(raw json.RawMessage, key string) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

func jsonPath(raw json.RawMessage, keys ...string) (json.RawMessage, bool) {
	cur := raw
	for _, k := range keys {
		v, ok := jsonGet(cur, k)
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func jsonString(raw json.RawMessage, key string) string {
	v, ok := jsonGet(raw, key)
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

func jsonFloat(raw json.RawMessage, key string) (float64, bool) {
	v, ok := jsonGet(raw, key)
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(v, &f) != nil {
		return 0, false
	}
	return f, true
}

func jsonStringSlice(raw json.RawMessage, key string) []string {
	v, ok := jsonGet(raw, key)
	if !ok {
		return nil
	}
	var s []string
	_ = json.Unmarshal(v, &s)
	return s
}
