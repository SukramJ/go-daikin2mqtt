// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
)

const fanControlJSON = `{"operationModes":{"cooling":{` +
	`"fanSpeed":{"currentMode":{"value":"quiet","values":["auto","quiet","fixed"]},"modes":{"fixed":{"minValue":1,"maxValue":5,"value":3}}},` +
	`"fanDirection":{"vertical":{"currentMode":{"value":"stop","values":["stop","swing","windNice"]}},"horizontal":{"currentMode":{"value":"stop","values":["stop","swing"]}}}` +
	`}}}`

func climateMP() model.ManagementPoint {
	return model.ManagementPoint{
		Type:       "climateControl",
		EmbeddedID: "climateControl",
		Characteristics: map[string]model.Characteristic{
			"operationMode": {Value: json.RawMessage(`"cooling"`)},
			"fanControl":    {Value: json.RawMessage(fanControlJSON)},
			"powerfulMode":  {Value: json.RawMessage(`"off"`)},
		},
	}
}

func TestParseClimateAux(t *testing.T) {
	a := parseClimateAux(climateMP(), "cooling")
	if want := []string{"auto", "quiet", "1", "2", "3", "4", "5"}; !reflect.DeepEqual(a.fanModes, want) {
		t.Errorf("fanModes = %v, want %v", a.fanModes, want)
	}
	if a.fanMode != "quiet" {
		t.Errorf("fanMode = %q, want quiet", a.fanMode)
	}
	if want := []string{"stop", "swing", "windnice"}; !reflect.DeepEqual(a.swingModes, want) {
		t.Errorf("swingModes = %v, want %v", a.swingModes, want)
	}
	if want := []string{"stop", "swing"}; !reflect.DeepEqual(a.swingHModes, want) {
		t.Errorf("swingHModes = %v, want %v", a.swingHModes, want)
	}
	// "none" is implicit in HA and must not be advertised.
	if want := []string{"boost"}; !reflect.DeepEqual(a.presetModes, want) {
		t.Errorf("presetModes = %v, want %v", a.presetModes, want)
	}
	if a.preset != "none" {
		t.Errorf("preset = %q, want none", a.preset)
	}
}

// TestClimateAuxInfoGerman verifies the discovery option lists carry the
// German labels while numeric fan speeds stay raw.
func TestClimateAuxInfoGerman(t *testing.T) {
	ci := parseClimateAux(climateMP(), "cooling").info("de")
	if want := []string{"Automatik", "Leise", "1", "2", "3", "4", "5"}; !reflect.DeepEqual(ci.FanModes, want) {
		t.Errorf("FanModes = %v, want %v", ci.FanModes, want)
	}
	if want := []string{"Aus", "Schwenken", "Sanfter Luftstrom"}; !reflect.DeepEqual(ci.SwingModes, want) {
		t.Errorf("SwingModes = %v, want %v", ci.SwingModes, want)
	}
	if want := []string{"Aus", "Schwenken"}; !reflect.DeepEqual(ci.SwingHorizontalModes, want) {
		t.Errorf("SwingHorizontalModes = %v, want %v", ci.SwingHorizontalModes, want)
	}
	if want := []string{"Boost"}; !reflect.DeepEqual(ci.PresetModes, want) {
		t.Errorf("PresetModes = %v, want %v", ci.PresetModes, want)
	}
}

// TestClimateAuxInfoEnglishRaw verifies non-German keeps the raw values so the
// command values stay language-neutral.
func TestClimateAuxInfoEnglishRaw(t *testing.T) {
	ci := parseClimateAux(climateMP(), "cooling").info("en")
	if want := []string{"stop", "swing", "windnice"}; !reflect.DeepEqual(ci.SwingModes, want) {
		t.Errorf("SwingModes = %v, want raw %v", ci.SwingModes, want)
	}
	if want := []string{"auto", "quiet", "1", "2", "3", "4", "5"}; !reflect.DeepEqual(ci.FanModes, want) {
		t.Errorf("FanModes = %v, want raw %v", ci.FanModes, want)
	}
}

