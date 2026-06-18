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
	// default_entity_id is the full English, device-unique entity_id so the HA
	// entity_id is English regardless of the (possibly German) device name.
	if got := cfg["default_entity_id"]; got != "sensor.daikin_dev-1_room_temperature" {
		t.Errorf("default_entity_id = %v, want sensor.daikin_dev-1_room_temperature (English)", got)
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

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
