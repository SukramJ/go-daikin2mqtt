// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"strconv"
	"strings"

	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
)

// setCharacteristic applies one setting change to a management point, choosing
// the control backend per device: the local Faikin interface when local mode is
// enabled and the device is mapped to a Faikin host AND the characteristic is
// locally controllable, otherwise the ONECTA cloud (PATCH). Characteristics
// Faikin does not model (e.g. mode-scoped fan direction the local firmware
// expresses differently) fall through to the cloud, so local mode degrades
// gracefully rather than dropping a command.
func (c *Coordinator) setCharacteristic(ctx context.Context, deviceID, embeddedID, characteristic string, value any, path string) error {
	if host, ok := c.localHost(deviceID); ok {
		if suffix, payload, ok := faikinCommand(characteristic, value); ok {
			topic := faikin.CommandTopic(c.deps.Cfg.LocalFaikinPrefix, host, suffix)
			return c.deps.FaikinMQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, false)
		}
		// Not locally controllable → fall through to the cloud below.
	}
	return c.deps.Client.Patch(ctx, deviceID, embeddedID, characteristic, value, path)
}

// localHost returns the Faikin host for a device when local control is active
// and the device is mapped, else ("", false).
func (c *Coordinator) localHost(deviceID string) (string, bool) {
	if c.deps.FaikinMQTT == nil || !c.deps.Cfg.LocalEnabled() {
		return "", false
	}
	return c.deps.Cfg.FaikinHost(deviceID)
}

// daikinToS21Mode maps an ONECTA operationMode value to the single-letter S21
// mode the Faikin `command/mode` topic expects.
var daikinToS21Mode = map[string]string{
	"cooling": "C",
	"heating": "H",
	"auto":    "A",
	"dry":     "D",
	"fanOnly": "F",
}

// faikinCommand translates a single ONECTA characteristic write into a Faikin
// command — the dedicated `command/<suffix>` topic and its payload (switches use
// "1"/"0", matching the firmware's own HA discovery). ok is false for
// characteristics the local firmware does not model, so the caller falls back to
// the cloud.
func faikinCommand(characteristic string, value any) (suffix, payload string, ok bool) {
	onoff := func(v any) string {
		if truthy(v) {
			return "1"
		}
		return "0"
	}
	switch characteristic {
	case "onOffMode":
		return "power", onoff(value), true
	case "powerfulMode":
		return "powerful", onoff(value), true
	case "econoMode":
		return "econo", onoff(value), true
	case "streamerMode":
		return "streamer", onoff(value), true
	case "outdoorSilentMode":
		return "quiet", onoff(value), true
	case "operationMode":
		m, ok := daikinToS21Mode[toStr(value)]
		if !ok {
			return "", "", false
		}
		return "mode", m, true
	case "temperatureControl":
		f, ok := toFloat(value)
		if !ok {
			return "", "", false
		}
		return "temp", strconv.FormatFloat(f, 'f', -1, 64), true
	case "demandControl":
		f, ok := toFloat(value)
		if !ok {
			return "", "", false
		}
		return "demand", strconv.Itoa(int(f)), true
	}
	return "", "", false
}

// truthy interprets the on/off forms ONECTA and HA use as a boolean.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "on", "true", "1":
			return true
		}
		return false
	}
	return false
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}
