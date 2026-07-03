// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// realFaikinState is a verbatim `state/Klima SZ` payload from a live module.
const realFaikinState = `{"online":true,"power":true,"target":22.50,"temp":21.00,"hum":66.00,"outside":28.00,"liquid":13.00,"demand":100,"consumption":120,"comp":25.0,"fanfreq":9.5,"energy":772600,"energyheat":71000,"energycool":117300,"mode":"cool","fan":"auto","streamer":false,"quiet":false,"econo":true,"comfort":false,"powerful":false,"swing":"off","preset":"eco"}`

func localReadCoordinator(t *testing.T, faikinMQTT, mainMQTT *stubMQTT) *Coordinator {
	t.Helper()
	cfg := &config.Config{
		MQTTTopic:         "daikin",
		Language:          "de",
		LocalMode:         true,
		LocalFaikinPrefix: "Faikout",
		LocalDeviceMap:    map[string]string{"dev1": "Klima SZ"},
	}
	return New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: mainMQTT, FaikinMQTT: faikinMQTT,
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
}

func TestPublishLocalState(t *testing.T) {
	main := newStubMQTT()
	c := localReadCoordinator(t, newStubMQTT(), main)
	// Simulate a prior cloud poll having learned the device's embeddedID.
	c.climateEmbedded["dev1"] = "climateControl"

	st, err := faikin.ParseState("Klima SZ", []byte(realFaikinState))
	if err != nil {
		t.Fatal(err)
	}
	c.publishLocalState(context.Background(), "dev1", st)

	want := map[string]string{
		"power/state":                "on",
		"hvac_mode/state":            "cool",
		"room_temperature/state":     "21.0",
		"temperature_setpoint/state": "22.5",
		"operation_mode/state":       "Kühlen", // localized (Language=de)
		"powerful_mode/state":        "off",
		"econo_mode/state":           "on",  // econo:true in the payload
		"streamer/state":             "off", // streamer:false
		"outdoor_silent/state":       "off", // quiet:false
		"demand_control/state":       "100", // demand:100
		// Local-only telemetry; energy Wh -> kWh.
		"energy_total/state":            "772.600",
		"heating_energy_total/state":    "71.000",
		"cooling_energy_total/state":    "117.300",
		"power_consumption/state":       "120",
		"compressor_frequency/state":    "25.0",
		"fan_frequency/state":           "9.5",
		"refrigerant_temperature/state": "13.0",
	}
	for suffix, exp := range want {
		topic := "daikin/dev1/climateControl/" + suffix
		got, ok := main.get(topic)
		if !ok {
			t.Errorf("missing local publish %q", topic)
			continue
		}
		if got.payload != exp {
			t.Errorf("%s = %q, want %q", suffix, got.payload, exp)
		}
		if !got.retain {
			t.Errorf("%s should be retained", suffix)
		}
	}
}

func TestPublishLocalStateSkipsWithoutEmbeddedID(t *testing.T) {
	main := newStubMQTT()
	c := localReadCoordinator(t, newStubMQTT(), main)
	// No climateEmbedded entry → cannot route; nothing published.
	st, _ := faikin.ParseState("Klima SZ", []byte(realFaikinState))
	c.publishLocalState(context.Background(), "dev1", st)
	if main.count() != 0 {
		t.Errorf("expected no publishes without embeddedID, got %d", main.count())
	}
}

func TestSubscribeLocalRoutesStateMessages(t *testing.T) {
	faikinMQTT := newStubMQTT()
	main := newStubMQTT()
	c := localReadCoordinator(t, faikinMQTT, main)
	c.climateEmbedded["dev1"] = "climateControl"

	c.subscribeLocal(context.Background())
	// The Faikin broker subscription must target the host's state topic.
	if faikinMQTT.filter != "state/Klima SZ" {
		t.Fatalf("subscribed filter = %q, want state/Klima SZ", faikinMQTT.filter)
	}
	// Simulate an inbound Faikin state message → it should be republished.
	faikinMQTT.handler("state/Klima SZ", []byte(realFaikinState), false)
	if _, ok := main.get("daikin/dev1/climateControl/hvac_mode/state"); !ok {
		t.Error("inbound Faikin state was not republished to the main broker")
	}
}

