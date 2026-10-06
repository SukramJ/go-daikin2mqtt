// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"fmt"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/model"

	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// Synthetic per-management-point topic suffixes for the combined HA climate
// entity. The coordinator publishes their state (derived from onOffMode /
// operationMode / fanControl / powerfulMode) and handles commands on them.
const (
	HVACModeTopic   = "hvac_mode"
	FanModeTopic    = "fan_mode"
	SwingModeTopic  = "swing_mode"
	SwingHModeTopic = "swing_h_mode"
	PresetModeTopic = "preset_mode"
)

// ClimateInfo carries the available fan/swing/preset options for a climate
// management point (derived from fanControl / powerfulMode). An enum with no
// codes omits the corresponding climate feature.
//
// Each is an enum rather than a list of strings because the status item carries
// the stable token (`quiet`, `windnice`, `boost`) while Home Assistant lists the
// localized label, and the discovery payload maps one onto the other in both
// directions ([climateEnumTemplates]). Holding both in one [model.Enum] is what
// keeps the option list and the two templates from disagreeing.
type ClimateInfo struct {
	FanModes             *model.Enum
	SwingModes           *model.Enum
	SwingHorizontalModes *model.Enum
	PresetModes          *model.Enum
}

// hasCodes reports whether an option enum offers anything at all.
func hasCodes(e *model.Enum) bool { return e != nil && len(e.Codes) > 0 }

// climateEnumTemplates is the state/command template pair for one climate
// role key under the status-object encoding.
//
// Where a label differs from its token in lang — German fan and swing names,
// say — the pair maps token to label for display and label back to token for
// the command, through go-hamqtt's [discovery.EnumTemplates] reading the
// object's `val`. Where none does, Home Assistant shows the token itself (and
// its own translation of the modes it knows), so only `val` is read and no
// command template is needed: the option Home Assistant sends back already is
// the token.
func climateEnumTemplates(e *model.Enum, lang string) (state, command string) {
	for _, code := range e.Codes {
		if e.Label(code, lang) != code {
			return discovery.EnumTemplates(e, lang, discovery.StatusValueField)
		}
	}
	return discovery.StatusValueTemplate, ""
}

// daikinToHA maps a Daikin operationMode to a Home Assistant hvac mode.
var daikinToHA = map[string]string{
	"heating": "heat",
	"cooling": "cool",
	"auto":    "heat_cool",
	"dry":     "dry",
	"fanOnly": "fan_only",
}

// HVACMode computes the HA hvac mode from onOffMode and operationMode: "off"
// when the unit is off, otherwise the mapped operationMode (falling back to
// "off" for an unmapped mode).
func HVACMode(onOff, operationMode string) string {
	if onOff == "off" {
		return "off"
	}
	if m, ok := daikinToHA[operationMode]; ok {
		return m
	}
	return "off"
}

// DaikinModeForHA maps an HA hvac mode back to a Daikin operationMode. The
// second result is false for "off" (handled via onOffMode) or unknown modes.
func DaikinModeForHA(ha string) (string, bool) {
	for d, h := range daikinToHA {
		if h == ha {
			return d, true
		}
	}
	return "", false
}