// TestHandleAuxWriteGermanLabels verifies German dropdown labels reverse-map
// back to the raw Daikin values on the write path (Language=de in testConfig).
func TestHandleAuxWriteGermanLabels(t *testing.T) {
	t.Run("fan", func(t *testing.T) {
		cloud := &stubCloud{}
		c := newCoordinator(t, cloud, newStubMQTT())
		c.updateModeCache([]model.Device{{ID: "dev1", ManagementPoints: []model.ManagementPoint{climateMP()}}})
		c.handleFanModeWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "fan_mode", payload: "Leise"})
		p := cloud.lastPatch(t)
		if p.path != "/operationModes/cooling/fanSpeed/currentMode" || p.value != "quiet" {
			t.Errorf("patch = %+v, want currentMode=quiet", p)
		}
	})
	t.Run("swing", func(t *testing.T) {
		cloud := &stubCloud{}
		c := newCoordinator(t, cloud, newStubMQTT())
		c.updateModeCache([]model.Device{{ID: "dev1", ManagementPoints: []model.ManagementPoint{climateMP()}}})
		c.handleSwingWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "swing_mode", payload: "Sanfter Luftstrom"}, "vertical")
		if p := cloud.lastPatch(t); p.value != "windNice" {
			t.Errorf("patch value = %v, want windNice", p.value)
		}
	})
	t.Run("preset", func(t *testing.T) {
		cloud := &stubCloud{}
		c := newCoordinator(t, cloud, newStubMQTT())
		c.handlePresetWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "preset_mode", payload: "Boost"})
		p := cloud.lastPatch(t)
		if p.characteristic != "powerfulMode" || p.value != "on" {
			t.Errorf("patch = %+v, want powerfulMode=on", p)
		}
	})
}

func TestParseClimateAuxFixedFanMode(t *testing.T) {
	mp := climateMP()
	// Switch currentMode to "fixed" → fanMode should reflect the numeric value.
	mp.Characteristics["fanControl"] = model.Characteristic{Value: json.RawMessage(
		`{"operationModes":{"cooling":{"fanSpeed":{"currentMode":{"value":"fixed","values":["auto","quiet","fixed"]},"modes":{"fixed":{"minValue":1,"maxValue":5,"value":4}}}}}}`,
	)}
	a := parseClimateAux(mp, "cooling")
	if a.fanMode != "4" {
		t.Errorf("fanMode = %q, want 4 (fixed value)", a.fanMode)
	}
}

func TestHandleFanModeWriteNumeric(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.updateModeCache([]model.Device{{ID: "dev1", ManagementPoints: []model.ManagementPoint{climateMP()}}})

	c.handleFanModeWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "fan_mode", payload: "3"})
	if cloud.patchCount() != 2 {
		t.Fatalf("patch count = %d, want 2 (currentMode=fixed + modes/fixed)", cloud.patchCount())
	}
	first, second := cloud.patches[0], cloud.patches[1]
	if first.characteristic != "fanControl" || first.path != "/operationModes/cooling/fanSpeed/currentMode" || first.value != "fixed" {
		t.Errorf("first patch = %+v", first)
	}
	if second.path != "/operationModes/cooling/fanSpeed/modes/fixed" || second.value != 3 {
		t.Errorf("second patch = %+v, want fixed=3", second)
	}
}

func TestHandleFanModeWriteNamed(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.updateModeCache([]model.Device{{ID: "dev1", ManagementPoints: []model.ManagementPoint{climateMP()}}})

	c.handleFanModeWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "fan_mode", payload: "quiet"})
	if cloud.patchCount() != 1 {
		t.Fatalf("patch count = %d, want 1", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.characteristic != "fanControl" || p.path != "/operationModes/cooling/fanSpeed/currentMode" || p.value != "quiet" {
		t.Errorf("patch = %+v", p)
	}
}

func TestHandleSwingWriteWindNice(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.updateModeCache([]model.Device{{ID: "dev1", ManagementPoints: []model.ManagementPoint{climateMP()}}})

	c.handleSwingWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "swing_mode", payload: "windnice"}, "vertical")
	p := cloud.lastPatch(t)
	if p.path != "/operationModes/cooling/fanDirection/vertical/currentMode" || p.value != "windNice" {
		t.Errorf("patch = %+v, want vertical currentMode=windNice", p)
	}
}

func TestHandlePresetWrite(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.handlePresetWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "preset_mode", payload: "boost"})
	p := cloud.lastPatch(t)
	if p.characteristic != "powerfulMode" || p.value != "on" || p.path != "" {
		t.Errorf("patch = %+v, want powerfulMode=on", p)
	}
}

// TestHandleHVACModeWriteHeat verifies the combined climate hvac-mode command
// turns the unit on and sets the mapped operationMode (two PATCHes).
func TestHandleHVACModeWriteHeat(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())

	c.handleHVACModeWrite(context.Background(), writeReq{
		deviceID: "dev1", embeddedID: "climateControl", topic: "hvac_mode", payload: "heat",
	})

	if cloud.patchCount() != 2 {
		t.Fatalf("patch count = %d, want 2 (onOffMode + operationMode)", cloud.patchCount())
	}
	got := map[string]any{}
	for _, p := range cloud.patches {
		if p.characteristic == "" || p.path != "" {
			t.Errorf("unexpected patch %+v (want top-level characteristic, no path)", p)
		}
		got[p.characteristic] = p.value
	}
	if got["onOffMode"] != "on" {
		t.Errorf("onOffMode = %v, want on", got["onOffMode"])
	}
	if got["operationMode"] != "heating" {
		t.Errorf("operationMode = %v, want heating", got["operationMode"])
	}
}

