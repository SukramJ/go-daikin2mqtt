// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

func TestHVACMode(t *testing.T) {
	cases := []struct{ onOff, op, want string }{
		{"off", "cooling", "off"},
		{"on", "cooling", "cool"},
		{"on", "heating", "heat"},
		{"on", "auto", "heat_cool"},
		{"on", "fanOnly", "fan_only"},
		{"on", "weird", "off"},
	}
	for _, c := range cases {
		if got := HVACMode(c.onOff, c.op); got != c.want {
			t.Errorf("HVACMode(%q,%q) = %q, want %q", c.onOff, c.op, got, c.want)
		}
	}
	if d, ok := DaikinModeForHA("heat"); !ok || d != "heating" {
		t.Errorf("DaikinModeForHA(heat) = (%q,%v), want (heating,true)", d, ok)
	}
	if _, ok := DaikinModeForHA("off"); ok {
		t.Error("DaikinModeForHA(off) should be false")
	}
}

func f64(v float64) *float64 { return &v }

func climatePoints() []process.Point {
	mk := func(topic, platform string, e catalog.Entry, val any) process.Point {
		e.Topic, e.Platform = topic, platform
		return process.Point{DeviceID: "dev-1", EmbeddedID: "climateControl", MPType: "climateControl", Topic: topic, Entry: e, Value: val}
	}
	power := mk("power", "switch", catalog.Entry{Settable: true}, "on")
	mode := mk("operation_mode", "select", catalog.Entry{Settable: true, Values: []catalog.ValueLabel{
		{Value: "heating", Label: "Heating"}, {Value: "cooling", Label: "Cooling"},
	}}, "cooling")
	setp := mk("temperature_setpoint", "number", catalog.Entry{Settable: true}, 22.5)
	setp.Min, setp.Max, setp.Step = f64(16), f64(32), f64(0.5)
	room := mk("room_temperature", "sensor", catalog.Entry{}, 21.0)
	return []process.Point{power, mode, setp, room}
}

func TestClimateEntitySuppressesIndividualControls(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	if err := d.Publish(context.Background(), climatePoints(), nil, nil); err != nil {
		t.Fatal(err)
	}

	// Climate entity published.
	raw, ok := pub.msgs["homeassistant/climate/daikin_dev-1_climate/config"]
	if !ok {
		t.Fatalf("climate config missing; got %v", keys(pub.msgs))
	}
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	if cfg["mode_command_topic"] != "daikin/dev-1/climateControl/hvac_mode/set" {
		t.Errorf("mode_command_topic = %v", cfg["mode_command_topic"])
	}
	if cfg["current_temperature_topic"] != "daikin/dev-1/climateControl/room_temperature/state" {
		t.Errorf("current_temperature_topic = %v", cfg["current_temperature_topic"])
	}
	if cfg["temperature_command_topic"] != "daikin/dev-1/climateControl/temperature_setpoint/set" {
		t.Errorf("temperature_command_topic = %v", cfg["temperature_command_topic"])
	}
	modes, _ := cfg["modes"].([]any)
	wantMode := map[string]bool{"off": true, "heat": true, "cool": true}
	for _, m := range modes {
		delete(wantMode, m.(string))
	}
	if len(wantMode) != 0 {
		t.Errorf("modes %v missing %v", modes, wantMode)
	}

	// Individual control entities suppressed; room sensor kept.
	for _, suppressed := range []string{
		"homeassistant/switch/daikin_dev-1_power/config",
		"homeassistant/select/daikin_dev-1_operation_mode/config",
		"homeassistant/number/daikin_dev-1_temperature_setpoint/config",
	} {
		if _, ok := pub.msgs[suppressed]; ok {
			t.Errorf("expected %s to be suppressed by the climate entity", suppressed)
		}
	}
	if _, ok := pub.msgs["homeassistant/sensor/daikin_dev-1_room_temperature/config"]; !ok {
		t.Error("room_temperature sensor should still be published")
	}
}