// climatePayload is the HA MQTT climate discovery config.
type climatePayload struct {
	Name string `json:"name"`
	// DefaultEntityID seeds the entity_id; see the configPayload comment in
	// discovery.go. The older object_id field is not published — HA replaced it
	// with default_entity_id and now drops it silently.
	DefaultEntityID         string   `json:"default_entity_id"`
	UniqueID                string   `json:"unique_id"`
	Modes                   []string `json:"modes,omitempty"`
	ModeStateTopic          string   `json:"mode_state_topic,omitempty"`
	ModeCommandTopic        string   `json:"mode_command_topic,omitempty"`
	CurrentTemperatureTopic string   `json:"current_temperature_topic,omitempty"`
	TemperatureStateTopic   string   `json:"temperature_state_topic,omitempty"`
	TemperatureCommandTopic string   `json:"temperature_command_topic,omitempty"`
	MinTemp                 *float64 `json:"min_temp,omitempty"`
	MaxTemp                 *float64 `json:"max_temp,omitempty"`
	TempStep                *float64 `json:"temp_step,omitempty"`

	FanModes            []string `json:"fan_modes,omitempty"`
	FanModeStateTopic   string   `json:"fan_mode_state_topic,omitempty"`
	FanModeCommandTopic string   `json:"fan_mode_command_topic,omitempty"`

	SwingModes            []string `json:"swing_modes,omitempty"`
	SwingModeStateTopic   string   `json:"swing_mode_state_topic,omitempty"`
	SwingModeCommandTopic string   `json:"swing_mode_command_topic,omitempty"`

	SwingHorizontalModes            []string `json:"swing_horizontal_modes,omitempty"`
	SwingHorizontalModeStateTopic   string   `json:"swing_horizontal_mode_state_topic,omitempty"`
	SwingHorizontalModeCommandTopic string   `json:"swing_horizontal_mode_command_topic,omitempty"`

	PresetModes            []string `json:"preset_modes,omitempty"`
	PresetModeStateTopic   string   `json:"preset_mode_state_topic,omitempty"`
	PresetModeCommandTopic string   `json:"preset_mode_command_topic,omitempty"`

	AvailabilityTopic   string `json:"availability_topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`

	JSONAttributesTopic string `json:"json_attributes_topic,omitempty"`

	Device device `json:"device"`
}

// ClimateAttributesTopic returns the climate entity's JSON-attributes status
// item, `<name>/status/<device>/<embedded>/climate/attributes`.
func (d *Discovery) ClimateAttributesTopic(deviceID, embeddedID string) string {
	return d.state.Climate(deviceID, embeddedID).Attributes()
}

// climateGroup collects the climateControl points relevant to one climate
// entity (one management point of one device).
type climateGroup struct {
	deviceID   string
	embeddedID string
	power      *process.Point // onOffMode (topic "power")
	mode       *process.Point // operationMode (topic "operation_mode")
	setpoint   *process.Point // temperature_setpoint or leaving_water_setpoint
	current    *process.Point // room_temperature
}

// pubMsg is a topic/payload pair ready to publish.
type pubMsg struct {
	topic   string
	payload []byte
}

// climateEligibleGroups collects the climateControl points of each management
// point and returns, in discovery order, the groups that qualify for a
// composite climate entity.
//
// Split out from [Discovery.climateEntities] at ADR 0070 phase 8 step 4 so the
// library rendering path decides which management points become a composite
// from the SAME code rather than from a second copy of the rule — which is F3
// in a different plane. It is a pure extraction: no rule, no order and no byte
// changed, which the twelve pinned scenario goldens and their digests hold.
func climateEligibleGroups(points []process.Point) []*climateGroup {
	groups := map[string]*climateGroup{}
	var order []string

	for i := range points {
		p := points[i]
		if p.MPType != "climateControl" {
			continue
		}
		key := p.DeviceID + "|" + p.EmbeddedID
		g := groups[key]
		if g == nil {
			g = &climateGroup{deviceID: p.DeviceID, embeddedID: p.EmbeddedID}
			groups[key] = g
			order = append(order, key)
		}
		switch p.Topic {
		case "power":
			g.power = &points[i]
		case "operation_mode":
			g.mode = &points[i]
		case "temperature_setpoint", "leaving_water_setpoint":
			g.setpoint = &points[i]
		case "room_temperature":
			g.current = &points[i]
		}
	}

	out := make([]*climateGroup, 0, len(order))
	for _, key := range order {
		g := groups[key]
		// A climate entity needs a power switch, a controllable mode and a
		// setpoint; without all three (e.g. an air purifier) we keep the
		// individual entities.
		if g.power == nil || g.mode == nil || g.setpoint == nil {
			continue
		}
		out = append(out, g)
	}
	return out
}

// climateConsumedKeys names the three individual control entities a composite
// climate replaces, by their entity key (the catalogue topic).
//
// It is the suppression list on both paths: [Discovery.climateEntities]
// composes it into the `consumed` set the publish path filters on, and
// [Discovery.climateEntity] hands it to [model.Suppressor] so the library
// removes the same three.
func climateConsumedKeys(g *climateGroup) []string {
	return []string{g.power.Topic, g.mode.Topic, g.setpoint.Topic}
}

