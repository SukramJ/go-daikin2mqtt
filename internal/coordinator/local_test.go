// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
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
