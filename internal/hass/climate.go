// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"fmt"

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

// ClimateInfo carries the available fan/swing/preset option lists for a
// climate management point (derived from fanControl / powerfulMode). Empty
// lists omit the corresponding climate feature.
type ClimateInfo struct {
	FanModes             []string
	SwingModes           []string
	SwingHorizontalModes []string
	PresetModes          []string
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
	Name                    string   `json:"name"`
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

	Device device `json:"device"`
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

// climateEntities builds combined climate entities from the resolved points.
// It returns the discovery messages plus the set of point keys
// (deviceID|embeddedID|topic) the climate entity consumes, so the caller can
// suppress the redundant individual control entities (power/mode/setpoint).
func (d *Discovery) climateEntities(points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) (msgs []pubMsg, consumed map[string]bool) {
	consumed = map[string]bool{}
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

	for _, key := range order {
		g := groups[key]
		// A climate entity needs a controllable mode and a setpoint; without
		// both (e.g. an air purifier) we keep the individual entities.
		if g.mode == nil || g.setpoint == nil {
			continue
		}
		topic, payload, ok := d.buildClimate(g, infos[g.deviceID], climateInfos[key])
		if !ok {
			continue
		}
		msgs = append(msgs, pubMsg{topic: topic, payload: payload})
		// Suppress the individual control entities the climate entity replaces.
		consumed[g.deviceID+"|"+g.embeddedID+"|"+g.power.Topic] = true
		consumed[g.deviceID+"|"+g.embeddedID+"|"+g.mode.Topic] = true
		consumed[g.deviceID+"|"+g.embeddedID+"|"+g.setpoint.Topic] = true
	}
	return msgs, consumed
}

// buildClimate renders one climate entity config.
func (d *Discovery) buildClimate(g *climateGroup, info DeviceInfo, ci ClimateInfo) (topic string, payload []byte, ok bool) {
	uid := sanitize(fmt.Sprintf("daikin_%s_climate", g.deviceID))
	auxBase := func(suffix string) string {
		return fmt.Sprintf("%s/%s/%s/%s", d.stateRoot, g.deviceID, g.embeddedID, suffix)
	}
	modeBase := auxBase(HVACModeTopic)

	modes := []string{"off"}
	for _, v := range g.mode.Entry.Values {
		if m, mapped := daikinToHA[v.Value]; mapped {
			modes = append(modes, m)
		}
	}

	cfg := climatePayload{
		Name:                    "Thermostat",
		DefaultEntityID:         "climate." + uid,
		UniqueID:                uid,
		Modes:                   modes,
		ModeStateTopic:          modeBase + "/state",
		ModeCommandTopic:        modeBase + "/set",
		TemperatureStateTopic:   d.StateTopic(*g.setpoint),
		TemperatureCommandTopic: d.CommandTopic(*g.setpoint),
		MinTemp:                 g.setpoint.Min,
		MaxTemp:                 g.setpoint.Max,
		TempStep:                g.setpoint.Step,
		AvailabilityTopic:       d.BridgeStatusTopic(),
		PayloadAvailable:        "online",
		PayloadNotAvailable:     "offline",
		Device:                  d.deviceBlock(g.deviceID, info),
	}
	if g.current != nil {
		cfg.CurrentTemperatureTopic = d.StateTopic(*g.current)
	}

	// Optional fan / swing / preset features, advertised only when available.
	if len(ci.FanModes) > 0 {
		cfg.FanModes = ci.FanModes
		cfg.FanModeStateTopic = auxBase(FanModeTopic) + "/state"
		cfg.FanModeCommandTopic = auxBase(FanModeTopic) + "/set"
	}
	if len(ci.SwingModes) > 0 {
		cfg.SwingModes = ci.SwingModes
		cfg.SwingModeStateTopic = auxBase(SwingModeTopic) + "/state"
		cfg.SwingModeCommandTopic = auxBase(SwingModeTopic) + "/set"
	}
	if len(ci.SwingHorizontalModes) > 0 {
		cfg.SwingHorizontalModes = ci.SwingHorizontalModes
		cfg.SwingHorizontalModeStateTopic = auxBase(SwingHModeTopic) + "/state"
		cfg.SwingHorizontalModeCommandTopic = auxBase(SwingHModeTopic) + "/set"
	}
	if len(ci.PresetModes) > 0 {
		cfg.PresetModes = ci.PresetModes
		cfg.PresetModeStateTopic = auxBase(PresetModeTopic) + "/state"
		cfg.PresetModeCommandTopic = auxBase(PresetModeTopic) + "/set"
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, false
	}
	return fmt.Sprintf("%s/climate/%s/config", d.baseTopic, uid), b, true
}