// climateEntities builds combined climate entities from the resolved points.
// It returns the discovery messages plus the set of point keys
// (deviceID|embeddedID|topic) the climate entity consumes, so the caller can
// suppress the redundant individual control entities (power/mode/setpoint).
func (d *Discovery) climateEntities(points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) (msgs []pubMsg, consumed map[string]bool) {
	consumed = map[string]bool{}
	for _, g := range climateEligibleGroups(points) {
		topic, payload, ok := d.buildClimate(g, infos[g.deviceID], climateInfos[g.deviceID+"|"+g.embeddedID])
		if !ok {
			continue
		}
		msgs = append(msgs, pubMsg{topic: topic, payload: payload})
		// Suppress the individual control entities the climate entity replaces.
		for _, key := range climateConsumedKeys(g) {
			consumed[g.deviceID+"|"+g.embeddedID+"|"+key] = true
		}
	}
	return msgs, consumed
}

// buildClimate renders one climate entity config on the frozen 0.13 layout —
// see [Discovery.Publish] for why it is kept and why it does not move.
func (d *Discovery) buildClimate(g *climateGroup, info DeviceInfo, ci ClimateInfo) (topic string, payload []byte, ok bool) {
	uid := sanitize(fmt.Sprintf("daikin_%s_climate", g.deviceID))
	// The composite climate's five synthetic slots (hvac_mode, fan_mode,
	// swing_mode, swing_h_mode, preset_mode) are backed by no catalogue entry
	// and no register, so they are the slots most likely to drift from the
	// coordinator's publish path — they have no shared process.Point to keep
	// them honest. They go through the same layout (F3).
	aux := func(suffix string) layout.LegacySlot {
		return d.legacy.Slot(g.deviceID, g.embeddedID, suffix)
	}
	mode := aux(HVACModeTopic)

	modes := []string{"off"}
	for _, v := range g.mode.Entry.Values {
		if m, mapped := daikinToHA[v.Value]; mapped {
			modes = append(modes, m)
		}
	}

	cfg := climatePayload{
		Name:                    "Thermostat",
		DefaultEntityID:         "climate." + entityObjectID(info.Name, "thermostat"),
		UniqueID:                uid,
		Modes:                   modes,
		ModeStateTopic:          mode.State(),
		ModeCommandTopic:        mode.Command(),
		TemperatureStateTopic:   d.legacySlot(*g.setpoint).State(),
		TemperatureCommandTopic: d.legacySlot(*g.setpoint).Command(),
		MinTemp:                 g.setpoint.Min,
		MaxTemp:                 g.setpoint.Max,
		TempStep:                g.setpoint.Step,
		AvailabilityTopic:       d.legacy.BridgeStatus(),
		PayloadAvailable:        "online",
		PayloadNotAvailable:     "offline",
		JSONAttributesTopic:     d.legacy.Climate(g.deviceID, g.embeddedID).Attributes(),
		Device:                  d.deviceBlock(g.deviceID, info),
	}
	if g.current != nil {
		cfg.CurrentTemperatureTopic = d.legacySlot(*g.current).State()
	}

	// Optional fan / swing / preset features, advertised only when available.
	if hasCodes(ci.FanModes) {
		cfg.FanModes = ci.FanModes.Options(d.lang)
		cfg.FanModeStateTopic = aux(FanModeTopic).State()
		cfg.FanModeCommandTopic = aux(FanModeTopic).Command()
	}
	if hasCodes(ci.SwingModes) {
		cfg.SwingModes = ci.SwingModes.Options(d.lang)
		cfg.SwingModeStateTopic = aux(SwingModeTopic).State()
		cfg.SwingModeCommandTopic = aux(SwingModeTopic).Command()
	}
	if hasCodes(ci.SwingHorizontalModes) {
		cfg.SwingHorizontalModes = ci.SwingHorizontalModes.Options(d.lang)
		cfg.SwingHorizontalModeStateTopic = aux(SwingHModeTopic).State()
		cfg.SwingHorizontalModeCommandTopic = aux(SwingHModeTopic).Command()
	}
	if hasCodes(ci.PresetModes) {
		cfg.PresetModes = ci.PresetModes.Options(d.lang)
		cfg.PresetModeStateTopic = aux(PresetModeTopic).State()
		cfg.PresetModeCommandTopic = aux(PresetModeTopic).Command()
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, false
	}
	return d.ConfigTopic("climate", uid), b, true
}
