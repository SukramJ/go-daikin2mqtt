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

// StateTopic returns the app-level state topic for a host (`state/<host>`).
// Faikin floods this topic with OS/heartbeat documents and publishes the full
// AC document only occasionally — prefer [StatusTopic] as the AC source.
func StateTopic(host string) string { return "state/" + host }

// StatusTopic returns the S21 status topic (`state/<host>/status`), which
// carries the AC state (including quiet/econo/…) reliably on every poll. Its
// field names differ from the app-level document (see [ParseStatus]).
func StatusTopic(host string) string { return "state/" + host + "/status" }

// s21ToAppMode maps the single-letter S21 mode used on the /status topic to the
// app-level mode the rest of the daemon expects.
var s21ToAppMode = map[string]string{
	"C": "cool",
	"H": "heat",
	"A": "auto",
	"D": "dry",
	"F": "fan",
}

// statusDoc is the subset of the `state/<host>/status` (S21) document we map.
// Note the field-name differences vs the app document: `home` is the room
// temperature, `temp` is the setpoint, `mode` is a single letter, and energy is
// `Whheating`/`Whcooling`.
type statusDoc struct {
	Power     bool    `json:"power"`
	Mode      string  `json:"mode"` // S21 letter: C/H/A/D/F
	Temp      float64 `json:"temp"` // setpoint
	Home      float64 `json:"home"` // room temperature
	Outside   float64 `json:"outside"`
	Hum       float64 `json:"hum"`
	Fan       string  `json:"fan"`
	Quiet     bool    `json:"quiet"`
	Econo     bool    `json:"econo"`
	Powerful  bool    `json:"powerful"`
	Streamer  bool    `json:"streamer"`
	Comfort   bool    `json:"comfort"`
	Demand    int     `json:"demand"`
	Whheating int64   `json:"Whheating"`
	Whcooling int64   `json:"Whcooling"`
}

// ParseStatus decodes a `state/<host>/status` (S21) payload into a [State],
// remapping the differing field names/forms. Like [ParseState] it sets HasAC
// from the presence of "power" so callers skip non-AC messages.
func ParseStatus(host string, payload []byte) (*State, error) {
	var d statusDoc
	if err := json.Unmarshal(payload, &d); err != nil {
		return nil, fmt.Errorf("faikin: parse status %q: %w", host, err)
	}
	s := &State{
		Host:       host,
		Power:      d.Power,
		Mode:       s21ToAppMode[d.Mode], // "" for an unknown letter → HAMode "off"
		Fan:        d.Fan,
		Quiet:      d.Quiet,
		Econo:      d.Econo,
		Powerful:   d.Powerful,
		Streamer:   d.Streamer,
		Comfort:    d.Comfort,
		Target:     d.Temp, // /status `temp` is the setpoint
		Temp:       d.Home, // /status `home` is the room temperature
		Outside:    d.Outside,
		Hum:        d.Hum,
		Demand:     d.Demand,
		EnergyHeat: d.Whheating,
		EnergyCool: d.Whcooling,
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) == nil {
		_, s.HasAC = probe["power"]
	}
	return s, nil
}

// CommandTopic returns a Faikin command topic for a host and setting suffix
// (`<prefix>/<host>/command/<suffix>`, e.g. "Faikout/Klima SZ/command/quiet").
// Faikin applies the dedicated per-setting command topics (payload "1"/"0" for
// switches) reliably; the combined `command/control` JSON does not take effect
// for outdoor silent on multi-split units.
func CommandTopic(prefix, host, suffix string) string {
	return prefix + "/" + host + "/command/" + suffix
}

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