func TestLocalOnlyPoints(t *testing.T) {
	const cy = `
- match: {managementPointType: climateControl, characteristic: econoMode}
  topic: econo_mode
  platform: switch
  settable: true
  scope: outdoor
- match: {managementPointType: climateControl, characteristic: streamerMode}
  topic: streamer
  platform: switch
  settable: true
- match: {managementPointType: climateControl, characteristic: outdoorSilentMode}
  topic: outdoor_silent
  platform: switch
  settable: true
  scope: outdoor
- match: {managementPointType: climateControl, characteristic: demandControl}
  topic: demand_control
  platform: number
  settable: true
  scope: outdoor
`
	cat, err := catalog.Load(strings.NewReader(cy))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		MQTTTopic: "daikin", Language: "de", LocalMode: true,
		LocalFaikinPrefix: "Faikout", LocalDeviceMap: map[string]string{"dev1": "Klima SZ"},
	}
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: newStubMQTT(), FaikinMQTT: newStubMQTT(),
		Catalog: cat, Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.climateEmbedded["dev1"] = "climateControl"

	devs := []model.Device{{ID: "dev1"}}
	pts := c.localOnlyPoints(devs, nil)

	got := map[string]process.Point{}
	for _, p := range pts {
		got[p.Topic] = p
	}
	for _, want := range []string{"econo_mode", "streamer", "outdoor_silent", "demand_control"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing synthesized point %q", want)
		}
	}
	// demand_control (number) gets HA bounds.
	if d := got["demand_control"]; d.Min == nil || d.Max == nil || *d.Min != 40 || *d.Max != 100 {
		t.Errorf("demand_control bounds = %v/%v, want 40/100", d.Min, d.Max)
	}
	// Unmapped device → nothing.
	if n := len(c.localOnlyPoints([]model.Device{{ID: "other"}}, nil)); n != 0 {
		t.Errorf("unmapped device synthesized %d points, want 0", n)
	}
	// A topic already resolved from the cloud is skipped.
	resolved := []process.Point{{DeviceID: "dev1", Topic: "econo_mode"}}
	for _, p := range c.localOnlyPoints(devs, resolved) {
		if p.Topic == "econo_mode" {
			t.Error("econo_mode should be skipped when already cloud-resolved")
		}
	}
}

func TestPublishLocalStateSkipsOSHeartbeat(t *testing.T) {
	main := newStubMQTT()
	c := localReadCoordinator(t, newStubMQTT(), main)
	c.climateEmbedded["dev1"] = "climateControl"
	// An OS heartbeat (HasAC=false) must not publish anything (else every AC
	// entity would be reset to its zero value).
	c.publishLocalState(context.Background(), "dev1", &faikin.State{Host: "Klima SZ", HasAC: false})
	if main.count() != 0 {
		t.Errorf("OS heartbeat published %d topics, want 0", main.count())
	}
}

func TestFlushLocalStatesAfterEmbeddedID(t *testing.T) {
	main := newStubMQTT()
	c := localReadCoordinator(t, newStubMQTT(), main)
	st, _ := faikin.ParseState("Klima SZ", []byte(realFaikinState))

	// Before the embeddedID is known, the state is cached but not published
	// (mirrors the retained Faikin state arriving at subscribe, pre-poll).
	c.publishLocalState(context.Background(), "dev1", st)
	if main.count() != 0 {
		t.Fatalf("expected no publish before embeddedID, got %d", main.count())
	}

	// After the cloud poll populates the embeddedID, the flush publishes it.
	c.climateEmbedded["dev1"] = "climateControl"
	c.flushLocalStates(context.Background())
	if _, ok := main.get("daikin/dev1/climateControl/room_temperature/state"); !ok {
		t.Error("flush did not publish the cached Faikin state once embeddedID was known")
	}
}

