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
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
)

// --- Local Faikin fan/swing translation -----------------------------------
//
// Faikin and the ONECTA cloud use different fan/swing vocabularies, so local
// fan/swing control needs an explicit mapping (not a passthrough):
//   - Fan: the cloud uses `auto`/`quiet`/`1`..`5`; Faikin's `command/<host>/fan`
//     matches its own names but falls back to the first character, so the
//     single-char codes `A`/`Q`/`1`..`5` work for any unit (3- or 5-speed).
//   - Swing: the cloud has two axes (vertical `swing_mode`, horizontal
//     `swing_h_mode`); Faikin has one combined `swing` (off/H/V/H+V/C). A
//     per-axis write is combined with the other axis's current state.

// cloudFanToFaikin maps a canonical cloud fan value to a `command/<host>/fan`
// payload (a single Faikin fan character). ok is false for values Faikin cannot
// express, so the caller falls back to the cloud.
func cloudFanToFaikin(v string) (string, bool) {
	switch v {
	case "auto":
		return "A", true
	case "quiet":
		return "Q", true
	case "1", "2", "3", "4", "5":
		return v, true
	}
	return "", false
}

// faikinFanToCloud maps the Faikin state `fan` name back to the canonical cloud
// fan value the climate fan_mode entity expects.
var faikinFanToCloud = map[string]string{
	"auto": "auto", "low": "1", "lowMedium": "2", "medium": "3",
	"mediumHigh": "4", "high": "5", "night": "quiet", "quiet": "quiet",
}

// faikinSwingAxes derives the (vertical, horizontal) cloud swing states from
// Faikin's combined `swing` value.
func faikinSwingAxes(s string) (vertical, horizontal string) {
	switch s {
	case "V":
		return "swing", "stop"
	case "H":
		return "stop", "swing"
	case "H+V", "on":
		return "swing", "swing"
	case "C":
		return "windnice", "stop" // comfort airflow is vertical
	default: // "off"
		return "stop", "stop"
	}
}

// faikinSwingCombine builds Faikin's combined `swing` command from the desired
// vertical+horizontal cloud states.
func faikinSwingCombine(vertical, horizontal string) string {
	if vertical == "windnice" {
		return "C" // comfort airflow
	}
	switch v, h := vertical == "swing", horizontal == "swing"; {
	case v && h:
		return "H+V"
	case v:
		return "V"
	case h:
		return "H"
	default:
		return "off"
	}
}

// faikinAux publishes a per-setting Faikin command (`command/<host>/<suffix>`).
func (c *Coordinator) faikinAux(ctx context.Context, host, suffix, value string) error {
	return c.deps.FaikinMQTT.Publish(ctx, faikin.CommandTopic(host, suffix), []byte(value), mqtt.QoS0, false)
}

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

// langDE is the language code for which the climate aux dropdowns are
// localized; any other code keeps the raw (English) values.
const langDE = "de"

// German display labels for the climate fan/swing/preset dropdowns, keyed by
// the canonical (lower-cased) Daikin value used internally. The strings mirror
// the native daikin_onecta HA integration (translations/de.json). Values not
// listed here pass through unchanged (numeric fan speeds, unmapped modes).
//
// A native HA integration keeps the raw value and localizes only the displayed
// state via integration translations. MQTT discovery has no separate label
// field — the list entry is both the displayed option and the command value —
// so the German label is emitted directly and reversed back to the raw value
// on write (see [canonicalAux] and the aux write handlers).
var (
	fanModeDE = map[string]string{
		"auto":  "Automatisch",
		"quiet": "Leise",
	}
	swingModeDE = map[string]string{
		"stop":                "Stopp",
		"swing":               "Schwingen",
		"windnice":            "Komfort Luftstrom",
		"floorheatingairflow": "Fußbodenheizung Luftstrom",
	}
	presetModeDE = map[string]string{
		"boost": "Boost",
	}
)

// localizeAux returns the German display label for a canonical climate aux
// value, or the value unchanged for other languages and for unmapped values
// (numeric fan speeds, unknown modes).
func localizeAux(value, lang string, table map[string]string) string {
	if lang == langDE {
		if de, ok := table[value]; ok {
			return de
		}
	}
	return value
}

// localizeAuxList localizes every entry of an option list (see [localizeAux]).
func localizeAuxList(values []string, lang string, table map[string]string) []string {
	if lang != langDE || len(values) == 0 {
		return values
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = localizeAux(v, lang, table)
	}
	return out
}

// canonicalAux reverses a (possibly localized) label back to the canonical
// lower-cased Daikin value. Unmapped labels — English values, numeric fan
// speeds — pass through unchanged so the existing write logic still applies.
func canonicalAux(label, lang string, table map[string]string) string {
	if lang == langDE {
		for canon, de := range table {
			if de == label {
				return canon
			}
		}
	}
	return label
}