// TestHandleHVACModeWriteOff verifies "off" maps to a single onOffMode=off.
func TestHandleHVACModeWriteOff(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())

	c.handleHVACModeWrite(context.Background(), writeReq{
		deviceID: "dev1", embeddedID: "climateControl", topic: "hvac_mode", payload: "off",
	})

	if cloud.patchCount() != 1 {
		t.Fatalf("patch count = %d, want 1", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.characteristic != "onOffMode" || p.value != "off" {
		t.Errorf("patch = %+v, want onOffMode=off", p)
	}
}

// TestHandleHVACModeWriteUnknown verifies an unknown mode is rejected.
func TestHandleHVACModeWriteUnknown(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())

	c.handleHVACModeWrite(context.Background(), writeReq{
		deviceID: "dev1", embeddedID: "climateControl", topic: "hvac_mode", payload: "bogus",
	})
	if cloud.patchCount() != 0 {
		t.Errorf("patch count = %d, want 0 for unknown mode", cloud.patchCount())
	}
}

func TestCloudFanFaikinMapping(t *testing.T) {
	fwd := map[string]string{"auto": "A", "quiet": "Q", "1": "1", "3": "3", "5": "5"}
	for cloud, want := range fwd {
		if got, ok := cloudFanToFaikin(cloud); !ok || got != want {
			t.Errorf("cloudFanToFaikin(%q) = %q,%v want %q", cloud, got, ok, want)
		}
	}
	if _, ok := cloudFanToFaikin("windnice"); ok {
		t.Error("unmappable fan value should return ok=false (cloud fallback)")
	}
	rev := map[string]string{"auto": "auto", "low": "1", "medium": "3", "high": "5", "night": "quiet"}
	for fa, want := range rev {
		if got := faikinFanToCloud[fa]; got != want {
			t.Errorf("faikinFanToCloud[%q] = %q want %q", fa, got, want)
		}
	}
}

func TestFaikinSwingMapping(t *testing.T) {
	axes := []struct{ s, v, h string }{
		{"off", "stop", "stop"},
		{"V", "swing", "stop"},
		{"H", "stop", "swing"},
		{"H+V", "swing", "swing"},
		{"C", "windnice", "stop"},
	}
	for _, c := range axes {
		if v, h := faikinSwingAxes(c.s); v != c.v || h != c.h {
			t.Errorf("faikinSwingAxes(%q) = %q,%q want %q,%q", c.s, v, h, c.v, c.h)
		}
	}
	comb := []struct{ v, h, want string }{
		{"stop", "stop", "off"},
		{"swing", "stop", "V"},
		{"stop", "swing", "H"},
		{"swing", "swing", "H+V"},
		{"windnice", "stop", "C"},
		{"windnice", "swing", "C"},
	}
	for _, c := range comb {
		if got := faikinSwingCombine(c.v, c.h); got != c.want {
			t.Errorf("faikinSwingCombine(%q,%q) = %q want %q", c.v, c.h, got, c.want)
		}
	}
}

func TestHandleFanModeWriteLocal(t *testing.T) {
	cloud := &stubCloud{}
	fk := newStubMQTT()
	c := localCoordinator(cloud, newStubMQTT(), fk) // dev1 -> Klima SZ
	c.handleFanModeWrite(context.Background(), writeReq{
		deviceID: "dev1", embeddedID: "climateControl", topic: "fan_mode", payload: "3",
	})
	if msg, ok := fk.get("command/Klima SZ/fan"); !ok || msg.payload != "3" {
		t.Errorf("expected command/Klima SZ/fan=3, got %v", fk.published)
	}
	if cloud.patchCount() != 0 {
		t.Errorf("cloud should not be patched in local mode, got %d", cloud.patchCount())
	}
}

func TestHandleSwingWriteLocalCombines(t *testing.T) {
	cloud := &stubCloud{}
	fk := newStubMQTT()
	c := localCoordinator(cloud, newStubMQTT(), fk)
	// Horizontal swing currently on; setting vertical swing must combine to H+V.
	c.lastLocal = map[string]*faikin.State{"dev1": {HasAC: true, Swing: "H"}}
	c.handleSwingWrite(context.Background(), writeReq{
		deviceID: "dev1", embeddedID: "climateControl", topic: "swing_mode", payload: "swing",
	}, "vertical")
	if msg, ok := fk.get("command/Klima SZ/swing"); !ok || msg.payload != "H+V" {
		t.Errorf("expected command/Klima SZ/swing=H+V, got %v", fk.published)
	}
}