func TestPublishOutdoorSharedAggregates(t *testing.T) {
	main := newStubMQTT()
	cfg := &config.Config{
		MQTTTopic: "daikin", Language: "de", LocalMode: true, LocalFaikinPrefix: "Faikout",
		LocalDeviceMap: map[string]string{"a": "Klima A", "b": "Klima B"},
	}
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: main, FaikinMQTT: newStubMQTT(),
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	// Same outdoor unit; a is idle (quiet/econo off), b (active) reports them on.
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "climateControl", "b": "climateControl"}
	c.lastLocal = map[string]*faikin.State{
		"a": {HasAC: true, Quiet: false, Econo: false, Demand: 100},
		"b": {HasAC: true, Quiet: true, Econo: true, Demand: 80},
	}

	c.publishOutdoorShared(context.Background(), "a")

	// The aggregate (any-on / most-restrictive) is published to BOTH members,
	// so the single HA entity reflects it whichever member it reads.
	for _, dev := range []string{"a", "b"} {
		if got, _ := main.get("daikin/" + dev + "/climateControl/outdoor_silent/state"); got.payload != "on" {
			t.Errorf("%s outdoor_silent = %q, want on (OR across group)", dev, got.payload)
		}
		if got, _ := main.get("daikin/" + dev + "/climateControl/econo_mode/state"); got.payload != "on" {
			t.Errorf("%s econo_mode = %q, want on (OR across group)", dev, got.payload)
		}
		if got, _ := main.get("daikin/" + dev + "/climateControl/demand_control/state"); got.payload != "80" {
			t.Errorf("%s demand_control = %q, want 80 (min across group)", dev, got.payload)
		}
	}
}

func TestOutdoorHold(t *testing.T) {
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	c.outdoorSerial = map[string]string{"dev1": "OD1"}

	// Write ON: the value is held even though the aggregate is still "off"
	// (the active indoor unit has not reported the change yet) → no snap-back.
	c.holdOutdoor("dev1", "outdoor_silent", "on")
	if got := c.heldOutdoorValue("OD1", "outdoor_silent", "off"); got != "on" {
		t.Errorf("held = %q, want on (held while unconfirmed)", got)
	}
	// A status that matches the held value confirms and clears the hold.
	if got := c.heldOutdoorValue("OD1", "outdoor_silent", "on"); got != "on" {
		t.Errorf("confirm = %q, want on", got)
	}
	// Hold cleared → the raw aggregate passes through again.
	if got := c.heldOutdoorValue("OD1", "outdoor_silent", "off"); got != "off" {
		t.Errorf("after confirm raw should pass through, got %q", got)
	}
	// No pending → raw is returned unchanged.
	if got := c.heldOutdoorValue("OD2", "demand_control", "80"); got != "80" {
		t.Errorf("no pending should return raw, got %q", got)
	}
}

func TestOutdoorTelemetryAggregate(t *testing.T) {
	main := newStubMQTT()
	cfg := &config.Config{
		MQTTTopic: "daikin", Language: "de", LocalMode: true, LocalFaikinPrefix: "Faikout",
		LocalDeviceMap: map[string]string{"a": "Klima A", "b": "Klima B"},
	}
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: main, FaikinMQTT: newStubMQTT(),
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "climateControl", "b": "climateControl"}
	// Both units are active and report their OWN per-unit power/energy; the
	// compressor frequency is the shared outdoor value (identical). Feed them
	// through publishLocalState so the held energy totals populate naturally.
	c.publishLocalState(context.Background(), "a",
		&faikin.State{HasAC: true, Power: true, Demand: 100, Consumption: 80, Comp: 22, Energy: 778300, EnergyHeat: 71000, EnergyCool: 118700})
	c.publishLocalState(context.Background(), "b",
		&faikin.State{HasAC: true, Power: true, Demand: 100, Consumption: 90, Comp: 22, Energy: 785500, EnergyHeat: 164000, EnergyCool: 197200})

	// Per indoor unit: each shows its OWN energy/power.
	perUnit := map[string]map[string]string{
		"a": {"energy_total": "778.300", "power_consumption": "80"},
		"b": {"energy_total": "785.500", "power_consumption": "90"},
	}
	for dev, want := range perUnit {
		for suffix, exp := range want {
			if got, ok := main.get("daikin/" + dev + "/climateControl/" + suffix + "/state"); !ok || got.payload != exp {
				t.Errorf("per-unit %s %s = %q (ok=%v), want %q", dev, suffix, got.payload, ok, exp)
			}
		}
	}
	// At the outdoor unit: power/energy SUMMED (system total), compressor shared.
	// Published identically to every member (discovery dedups to one entity).
	outdoor := map[string]string{
		"outdoor_power":                "170",      // 80 + 90
		"compressor_frequency":         "22.0",     // shared
		"outdoor_energy_total":         "1563.800", // 778.300 + 785.500
		"outdoor_heating_energy_total": "235.000",  // 71.000 + 164.000
		"outdoor_cooling_energy_total": "315.900",  // 118.700 + 197.200
	}
	for _, dev := range []string{"a", "b"} {
		for suffix, exp := range outdoor {
			if got, ok := main.get("daikin/" + dev + "/climateControl/" + suffix + "/state"); !ok || got.payload != exp {
				t.Errorf("outdoor %s %s = %q (ok=%v), want %q", dev, suffix, got.payload, ok, exp)
			}
		}
	}
}

