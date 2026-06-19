// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// capturePub records published messages.
type capturePub struct {
	msgs map[string][]byte
}

func (c *capturePub) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, _ bool) error {
	if c.msgs == nil {
		c.msgs = map[string][]byte{}
	}
	c.msgs[topic] = payload
	return nil
}

func samplePoint() process.Point {
	return process.Point{
		DeviceID:   "dev-1",
		EmbeddedID: "climateControl",
		MPType:     "climateControl",
		Topic:      "room_temperature",
		Unit:       "°C",
		Entry: catalog.Entry{
			Topic:       "room_temperature",
			Name:        "Room temperature",
			NameDE:      "Raumtemperatur",
			Platform:    "sensor",
			DeviceClass: "temperature",
			StateClass:  "measurement",
		},
		Value: 20.0,
	}
}

// TestDiscoveryEnglishEntityIDLocalizedName verifies the core requirement:
// default_entity_id (which seeds the HA entity_id) is the English topic, while
// name is localized.
func TestDiscoveryEnglishEntityIDLocalizedName(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "de", pub)
	if err := d.Publish(context.Background(), []process.Point{samplePoint()}, map[string]DeviceInfo{"dev-1": {Name: "Wohnzimmer", Model: "FTXA20", ModelID: "dx4", SerialNumber: "J035347"}}, nil); err != nil {
		t.Fatal(err)
	}

	const wantTopic = "homeassistant/sensor/daikin_dev-1_room_temperature/config"
	raw, ok := pub.msgs[wantTopic]
	if !ok {
		t.Fatalf("config not published to %s; got %v", wantTopic, keys(pub.msgs))
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// default_entity_id keeps the device-name prefix but uses the English topic
	// for the measurement, so the HA entity_id stays English even though the
	// display name (and the German locale) is set.
	if got := cfg["default_entity_id"]; got != "sensor.wohnzimmer_room_temperature" {
		t.Errorf("default_entity_id = %v, want sensor.wohnzimmer_room_temperature (English)", got)
	}
	if got := cfg["name"]; got != "Raumtemperatur" {
		t.Errorf("name = %v, want Raumtemperatur (localized)", got)
	}
	if got := cfg["state_topic"]; got != "daikin/dev-1/climateControl/room_temperature/state" {
		t.Errorf("state_topic = %v", got)
	}
	dev, _ := cfg["device"].(map[string]any)
	if dev["name"] != "Wohnzimmer" {
		t.Errorf("device.name = %v, want Wohnzimmer", dev["name"])
	}
}

// TestDiscoveryEnglishNameFallback verifies English is used when no German
// override exists / lang is en.
func TestDiscoveryEnglishNameFallback(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	_ = d.Publish(context.Background(), []process.Point{samplePoint()}, nil, nil)
	raw := pub.msgs["homeassistant/sensor/daikin_dev-1_room_temperature/config"]
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	if cfg["name"] != "Room temperature" {
		t.Errorf("name = %v, want Room temperature", cfg["name"])
	}
}

