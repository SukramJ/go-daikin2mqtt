// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
)

func TestFaikinCommand(t *testing.T) {
	cases := []struct {
		char            string
		value           any
		suffix, payload string // "" suffix means not controllable
	}{
		{"onOffMode", "on", "power", "1"},
		{"onOffMode", "off", "power", "0"},
		{"operationMode", "cooling", "mode", "C"},
		{"operationMode", "heating", "mode", "H"},
		{"operationMode", "fanOnly", "mode", "F"},
		{"temperatureControl", 22.5, "temp", "22.5"},
		{"powerfulMode", "on", "powerful", "1"},
		{"econoMode", "off", "econo", "0"},
		{"streamerMode", "on", "streamer", "1"},
		{"outdoorSilentMode", "on", "quiet", "1"},
		{"demandControl", 80.0, "demand", "80"},
		{"operationMode", "bogus", "", ""}, // unknown mode → not controllable
		{"fanControl", "auto", "", ""},     // not modelled → cloud fallback
	}
	for _, tc := range cases {
		suffix, payload, ok := faikinCommand(tc.char, tc.value)
		if tc.suffix == "" {
			if ok {
				t.Errorf("%s=%v: expected not controllable", tc.char, tc.value)
			}
			continue
		}
		if !ok || suffix != tc.suffix || payload != tc.payload {
			t.Errorf("%s=%v → command/%s %q (ok=%v), want command/%s %q",
				tc.char, tc.value, suffix, payload, ok, tc.suffix, tc.payload)
		}
	}
}

// localCoordinator builds a coordinator with local mode on and one mapped device.
func localCoordinator(cloud *stubCloud, cloudMQTT, faikinMQTT *stubMQTT) *Coordinator {
	cfg := &config.Config{
		MQTTTopic:         "daikin",
		Language:          "de",
		LocalMode:         true,
		LocalFaikinPrefix: "Faikout",
		LocalDeviceMap:    map[string]string{"dev1": "Klima SZ"},
	}
	return New(Deps{
		Cfg: cfg, Client: cloud, MQTT: cloudMQTT, FaikinMQTT: faikinMQTT,
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
}

func TestSetCharacteristicRoutesLocal(t *testing.T) {
	cloud := &stubCloud{}
	faikin := newStubMQTT()
	c := localCoordinator(cloud, newStubMQTT(), faikin)

	// Mapped device + outdoor silent → dedicated command/quiet topic, "1"/"0".
	if err := c.setCharacteristic(context.Background(), "dev1", "climateControl", "outdoorSilentMode", "on", ""); err != nil {
		t.Fatal(err)
	}
	msg, ok := faikin.get("Faikout/Klima SZ/command/quiet")
	if !ok {
		t.Fatalf("expected publish to command/quiet; got %v", faikin.published)
	}
	if msg.payload != "1" {
		t.Errorf("quiet payload = %q, want \"1\"", msg.payload)
	}
	if cloud.patchCount() != 0 {
		t.Errorf("cloud should not be patched in local mode, got %d", cloud.patchCount())
	}
}

func TestSetCharacteristicFallsBackToCloud(t *testing.T) {
	cloud := &stubCloud{}
	faikin := newStubMQTT()
	c := localCoordinator(cloud, newStubMQTT(), faikin)

	// Unsupported characteristic on a mapped device → cloud fallback.
	if err := c.setCharacteristic(context.Background(), "dev1", "climateControl", "fanControl", "auto", "/path"); err != nil {
		t.Fatal(err)
	}
	if cloud.patchCount() != 1 {
		t.Fatalf("expected cloud fallback patch, got %d", cloud.patchCount())
	}
	if len(faikin.published) != 0 {
		t.Errorf("no local publish expected for unsupported char, got %v", faikin.published)
	}

	// Unmapped device → cloud even for a supported characteristic.
	if err := c.setCharacteristic(context.Background(), "other", "climateControl", "onOffMode", "on", ""); err != nil {
		t.Fatal(err)
	}
	if cloud.patchCount() != 2 {
		t.Errorf("unmapped device should patch cloud, got %d", cloud.patchCount())
	}
}

func TestSetCharacteristicCloudWhenLocalDisabled(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT()) // local mode off, no FaikinMQTT
	if err := c.setCharacteristic(context.Background(), "dev1", "climateControl", "onOffMode", "on", ""); err != nil {
		t.Fatal(err)
	}
	if cloud.patchCount() != 1 {
		t.Errorf("expected cloud patch when local disabled, got %d", cloud.patchCount())
	}
}