// info converts the option lists into the discovery-facing [hass.ClimateInfo],
// localizing the option labels for lang. The raw API values are recovered on
// the write path via [canonicalAux].
func (a climateAux) info(lang string) hass.ClimateInfo {
	return hass.ClimateInfo{
		FanModes:             localizeAuxList(a.fanModes, lang, fanModeDE),
		SwingModes:           localizeAuxList(a.swingModes, lang, swingModeDE),
		SwingHorizontalModes: localizeAuxList(a.swingHModes, lang, swingModeDE),
		PresetModes:          localizeAuxList(a.presetModes, lang, presetModeDE),
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
func climateInfos(devices []model.Device, lang string) map[string]hass.ClimateInfo {
	out := map[string]hass.ClimateInfo{}
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			if mp.Type != "climateControl" {
				continue
			}
			a := parseClimateAux(mp, currentMode(mp))
			out[d.ID+"|"+mp.EmbeddedID] = a.info(lang)
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
			lang := c.deps.Cfg.Language
			base := fmt.Sprintf("%s/%s/%s", c.topicRoot, d.ID, mp.EmbeddedID)
			pub := func(suffix, val string) {
				_ = c.deps.MQTT.Publish(ctx, base+"/"+suffix+"/state", []byte(val), mqtt.QoS0, true)
			}
			// In local mode the Faikin read path owns fan/swing (the cloud poll's
			// values are stale for a locally-controlled unit), so skip them here.
			localFanSwing := c.localActiveFor(d.ID)
			// State payloads carry the localized label so the HA dropdown (whose
			// options are localized) highlights the current selection; the write
			// path reverses the label back to the raw value.
			if len(a.fanModes) > 0 && !localFanSwing {
				pub(hass.FanModeTopic, localizeAux(a.fanMode, lang, fanModeDE))
			}
			if len(a.swingModes) > 0 && !localFanSwing {
				pub(hass.SwingModeTopic, localizeAux(a.swing, lang, swingModeDE))
			}
			if len(a.swingHModes) > 0 && !localFanSwing {
				pub(hass.SwingHModeTopic, localizeAux(a.swingH, lang, swingModeDE))
			}
			if len(a.presetModes) > 0 {
				pub(hass.PresetModeTopic, localizeAux(a.preset, lang, presetModeDE))
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
	// Reverse a localized label back to the raw value; numeric speeds and
	// English values pass through unchanged.
	payload := canonicalAux(strings.TrimSpace(req.payload), c.deps.Cfg.Language, fanModeDE)
	// Local-first: route to Faikin's command/<host>/fan when mapped.
	if host, ok := c.localHost(req.deviceID); ok {
		if fv, ok := cloudFanToFaikin(payload); ok {
			if err := c.faikinAux(ctx, host, "fan", fv); err != nil {
				c.deps.Logger.Warn("coordinator.local_fan_failed",
					slog.String("topic", req.topic), slog.String("err", err.Error()))
			}
			return
		}
		// Unmappable fan value → fall through to the cloud.
	}
	mode := c.mode(req)
	if mode == "" {
		return
	}
	base := "/operationModes/" + mode + "/fanSpeed"
	if n, err := strconv.Atoi(payload); err == nil {
		c.patchClimate(ctx, req, "fanControl", base+"/currentMode", "fixed")
		c.patchClimate(ctx, req, "fanControl", base+"/modes/fixed", n)
		return
	}
	c.patchClimate(ctx, req, "fanControl", base+"/currentMode", payload)
}

// handleSwingWrite sets a swing direction (vertical / horizontal).
func (c *Coordinator) handleSwingWrite(ctx context.Context, req writeReq, direction string) {
	// Reverse a localized label to the canonical lower-cased value.
	daikinVal := canonicalAux(req.payload, c.deps.Cfg.Language, swingModeDE)
	// Local-first: combine this axis with the other axis's current state into
	// Faikin's single `swing` command. floorheatingairflow has no Faikin
	// equivalent, so it still goes to the cloud.
	if host, ok := c.localHost(req.deviceID); ok && daikinVal != "floorheatingairflow" {
		v, h := faikinSwingAxes(c.currentFaikinSwing(req.deviceID))
		if direction == "vertical" {
			v = daikinVal
		} else {
			h = daikinVal
		}
		if err := c.faikinAux(ctx, host, "swing", faikinSwingCombine(v, h)); err != nil {
			c.deps.Logger.Warn("coordinator.local_swing_failed",
				slog.String("topic", req.topic), slog.String("err", err.Error()))
		}
		return
	}
	mode := c.mode(req)
	if mode == "" {
		return
	}
	// Restore the known mixed-case Daikin spellings (the cloud API is case-sensitive).
	switch daikinVal {
	case "windnice":
		daikinVal = "windNice"
	case "floorheatingairflow":
		daikinVal = "floorHeatingAirflow"
	}
	path := "/operationModes/" + mode + "/fanDirection/" + direction + "/currentMode"
	c.patchClimate(ctx, req, "fanControl", path, daikinVal)
}

// currentFaikinSwing returns the device's last-seen combined Faikin swing value
// (default "off"), used to combine a per-axis write with the other axis.
func (c *Coordinator) currentFaikinSwing(deviceID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.lastLocal[deviceID]; ok && st != nil {
		return st.Swing
	}
	return "off"
}

// handlePresetWrite maps the HA preset to powerfulMode (boost/none).
func (c *Coordinator) handlePresetWrite(ctx context.Context, req writeReq) {
	switch canonicalAux(strings.TrimSpace(req.payload), c.deps.Cfg.Language, presetModeDE) {
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

// patchClimate applies a climate sub-setting via the active backend (local
// Faikin or cloud) and logs the outcome.
func (c *Coordinator) patchClimate(ctx context.Context, req writeReq, characteristic, path string, value any) {
	if err := c.setCharacteristic(ctx, req.deviceID, req.embeddedID, characteristic, value, path); err != nil {
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