func TestOutdoorEnergyNotResetToZero(t *testing.T) {
	main := newStubMQTT()
	c := newCoordinator(t, &stubCloud{}, main)
	c.outdoorSerial = map[string]string{"dev1": "OD1"}
	c.climateEmbedded = map[string]string{"dev1": "climateControl"}
	// No member reports energy (all idle) → energy totals must NOT be published
	// (publishing 0 would reset the total_increasing counter in HA).
	c.lastLocal = map[string]*faikin.State{"dev1": {HasAC: true, Demand: 100}}

	c.publishOutdoorShared(context.Background(), "dev1")

	for _, suffix := range []string{"outdoor_energy_total", "outdoor_heating_energy_total", "outdoor_cooling_energy_total"} {
		if _, ok := main.get("daikin/dev1/climateControl/" + suffix + "/state"); ok {
			t.Errorf("%s should not be published when aggregate is 0", suffix)
		}
	}
	// System power (0 W is a valid reading) is still published.
	if got, ok := main.get("daikin/dev1/climateControl/outdoor_power/state"); !ok || got.payload != "0" {
		t.Errorf("outdoor_power = %q (ok=%v), want 0", got.payload, ok)
	}
}

func TestOutdoorEnergyHoldAcrossIdle(t *testing.T) {
	main := newStubMQTT()
	cfg := &config.Config{
		MQTTTopic: "daikin", Language: "de", LocalMode: true, LocalFaikinPrefix: "Faikout",
		LocalDeviceMap: map[string]string{"a": "Klima A", "b": "Klima B"},
	}
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: main, FaikinMQTT: newStubMQTT(),
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "climateControl", "b": "climateControl"}

	// Both report energy → summed system total at the outdoor unit.
	c.publishLocalState(context.Background(), "a",
		&faikin.State{HasAC: true, Power: true, Demand: 100, Energy: 100000})
	c.publishLocalState(context.Background(), "b",
		&faikin.State{HasAC: true, Power: true, Demand: 100, Energy: 50000})
	if got, _ := main.get("daikin/a/climateControl/outdoor_energy_total/state"); got.payload != "150.000" {
		t.Fatalf("outdoor_energy_total = %q, want 150.000 (100+50)", got.payload)
	}

	// b goes idle and stops reporting energy (0). The held 50 kWh must persist,
	// so the total stays 150 — not drop to 100.
	c.publishLocalState(context.Background(), "b",
		&faikin.State{HasAC: true, Power: false, Demand: 100, Energy: 0})
	if got, _ := main.get("daikin/a/climateControl/outdoor_energy_total/state"); got.payload != "150.000" {
		t.Errorf("outdoor_energy_total after b idle = %q, want 150.000 (held, no reset)", got.payload)
	}
}

func TestAggregateEnergySharedVsPerUnit(t *testing.T) {
	cases := []struct {
		name string
		vals []int64
		want int64
	}{
		{"per-unit differ → sum", []int64{778300, 788600, 785500}, 2352400},
		{"shared identical → as-is", []int64{500000, 500000, 500000}, 500000},
		{"one reporter, rest idle → that value", []int64{0, 600000, 0}, 600000},
		{"none reporting → 0", []int64{0, 0, 0}, 0},
		{"two differ, one idle → sum of reporters", []int64{100000, 0, 50000}, 150000},
	}
	for _, tc := range cases {
		if got := aggregateEnergy(tc.vals); got != tc.want {
			t.Errorf("%s: aggregateEnergy(%v) = %d, want %d", tc.name, tc.vals, got, tc.want)
		}
	}
}

