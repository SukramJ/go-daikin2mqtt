// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package faikin decodes the local MQTT interface exposed by Faikin / Faikout
// ESP32 modules (revk/ESP32-Faikout) that replace the Daikin Wi-Fi adapter and
// talk to the indoor unit over its serial bus.
//
// Each module publishes its state to `state/<host>` (and the richer S21 view to
// `state/<host>/status`), and accepts per-setting commands on
// `command/<host>/<suffix>` (e.g. `command/<host>/quiet`, payload "true"/"false"). This
// package is the translation layer between that local representation and the
// daemon's domain model, so the coordinator can read and write a unit locally
// instead of via the ONECTA cloud. It performs no I/O.
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
	Liquid  float64 `json:"liquid"`  // refrigerant liquid-line temperature °C
	Demand  int     `json:"demand"`  // demand-control limit %

	// Live telemetry the cloud does not expose (local-only).
	Consumption int     `json:"consumption"` // current power draw, W
	Comp        float64 `json:"comp"`        // compressor frequency, Hz
	FanFreq     float64 `json:"fanfreq"`     // indoor fan frequency, Hz

	Energy     int64 `json:"energy"`     // lifetime total Wh
	EnergyHeat int64 `json:"energyheat"` // lifetime heating Wh
	EnergyCool int64 `json:"energycool"` // lifetime cooling Wh
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

// StateTopic returns the firmware's canonical state topic for a host
// (`state/<host>`). It is retained and the topic every entity in Faikin's own
// HA discovery reads from; the app document carries the full AC state
// (mode as a word, temp = room temperature, target = setpoint). OS/heartbeat
// documents lacking `power` are filtered by [State.HasAC].
func StateTopic(host string) string { return "state/" + host }

// CommandTopic returns a Faikin command topic for a host and setting suffix:
// `command/<host>/<suffix>`, e.g. "command/Klima SZ/quiet". This mirrors the
// firmware's own HA discovery (revk_topic(topiccommand)) — the app name
// ("Faikout") is not part of the path, just as the state topic is `state/<host>`.
// Faikin applies these dedicated per-setting topics (payload "true"/"false" for
// switches) reliably; the combined `command/control` JSON does not take effect
// for outdoor silent on multi-split units.
func CommandTopic(host, suffix string) string {
	return "command/" + host + "/" + suffix
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
