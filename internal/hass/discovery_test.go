// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// capturePub records published messages.
type capturePub struct {
	msgs map[string][]byte
}

func (c *capturePub) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, _ bool, _ ...mqtt.PublishOption) error {
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
	if _, err := d.Publish(context.Background(), []process.Point{samplePoint()}, map[string]DeviceInfo{"dev-1": {Name: "Wohnzimmer", Model: "FTXA20", ModelID: "dx4", SerialNumber: "J035347"}}, nil); err != nil {
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
	// The deprecated object_id must not be published at all: no MQTT platform
	// schema accepts it, so HA drops it silently.
	if _, ok := cfg["object_id"]; ok {
		t.Errorf("object_id present in config, want it omitted")
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
	_, _ = d.Publish(context.Background(), []process.Point{samplePoint()}, nil, nil)
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
	if _, err := d.Publish(context.Background(), []process.Point{p}, nil, nil); err != nil {
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
	if _, err := d.Publish(context.Background(),
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

func refreshPoint(dev string) process.Point {
	return process.Point{
		DeviceID:   dev,
		EmbeddedID: "climateControl",
		MPType:     "climateControl",
		Topic:      RefreshTopic,
		Entry: catalog.Entry{
			Topic:    RefreshTopic,
			Name:     "Refresh from cloud",
			NameDE:   "Aus Cloud aktualisieren",
			Platform: "button",
			Icon:     "mdi:cloud-refresh",
			Settable: true,
			Scope:    "outdoor",
		},
	}
}

// TestDiscoveryRefreshButton verifies the manual cloud-refresh button: it
// collapses to one command-only entity on the outdoor device, and carries no
// state_topic — HA's MQTT button has no state and rejects a config with one.
func TestDiscoveryRefreshButton(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "de", pub)
	infos := map[string]DeviceInfo{
		"dev-1": {Name: "Galerie", Outdoor: &SubDevice{SerialNumber: "OD1"}},
		"dev-2": {Name: "Wohnzimmer", Outdoor: &SubDevice{SerialNumber: "OD1"}},
	}
	if _, err := d.Publish(context.Background(),
		[]process.Point{refreshPoint("dev-1"), refreshPoint("dev-2")}, infos, nil); err != nil {
		t.Fatal(err)
	}

	const wantTopic = "homeassistant/button/daikin_outdoor_OD1_refresh/config"
	raw, ok := pub.msgs[wantTopic]
	if !ok {
		t.Fatalf("missing refresh button config %q; got %v", wantTopic, keys(pub.msgs))
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["state_topic"]; ok {
		t.Errorf("button config carries a state_topic (%v); HA rejects that", cfg["state_topic"])
	}
	if got := cfg["command_topic"]; got != "daikin/dev-1/climateControl/refresh/set" {
		t.Errorf("command_topic = %v, want daikin/dev-1/climateControl/refresh/set", got)
	}
	if got := cfg["payload_press"]; got != "PRESS" {
		t.Errorf("payload_press = %v, want PRESS", got)
	}
	if got := cfg["icon"]; got != "mdi:cloud-refresh" {
		t.Errorf("icon = %v, want mdi:cloud-refresh", got)
	}
	if got := cfg["name"]; got != "Aus Cloud aktualisieren" {
		t.Errorf("name = %v, want the localized name", got)
	}
	dev, _ := cfg["device"].(map[string]any)
	ids, _ := dev["identifiers"].([]any)
	if len(ids) != 1 || ids[0] != "daikin_outdoor_OD1" {
		t.Errorf("device identifiers = %v, want [daikin_outdoor_OD1] (the outdoor unit)", ids)
	}
	if _, ok := pub.msgs["homeassistant/button/daikin_dev-2_refresh/config"]; ok {
		t.Error("second indoor unit published its own button instead of deduplicating")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestIsOwnConfig(t *testing.T) {
	d := New("homeassistant", "daikin", "en", &capturePub{})
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"our sensor", `{"unique_id":"daikin_dev1_room_temperature","state_topic":"daikin/dev1/climateControl/room_temperature/state"}`, true},
		{"our climate (no state_topic)", `{"unique_id":"daikin_dev1_climate","mode_state_topic":"daikin/dev1/climateControl/hvac_mode/state"}`, true},
		{"foreign integration", `{"unique_id":"zigbee2mqtt_0x123","state_topic":"zigbee2mqtt/x"}`, false},
		{"daikin uid but foreign state topic", `{"unique_id":"daikin_dev1_x","state_topic":"other/x"}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		if got := d.IsOwnConfig([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: IsOwnConfig = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLegacyConfigTopicFormNamesTheFormThisBridgePublishes is the step-6 guard.
//
// go-hamqtt's publisher.SupersededTopics defaults to LegacyTopicWithNodeID, the
// FIVE-segment form "<prefix>/<platform>/<node_id>/<object_id>/config". This
// bridge has never published that shape, so the default would retract nothing:
// the device bundle would land while all 264 per-entity configs were still
// retained, Home Assistant would refuse it with one "Received a conflicting
// MQTT discovery message", and no entities would appear.
//
// The two candidate forms are transcribed here rather than imported, because
// nothing in this repository takes the go-hamqtt dependency yet. Both are
// rendered over the real builder's output, and only one of them reproduces it.
func TestLegacyConfigTopicFormNamesTheFormThisBridgePublishes(t *testing.T) {
	t.Parallel()

	const prefix = "homeassistant"
	d := New(prefix, "daikin", "en", nil)

	// go-hamqtt publisher.LegacyTopicByUniqueID and LegacyTopicWithNodeID,
	// transcribed. They delete themselves when the dependency is taken.
	byUniqueID := func(platform, nodeID, uid string) string {
		_ = nodeID
		return prefix + "/" + platform + "/" + uid + "/config"
	}
	withNodeID := func(platform, nodeID, uid string) string {
		return prefix + "/" + platform + "/" + nodeID + "/" + uid + "/config"
	}

	for _, c := range []struct{ platform, nodeID, uid string }{
		{"sensor", "daikin_809d41d9", "daikin_809d41d9-4d42-45fa-af6a-84b512143672_room_temperature"},
		{"climate", "daikin_809d41d9", "daikin_809d41d9-4d42-45fa-af6a-84b512143672_climate"},
		{"switch", "daikin_scheduler", "daikin_schedule_werktag"},
		{"button", "daikin_outdoor_ODU0000000001", "daikin_outdoor_ODU0000000001_refresh"},
	} {
		published := d.ConfigTopic(c.platform, c.uid)
		if got := byUniqueID(c.platform, c.nodeID, c.uid); got != published {
			t.Errorf("LegacyTopicByUniqueID renders %q, the bridge publishes %q", got, published)
		}
		if got := withNodeID(c.platform, c.nodeID, c.uid); got == published {
			t.Errorf("LegacyTopicWithNodeID renders %q, which the bridge also publishes — "+
				"the two forms are no longer distinguishable and this test proves nothing", got)
		}
	}

	if LegacyConfigTopicForm != "publisher.LegacyTopicByUniqueID" {
		t.Errorf("LegacyConfigTopicForm = %q, but the four-segment form is the one measured",
			LegacyConfigTopicForm)
	}
}

// TestConfigTopicIsTheOneFormAllThreeBuildersUse pins that the catalogue
// entity, the composite climate and the schedule switch agree on the config
// topic. They were three fmt.Sprintf expressions, and the composite climate's
// and the schedule switch's hard-coded their platform into the format string.
func TestConfigTopicIsTheOneFormAllThreeBuildersUse(t *testing.T) {
	t.Parallel()

	d := New("homeassistant", "daikin", "en", nil)
	for _, c := range []struct{ platform, uid, want string }{
		{"sensor", "daikin_x_room_temperature", "homeassistant/sensor/daikin_x_room_temperature/config"},
		{"climate", "daikin_x_climate", "homeassistant/climate/daikin_x_climate/config"},
		{"switch", "daikin_schedule_werktag", "homeassistant/switch/daikin_schedule_werktag/config"},
	} {
		if got := d.ConfigTopic(c.platform, c.uid); got != c.want {
			t.Errorf("ConfigTopic(%q, %q) = %q, want %q", c.platform, c.uid, got, c.want)
		}
	}
	// And the reconcile filter must match what the builder produces, or the
	// orphan sweep collects nothing and every renamed entity lingers forever.
	if f := d.ConfigFilter(); f != "homeassistant/+/+/config" {
		t.Errorf("ConfigFilter = %q, want %q", f, "homeassistant/+/+/config")
	}
}

// TestEntityIDSeedIsLanguageIndependentOnEverySubDevicePath pins F2 on all
// four paths entityIdentity can take, not just the two a shipped fixture
// reaches.
//
// Mutation testing found the gap: making subDeviceBlock (an outdoorUnit
// management point WITHOUT a serial) seed from the localized name again
// survived the whole suite, because no ONECTA fixture in this repository has
// that shape. It is reachable in the field — an outdoor unit whose serial the
// cloud does not report — and the consequence there is the same one F2
// describes: an operator switching LANGUAGE gets a second set of entities and
// the first set is stranded, because Home Assistant never renames a registered
// entity.
func TestEntityIDSeedIsLanguageIndependentOnEverySubDevicePath(t *testing.T) {
	t.Parallel()

	info := DeviceInfo{
		Name:    "Wohnzimmer",
		Gateway: &SubDevice{SerialNumber: "GW1"},
		Outdoor: &SubDevice{SerialNumber: "ODU1"},
		ModelID: "dx4",
	}
	noSerial := DeviceInfo{Name: "Wohnzimmer", ModelID: "dx4"}

	cases := []struct {
		name string
		p    process.Point
		info DeviceInfo
		want string // the expected entity-id seed slug, in BOTH languages
	}{
		{"scope:outdoor shared", pointFor("outdoor_silent", "climateControl", "outdoor"), info, "daikin_outdoor_unit_outdoor_silent"},
		{"outdoorUnit shared", pointFor("outdoor_temperature", "outdoorUnit", ""), info, "daikin_outdoor_unit_outdoor_temperature"},
		{"outdoorUnit, no serial", pointFor("outdoor_temperature", "outdoorUnit", ""), noSerial, "wohnzimmer_outdoor_unit_outdoor_temperature"},
		{"gateway", pointFor("wifi_strength", "gateway", ""), info, "gateway_wohnzimmer_wifi_strength"},
		{"main device", pointFor("room_temperature", "climateControl", ""), info, "wohnzimmer_room_temperature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var seeds []string
			for _, lang := range []string{"en", "de"} {
				d := New("homeassistant", "daikin", lang, nil)
				_, _, seed := d.entityIdentity(c.p, c.info)
				seeds = append(seeds, entityObjectID(seed, c.p.Topic))
			}
			if seeds[0] != seeds[1] {
				t.Errorf("entity-id seed moves with LANGUAGE: en=%q de=%q (F2)", seeds[0], seeds[1])
			}
			if seeds[0] != c.want {
				t.Errorf("entity-id seed = %q, want %q", seeds[0], c.want)
			}
		})
	}
}

// pointFor builds the minimal process.Point entityIdentity reads.
func pointFor(topic, mpType, scope string) process.Point {
	return process.Point{
		DeviceID:   "dev1",
		EmbeddedID: mpType,
		MPType:     mpType,
		Topic:      topic,
		Entry:      catalog.Entry{Platform: "sensor", Scope: scope},
	}
}
