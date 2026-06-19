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
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// realFaikinState is a verbatim `state/Klima SZ` payload from a live module.
const realFaikinState = `{"online":true,"power":true,"target":22.50,"temp":21.00,"hum":66.00,"outside":28.00,"demand":100,"energy":772600,"mode":"cool","fan":"auto","streamer":false,"quiet":false,"econo":true,"comfort":false,"powerful":false,"swing":"off","preset":"eco"}`

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
	faikinMQTT.handler("state/Klima SZ", []byte(realFaikinState))
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