func TestApplyFaikinConfigURLs(t *testing.T) {
	c := localReadCoordinator(t, newStubMQTT(), newStubMQTT()) // dev1 -> Klima SZ
	c.lastLocal["dev1"] = &faikin.State{HasAC: true, IPv4: "172.18.9.135", IPv6: "2003::4320"}

	infos := map[string]hass.DeviceInfo{
		"dev1":  {Name: "Schlafzimmer"}, // locally mapped → Faikin URL
		"other": {Name: "Cloud only"},   // not mapped → unchanged
	}
	c.applyFaikinConfigURLs(infos)

	if got := infos["dev1"].ConfigurationURL; got != "http://172.18.9.135/" {
		t.Errorf("dev1 config URL = %q, want http://172.18.9.135/ (Faikin, IPv4 preferred)", got)
	}
	if got := infos["other"].ConfigurationURL; got != "" {
		t.Errorf("unmapped device config URL = %q, want empty (keeps cloud default)", got)
	}
}

func TestLocalPresetMirrorsPowerful(t *testing.T) {
	main := newStubMQTT()
	c := localReadCoordinator(t, newStubMQTT(), main)
	c.climateEmbedded["dev1"] = "climateControl"

	// Powerful on → climate preset must read "boost" (so it stays toggleable).
	c.publishLocalState(context.Background(), "dev1", &faikin.State{HasAC: true, Power: true, Powerful: true})
	if got, _ := main.get("daikin/dev1/climateControl/preset_mode/state"); got.payload != "Boost" {
		t.Errorf("preset with powerful on = %q, want Boost", got.payload)
	}
	if got, _ := main.get("daikin/dev1/climateControl/powerful_mode/state"); got.payload != "on" {
		t.Errorf("powerful switch = %q, want on", got.payload)
	}

	// Powerful off → preset "none".
	c.publishLocalState(context.Background(), "dev1", &faikin.State{HasAC: true, Power: true, Powerful: false})
	if got, _ := main.get("daikin/dev1/climateControl/preset_mode/state"); got.payload != "none" {
		t.Errorf("preset with powerful off = %q, want none", got.payload)
	}
}

func TestDataSource(t *testing.T) {
	c := localReadCoordinator(t, newStubMQTT(), newStubMQTT()) // dev1 -> Klima SZ (local)
	cases := []struct {
		dev, topic, want string
	}{
		{"dev1", "room_temperature", "local"},   // in localTopics
		{"dev1", "energy_total", "local"},       // in localOnlyTopics
		{"dev1", hass.HVACModeTopic, "local"},   // synthetic climate topic
		{"dev1", hass.PresetModeTopic, "local"}, // preset mirrors local powerful
		{"dev1", "error_code", "cloud"},         // cloud-only diagnostic
		{"dev1", "gateway_ip_address", "cloud"}, // cloud-only
		{"other", "room_temperature", "cloud"},  // device not mapped to Faikin
	}
	for _, tc := range cases {
		if got := c.dataSource(tc.dev, tc.topic); got != tc.want {
			t.Errorf("dataSource(%q, %q) = %q, want %q", tc.dev, tc.topic, got, tc.want)
		}
	}
}

func TestClearOrphanConfigs(t *testing.T) {
	m := newStubMQTT()
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: m, Catalog: loadTestCatalog(t),
		HASS:   hass.New("homeassistant", "daikin", "de", m),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	current := `{"unique_id":"daikin_dev1_room_temperature","state_topic":"daikin/dev1/climateControl/room_temperature/state"}`
	orphan := `{"unique_id":"daikin_dev1_old_sensor","state_topic":"daikin/dev1/climateControl/old_sensor/state"}`
	foreign := `{"unique_id":"zigbee2mqtt_x","state_topic":"zigbee2mqtt/x"}`
	retained := map[string][]byte{
		"homeassistant/sensor/daikin_dev1_room_temperature/config": []byte(current),
		"homeassistant/sensor/daikin_dev1_old_sensor/config":       []byte(orphan),
		"homeassistant/sensor/zigbee2mqtt_x/config":                []byte(foreign),
	}
	published := map[string]bool{"homeassistant/sensor/daikin_dev1_room_temperature/config": true}

	if n := c.clearOrphanConfigs(context.Background(), retained, published); n != 1 {
		t.Fatalf("cleared %d, want 1 (only the orphaned daikin config)", n)
	}
	// The orphan was cleared with an empty retained payload.
	if msg, ok := m.get("homeassistant/sensor/daikin_dev1_old_sensor/config"); !ok || msg.payload != "" || !msg.retain {
		t.Errorf("orphan clear = %+v, want empty retained payload", msg)
	}
	// The current and foreign configs are untouched.
	if _, ok := m.get("homeassistant/sensor/zigbee2mqtt_x/config"); ok {
		t.Error("foreign config must never be cleared")
	}
}
