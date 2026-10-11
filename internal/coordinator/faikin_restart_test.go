// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
)

// restartCoordinator builds a coordinator on the shipped characteristics.yaml
// (the restart entry lives there, not in the test catalogue) with local mode
// on (unless cloudOnly) and dev1 mapped to "Klima SZ", returning the cloud and
// both broker stubs so a test can assert where a press went.
func restartCoordinator(t *testing.T, cloudOnly bool) (c *Coordinator, cloud *stubCloud, mainBroker, faikinBroker *stubMQTT) {
	t.Helper()
	cfg := &config.Config{MQTTTopic: "daikin", Language: "en"}
	if !cloudOnly {
		cfg.LocalMode = true
		cfg.LocalFaikinPrefix = "Faikout"
		cfg.LocalDeviceMap = map[string]string{"dev1": "Klima SZ"}
	}
	cloud = &stubCloud{}
	mainBroker, faikinBroker = newStubMQTT(), newStubMQTT()
	c = New(Deps{
		Cfg: cfg, Client: cloud, MQTT: mainBroker, FaikinMQTT: faikinBroker,
		Catalog: loadRealCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.climateEmbedded["dev1"] = "climateControl"
	return c, cloud, mainBroker, faikinBroker
}

func pressRestart(c *Coordinator, deviceID string) {
	c.handleWrite(context.Background(), writeReq{
		deviceID: deviceID, embeddedID: "climateControl", topic: FaikinRestartTopic, payload: "PRESS",
	})
}

// A press on a mapped device's restart button is forwarded to the firmware's
// own restart command — the same topic and payload the firmware's HA discovery
// button uses — and nothing else: no cloud PATCH, no status on the main broker.
func TestFaikinRestartPressForwardsToModule(t *testing.T) {
	c, cloud, main, fk := restartCoordinator(t, false)

	pressRestart(c, "dev1")

	msg, ok := fk.get("command/Klima SZ/restart")
	if !ok {
		t.Fatalf("restart press did not reach command/Klima SZ/restart; faikin publishes: %v", fk.published)
	}
	if msg.payload != faikin.RestartPayload || msg.retain {
		t.Errorf("restart publish = %q retain=%v, want %q retain=false", msg.payload, msg.retain, faikin.RestartPayload)
	}
	if n := fk.count(); n != 1 {
		t.Errorf("faikin publishes = %d, want exactly the restart command", n)
	}
	if n := cloud.patchCount(); n != 0 {
		t.Errorf("cloud patches = %d, want 0: a module restart is not a device setting", n)
	}
	if n := main.count(); n != 0 {
		t.Errorf("main-broker publishes = %d, want 0: a press is never echoed as a status", n)
	}
}

// A device without a Faikin host has no module to restart: the press is
// rejected and in particular NOT sent to the cloud (faikinLocal is not an
// ONECTA characteristic).
func TestFaikinRestartRejectedForUnmappedDevice(t *testing.T) {
	c, cloud, _, fk := restartCoordinator(t, false)

	pressRestart(c, "other")

	if n := fk.count(); n != 0 {
		t.Errorf("faikin publishes = %d, want 0 for an unmapped device", n)
	}
	if n := cloud.patchCount(); n != 0 {
		t.Errorf("cloud patches = %d, want 0", n)
	}
}

// With local mode off there is no Faikin path at all; a stray press (e.g. a
// retained-free replay from an automation) must not become a cloud PATCH.
func TestFaikinRestartRejectedInCloudMode(t *testing.T) {
	c, cloud, _, fk := restartCoordinator(t, true)

	pressRestart(c, "dev1")

	if n := fk.count(); n != 0 {
		t.Errorf("faikin publishes = %d, want 0 with local mode off", n)
	}
	if n := cloud.patchCount(); n != 0 {
		t.Errorf("cloud patches = %d, want 0", n)
	}
}

// The button is synthesized per mapped device from the real catalog as a
// settable button, so Home Assistant gets a discovery config for it although
// the cloud poll never resolves a faikinLocal characteristic.
func TestLocalOnlyPointsSynthesizeFaikinRestart(t *testing.T) {
	c, _, _, _ := restartCoordinator(t, false)

	var found bool
	for _, p := range c.localOnlyPoints([]model.Device{{ID: "dev1"}}, nil) {
		if p.Topic != FaikinRestartTopic {
			continue
		}
		found = true
		if p.Entry.Platform != "button" || !p.Settable || p.Entry.Scope == "outdoor" {
			t.Errorf("faikin_restart point = platform %q settable %v scope %q, want a per-unit settable button",
				p.Entry.Platform, p.Settable, p.Entry.Scope)
		}
	}
	if !found {
		t.Fatal("localOnlyPoints did not synthesize faikin_restart for the mapped device")
	}
	if c.dataSource("dev1", FaikinRestartTopic) != "local" {
		t.Error("faikin_restart data_source should be local for a mapped device")
	}
}
