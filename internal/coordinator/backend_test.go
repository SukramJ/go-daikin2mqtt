// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
)

func TestFaikinControlFor(t *testing.T) {
	cases := []struct {
		char  string
		value any
		want  string // expected control JSON, "" means not controllable
	}{
		{"onOffMode", "on", `{"power":true}`},
		{"onOffMode", "off", `{"power":false}`},
		{"operationMode", "cooling", `{"mode":"cool"}`},
		{"operationMode", "heating", `{"mode":"heat"}`},
		{"operationMode", "fanOnly", `{"mode":"fan"}`},
		{"temperatureControl", 22.5, `{"temp":22.5}`},
		{"powerfulMode", "on", `{"powerful":true}`},
		{"econoMode", "off", `{"econo":false}`},
		{"streamerMode", "on", `{"streamer":true}`},
		{"outdoorSilentMode", "on", `{"quiet":true}`},
		{"demandControl", 80.0, `{"demand":80}`},
		{"operationMode", "bogus", ""}, // unknown mode → not controllable
		{"fanControl", "auto", ""},     // not modelled here → cloud fallback
	}
	for _, tc := range cases {
		ctrl, ok := faikinControlFor(tc.char, tc.value)
		if tc.want == "" {
			if ok {
				t.Errorf("%s=%v: expected not controllable", tc.char, tc.value)
			}
			continue
		}
		if !ok {
			t.Errorf("%s=%v: expected controllable", tc.char, tc.value)
			continue
		}
		raw, _ := ctrl.JSON()
		if string(raw) != tc.want {
			t.Errorf("%s=%v → %s, want %s", tc.char, tc.value, raw, tc.want)
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

	// Mapped device + supported characteristic → local publish, no cloud patch.
	if err := c.setCharacteristic(context.Background(), "dev1", "climateControl", "operationMode", "cooling", ""); err != nil {
		t.Fatal(err)
	}
	msg, ok := faikin.get("Faikout/Klima SZ/command/control")
	if !ok {
		t.Fatalf("expected local command publish; got %v", faikin.published)
	}
	var ctrl map[string]any
	_ = json.Unmarshal([]byte(msg.payload), &ctrl)
	if ctrl["mode"] != "cool" {
		t.Errorf("local control = %s, want mode:cool", msg.payload)
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