// TestDiscoverySelectLocalizedOptions verifies select options are localized
// labels and that each label maps back to its raw API code.
func TestDiscoverySelectLocalizedOptions(t *testing.T) {
	entry := catalog.Entry{
		Topic:    "operation_mode",
		Name:     "Operation mode",
		NameDE:   "Betriebsart",
		Platform: "select",
		Settable: true,
		Values: []catalog.ValueLabel{
			{Value: "heating", Label: "Heating", LabelDE: "Heizen"},
			{Value: "cooling", Label: "Cooling", LabelDE: "Kühlen"},
		},
	}
	p := process.Point{DeviceID: "dev-1", EmbeddedID: "climateControl", Topic: "operation_mode", Entry: entry, Value: "cooling"}

	pub := &capturePub{}
	d := New("homeassistant", "daikin", "de", pub)
	if err := d.Publish(context.Background(), []process.Point{p}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal(pub.msgs["homeassistant/select/daikin_dev-1_operation_mode/config"], &cfg)

	opts, _ := cfg["options"].([]any)
	want := map[string]bool{"Heizen": true, "Kühlen": true}
	if len(opts) != 2 {
		t.Fatalf("options = %v, want 2 localized labels", opts)
	}
	for _, o := range opts {
		if !want[o.(string)] {
			t.Errorf("unexpected option %v, want localized German labels", o)
		}
	}

	// Round-trip: the localized label maps back to the raw API code.
	if code, ok := entry.CodeForLabel("Kühlen"); !ok || code != "cooling" {
		t.Errorf("CodeForLabel(Kühlen) = (%q,%v), want (cooling,true)", code, ok)
	}
}

func TestEntityObjectID(t *testing.T) {
	cases := []struct {
		device, topic, want string
	}{
		{"Galerie", "room_temperature", "galerie_room_temperature"},
		{"Schlafzimmer", "outdoor_temperature", "schlafzimmer_outdoor_temperature"},
		{"Daikin Außengerät", "outdoor_unit_model", "daikin_aussengerat_outdoor_unit_model"},
		// Adjacent duplicate token ("gateway") is collapsed.
		{"Galerie Gateway", "gateway_firmware_version", "galerie_gateway_firmware_version"},
		{"Wohnzimmer", "powerful_mode", "wohnzimmer_powerful_mode"},
	}
	for _, c := range cases {
		if got := entityObjectID(c.device, c.topic); got != c.want {
			t.Errorf("entityObjectID(%q, %q) = %q, want %q", c.device, c.topic, got, c.want)
		}
	}
}

func outdoorSilentPoint(dev string) process.Point {
	return process.Point{
		DeviceID:   dev,
		EmbeddedID: "climateControl",
		MPType:     "climateControl",
		Topic:      "outdoor_silent",
		Entry: catalog.Entry{
			Topic:    "outdoor_silent",
			Name:     "Outdoor silent",
			NameDE:   "Außen Geräuscharm",
			Platform: "switch",
			Settable: true,
			Scope:    "outdoor",
		},
		Value: "off",
	}
}

// TestDiscoveryOutdoorScopedDedup verifies a scope:outdoor setting collapses to
// a single entity on the outdoor device, regardless of how many indoor units
// expose it.
func TestDiscoveryOutdoorScopedDedup(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "de", pub)
	infos := map[string]DeviceInfo{
		"dev-1": {Name: "Galerie", Outdoor: &SubDevice{SerialNumber: "OD1"}},
		"dev-2": {Name: "Wohnzimmer", Outdoor: &SubDevice{SerialNumber: "OD1"}},
	}
	if err := d.Publish(context.Background(),
		[]process.Point{outdoorSilentPoint("dev-1"), outdoorSilentPoint("dev-2")}, infos, nil); err != nil {
		t.Fatal(err)
	}

	const wantTopic = "homeassistant/switch/daikin_outdoor_OD1_outdoor_silent/config"
	raw, ok := pub.msgs[wantTopic]
	if !ok {
		t.Fatalf("missing outdoor-scoped config %q; got %v", wantTopic, keys(pub.msgs))
	}
	if _, ok := pub.msgs["homeassistant/switch/daikin_dev-2_outdoor_silent/config"]; ok {
		t.Error("per-device outdoor_silent should have been deduplicated away")
	}
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	dev, _ := cfg["device"].(map[string]any)
	ids, _ := dev["identifiers"].([]any)
	if len(ids) != 1 || ids[0] != "daikin_outdoor_OD1" {
		t.Errorf("device identifiers = %v, want [daikin_outdoor_OD1]", ids)
	}
	if cfg["name"] != "Außen Geräuscharm" {
		t.Errorf("name = %v, want localized 'Außen Geräuscharm'", cfg["name"])
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
