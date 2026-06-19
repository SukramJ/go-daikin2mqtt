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
		if ctrl, ok := faikinControlFor(characteristic, value); ok {
			payload, err := ctrl.JSON()
			if err != nil {
				return err
			}
			topic := faikin.CommandTopic(c.deps.Cfg.LocalFaikinPrefix, host)
			return c.deps.FaikinMQTT.Publish(ctx, topic, payload, mqtt.QoS0, false)
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

// daikinToFaikinMode maps an ONECTA operationMode value to a Faikin app mode.
var daikinToFaikinMode = map[string]string{
	"cooling": "cool",
	"heating": "heat",
	"auto":    "auto",
	"dry":     "dry",
	"fanOnly": "fan",
}

// faikinControlFor translates a single ONECTA characteristic write into a
// partial Faikin control command. ok is false for characteristics the local
// firmware does not model, so the caller can fall back to the cloud.
func faikinControlFor(characteristic string, value any) (faikin.Control, bool) {
	b := func(v bool) *bool { return &v }
	s := func(v string) *string { return &v }
	switch characteristic {
	case "onOffMode":
		return faikin.Control{Power: b(truthy(value))}, true
	case "operationMode":
		m, ok := daikinToFaikinMode[toStr(value)]
		if !ok {
			return faikin.Control{}, false
		}
		return faikin.Control{Mode: s(m)}, true
	case "temperatureControl":
		f, ok := toFloat(value)
		if !ok {
			return faikin.Control{}, false
		}
		return faikin.Control{Temp: &f}, true
	case "powerfulMode":
		return faikin.Control{Powerful: b(truthy(value))}, true
	case "econoMode":
		return faikin.Control{Econo: b(truthy(value))}, true
	case "streamerMode":
		return faikin.Control{Streamer: b(truthy(value))}, true
	case "outdoorSilentMode":
		return faikin.Control{Quiet: b(truthy(value))}, true
	case "demandControl":
		f, ok := toFloat(value)
		if !ok {
			return faikin.Control{}, false
		}
		n := int(f)
		return faikin.Control{Demand: &n}, true
	}
	return faikin.Control{}, false
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
