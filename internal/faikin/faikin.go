// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package faikin decodes the local MQTT interface exposed by Faikin / Faikout
// ESP32 modules (revk/ESP32-Faikout) that replace the Daikin Wi-Fi adapter and
// talk to the indoor unit over its serial bus.
//
// Each module publishes a retained JSON document to `state/<host>` and accepts
// partial commands as JSON on `<prefix>/<host>/command/control` (the prefix is
// the firmware "app" name, e.g. "Faikout"). This package is the translation
// layer between that local representation and the daemon's domain model, so the
// coordinator can read and write a unit locally instead of via the ONECTA
// cloud. It performs no I/O.
package faikin

import (
	"encoding/json"
	"fmt"
)

// State is the decoded `state/<host>` document (the firmware's app-level view).
// Only the fields the daemon maps are modelled; unknown keys are ignored.
type State struct {
	Host string `json:"-"` // set by the caller from the topic, not the payload
	// HasAC reports whether the message actually carried the AC state. Faikin
	// also publishes OS/heartbeat documents to state/<host> (uptime, rssi, …)
	// with no AC fields; processing those would reset every entity to its zero
	// value (power off, temp 0, …), so callers must skip when this is false.
	HasAC bool `json:"-"`

	Online   bool   `json:"online"`
	Power    bool   `json:"power"`
	Mode     string `json:"mode"` // cool|heat|auto|dry|fan (off is Power=false)
	Fan      string `json:"fan"`  // auto|1..5|quiet|… (firmware-defined)
	Swing    string `json:"swing"`
	Quiet    bool   `json:"quiet"` // outdoor silent / low-noise
	Econo    bool   `json:"econo"`
	Powerful bool   `json:"powerful"`
	Streamer bool   `json:"streamer"`
	Comfort  bool   `json:"comfort"`
	Preset   string `json:"preset"`

	Target  float64 `json:"target"`  // setpoint °C
	Temp    float64 `json:"temp"`    // room temperature °C
	Hum     float64 `json:"hum"`     // relative humidity %
	Outside float64 `json:"outside"` // outdoor temperature °C
	Demand  int     `json:"demand"`  // demand-control limit %

	Energy     int64 `json:"energy"`     // total Wh
	EnergyHeat int64 `json:"energyheat"` // heating Wh
	EnergyCool int64 `json:"energycool"` // cooling Wh
}

// ParseState decodes a `state/<host>` payload, tagging it with its host. It
// also sets [State.HasAC] from the presence of the "power" key so callers can
// ignore the OS/heartbeat documents Faikin interleaves on the same topic.
func ParseState(host string, payload []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("faikin: parse state %q: %w", host, err)
	}
	s.Host = host
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) == nil {
		_, s.HasAC = probe["power"]
	}
	return &s, nil
}

// Control is a partial command for `<prefix>/<host>/command/control`. Only the
// non-nil fields are emitted; the firmware applies just the supplied keys, so
// a command never disturbs settings it does not mention.
type Control struct {
	Power    *bool    `json:"power,omitempty"`
	Mode     *string  `json:"mode,omitempty"`
	Temp     *float64 `json:"temp,omitempty"`
	Fan      *string  `json:"fan,omitempty"`
	Swing    *string  `json:"swing,omitempty"`
	Quiet    *bool    `json:"quiet,omitempty"`
	Econo    *bool    `json:"econo,omitempty"`
	Powerful *bool    `json:"powerful,omitempty"`
	Streamer *bool    `json:"streamer,omitempty"`
	Comfort  *bool    `json:"comfort,omitempty"`
	Demand   *int     `json:"demand,omitempty"` // demand-control limit % (40..100)
}

// JSON renders the command payload.
func (c Control) JSON() ([]byte, error) { return json.Marshal(c) }

// StateTopic returns the retained state topic for a host (`state/<host>`).
func StateTopic(host string) string { return "state/" + host }

// CommandTopic returns the control command topic for a host
// (`<prefix>/<host>/command/control`, e.g. "Faikout/Klima SZ/command/control").
func CommandTopic(prefix, host string) string {
	return prefix + "/" + host + "/command/control"
}

// HVAC mode mapping mirrors internal/coordinator's daikinToHA so local and
// cloud paths surface the same Home Assistant hvac_mode values.

// HAMode maps the Faikin power flag + app mode to an HA climate hvac_mode.
func (s *State) HAMode() string {
	if !s.Power {
		return "off"
	}
	if ha, ok := faikinToHA[s.Mode]; ok {
		return ha
	}
	return "off"
}

var faikinToHA = map[string]string{
	"cool": "cool",
	"heat": "heat",
	"auto": "heat_cool",
	"dry":  "dry",
	"fan":  "fan_only",
}

var haToFaikin = map[string]string{
	"cool":      "cool",
	"heat":      "heat",
	"heat_cool": "auto",
	"dry":       "dry",
	"fan_only":  "fan",
}

// ControlForHAMode builds the control needed to apply an HA hvac_mode: "off"
// powers the unit down; any mapped mode powers it on and selects the Faikin
// mode. ok is false for an unrecognized value.
func ControlForHAMode(haMode string) (c Control, ok bool) {
	if haMode == "off" {
		off := false
		return Control{Power: &off}, true
	}
	fm, ok := haToFaikin[haMode]
	if !ok {
		return Control{}, false
	}
	on := true
	return Control{Power: &on, Mode: &fm}, true
}
