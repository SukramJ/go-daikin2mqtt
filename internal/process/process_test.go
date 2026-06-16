// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package process

import (
	"strings"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

const catalogYAML = `
- match: {managementPointType: climateControl, characteristic: onOffMode}
  topic: power
  platform: switch
  settable: true
- match: {managementPointType: climateControl, characteristic: sensoryData}
  topic: room_temperature
  platform: sensor
  unit: "°C"
  value_path: roomTemperature
  precision: 1
- match: {managementPointType: climateControl, characteristic: temperatureControl}
  topic: temperature_setpoint
  platform: number
  settable: true
  unit: "°C"
  precision: 1
  value_path: operationModes/{mode}/setpoints/roomTemperature
`

const deviceJSON = `[{
  "id": "dev-1",
  "deviceModel": "dx4",
  "managementPoints": [{
    "embeddedId": "climateControl",
    "managementPointType": "climateControl",
    "managementPointCategory": "primary",
    "onOffMode": {"value": "on", "settable": true},
    "operationMode": {"value": "cooling", "settable": true},
    "sensoryData": {"settable": false, "value": {
      "roomTemperature": {"value": 20, "unit": "°C", "minValue": -25, "maxValue": 48, "stepValue": 1, "settable": false}
    }},
    "temperatureControl": {"settable": true, "value": {"operationModes": {
      "cooling": {"setpoints": {"roomTemperature": {"value": 22.5, "unit": "°C", "minValue": 18, "maxValue": 32, "stepValue": 0.5, "settable": true}}}
    }}}
  }]
}]`

func resolveFixture(t *testing.T) map[string]Point {
	t.Helper()
	cat, err := catalog.Load(strings.NewReader(catalogYAML))
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	devices, err := model.ParseDevices([]byte(deviceJSON))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	byTopic := map[string]Point{}
	points := Resolve(devices, cat)
	for i := range points {
		byTopic[points[i].Topic] = points[i]
	}
	return byTopic
}

func TestResolveTopLevelScalar(t *testing.T) {
	pts := resolveFixture(t)
	p, ok := pts["power"]
	if !ok {
		t.Fatal("power point missing")
	}
	if p.Value != "on" || p.Format() != "on" {
		t.Errorf("power = %v / %q, want on", p.Value, p.Format())
	}
	if !p.Settable {
		t.Error("power should be settable")
	}
}

func TestResolveNestedSensoryData(t *testing.T) {
	pts := resolveFixture(t)
	p, ok := pts["room_temperature"]
	if !ok {
		t.Fatal("room_temperature point missing")
	}
	if f, _ := p.Value.(float64); f != 20 {
		t.Errorf("room_temperature value = %v, want 20", p.Value)
	}
	if p.Unit != "°C" {
		t.Errorf("unit = %q, want °C (from nested wrapper)", p.Unit)
	}
	if p.Format() != "20.0" {
		t.Errorf("format = %q, want 20.0", p.Format())
	}
}

func TestResolveModeScopedSetpoint(t *testing.T) {
	pts := resolveFixture(t)
	p, ok := pts["temperature_setpoint"]
	if !ok {
		t.Fatal("temperature_setpoint point missing (mode substitution failed?)")
	}
	if f, _ := p.Value.(float64); f != 22.5 {
		t.Errorf("setpoint value = %v, want 22.5", p.Value)
	}
	if p.Min == nil || *p.Min != 18 || p.Max == nil || *p.Max != 32 || p.Step == nil || *p.Step != 0.5 {
		t.Errorf("setpoint bounds not resolved from API: min=%v max=%v step=%v", p.Min, p.Max, p.Step)
	}
	if !p.Settable {
		t.Error("setpoint should be settable")
	}
}

func TestResolveSkipsUnknownMode(t *testing.T) {
	// A device with no operationMode cannot resolve the {mode} setpoint.
	dev := strings.ReplaceAll(deviceJSON, `"operationMode": {"value": "cooling", "settable": true},`, "")
	cat, _ := catalog.Load(strings.NewReader(catalogYAML))
	devices, _ := model.ParseDevices([]byte(dev))
	for _, p := range Resolve(devices, cat) {
		if p.Topic == "temperature_setpoint" {
			t.Fatal("setpoint should be skipped when operationMode is absent")
		}
	}
}