func TestSubDevicesDedupBySerial(t *testing.T) {
	// Two API devices share the same physical gateway + outdoor unit (same
	// serials). They must collapse to a single HA gateway and outdoor device.
	gwPoint := func(dev string) process.Point {
		return process.Point{
			DeviceID: dev, EmbeddedID: "gateway", MPType: "gateway", Topic: "gateway_ip_address",
			Entry: catalog.Entry{Topic: "gateway_ip_address", Platform: "sensor", Name: "Gateway IP", Category: "diagnostic"}, Value: "1.2.3.4",
		}
	}
	odPoint := func(dev string) process.Point {
		return process.Point{
			DeviceID: dev, EmbeddedID: "outdoorUnit", MPType: "outdoorUnit", Topic: "outdoor_unit_model",
			Entry: catalog.Entry{Topic: "outdoor_unit_model", Platform: "sensor", Name: "Outdoor model", Category: "diagnostic"}, Value: "3MXM52",
		}
	}
	infos := map[string]DeviceInfo{
		"dev-1": {Gateway: &SubDevice{Model: "BRP069C4x", SerialNumber: "GW1", MAC: "b0:65:3a:5c:cf:56"}, Outdoor: &SubDevice{Model: "3MXM52", SerialNumber: "OD1"}},
		"dev-2": {Gateway: &SubDevice{Model: "BRP069C4x", SerialNumber: "GW1", MAC: "b0:65:3a:5c:cf:56"}, Outdoor: &SubDevice{Model: "3MXM52", SerialNumber: "OD1"}},
	}

	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	if err := d.Publish(context.Background(), []process.Point{gwPoint("dev-1"), odPoint("dev-1"), gwPoint("dev-2"), odPoint("dev-2")}, infos, nil); err != nil {
		t.Fatal(err)
	}

	// Exactly one gateway and one outdoor entity (deduplicated by serial).
	gwConfigs, odConfigs := 0, 0
	for topic := range pub.msgs {
		if topic == "homeassistant/sensor/daikin_gateway_GW1_gateway_ip_address/config" {
			gwConfigs++
		}
		if topic == "homeassistant/sensor/daikin_outdoor_OD1_outdoor_unit_model/config" {
			odConfigs++
		}
	}
	if gwConfigs != 1 || odConfigs != 1 {
		t.Fatalf("expected 1 gateway + 1 outdoor config, got %d/%d; topics=%v", gwConfigs, odConfigs, keys(pub.msgs))
	}
	// No per-API-device duplicates.
	if _, ok := pub.msgs["homeassistant/sensor/daikin_dev-2_gateway_ip_address/config"]; ok {
		t.Error("dev-2 gateway should have been deduplicated away")
	}

	// The gateway is serial-identified, nests under its indoor unit (the first
	// one when a serial is shared), is diagnostic, and carries the MAC connection.
	var gwcfg map[string]any
	_ = json.Unmarshal(pub.msgs["homeassistant/sensor/daikin_gateway_GW1_gateway_ip_address/config"], &gwcfg)
	if gwcfg["entity_category"] != "diagnostic" {
		t.Errorf("gateway entity_category = %v, want diagnostic", gwcfg["entity_category"])
	}
	dev, _ := gwcfg["device"].(map[string]any)
	ids, _ := dev["identifiers"].([]any)
	if len(ids) != 1 || ids[0] != "daikin_gateway_GW1" {
		t.Errorf("gateway identifiers = %v, want [daikin_gateway_GW1]", ids)
	}
	if dev["via_device"] != "daikin_dev-1" {
		t.Errorf("gateway should nest under its indoor unit, via_device = %v, want daikin_dev-1", dev["via_device"])
	}
	if conns, _ := dev["connections"].([]any); len(conns) != 1 {
		t.Errorf("gateway device should carry a mac connection, got %v", conns)
	}

	// The outdoor unit, by contrast, stays standalone (shared across indoor
	// units, no single parent).
	var odcfg map[string]any
	_ = json.Unmarshal(pub.msgs["homeassistant/sensor/daikin_outdoor_OD1_outdoor_unit_model/config"], &odcfg)
	oddev, _ := odcfg["device"].(map[string]any)
	if _, hasVia := oddev["via_device"]; hasVia {
		t.Errorf("shared outdoor unit should be standalone (no via_device), got %v", oddev["via_device"])
	}
}
