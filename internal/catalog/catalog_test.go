// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package catalog

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const sampleYAML = `
- match:
    managementPointType: climateControl
    characteristic: onOffMode
  topic: power
  name: Power
  name_de: Betrieb
  platform: switch
  settable: true
  values:
    - {value: "on", label: "On", label_de: "An"}
    - {value: "off", label: "Off", label_de: "Aus"}
- match:
    managementPointType: climateControl
    characteristic: operationMode
  topic: operation_mode
  name: Operation mode
  platform: select
  settable: true
  values:
    - {value: heating, label: Heating, label_de: Heizen}
    - {value: cooling, label: Cooling}
- match:
    managementPointType: climateControl
    characteristic: roomTemperature
  topic: room_temperature
  name: Room temperature
  name_de: Raumtemperatur
  platform: sensor
  device_class: temperature
  unit: "°C"
  state_class: measurement
`

func mustLoad(t *testing.T, doc string) *Catalog {
	t.Helper()
	c, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	return c
}

func TestLoadAndMatch(t *testing.T) {
	c := mustLoad(t, sampleYAML)

	if got := len(c.Entries()); got != 3 {
		t.Fatalf("Entries(): got %d, want 3", got)
	}

	e, ok := c.Match("climateControl", "onOffMode")
	if !ok {
		t.Fatal("Match(climateControl/onOffMode): not found")
	}
	if e.Topic != "power" || e.Platform != "switch" || !e.Settable {
		t.Errorf("matched wrong entry: %+v", e)
	}

	if _, ok := c.Match("climateControl", "doesNotExist"); ok {
		t.Error("Match: expected miss for unknown characteristic")
	}
	if _, ok := c.Match("gateway", "onOffMode"); ok {
		t.Error("Match: expected miss for unknown management-point type")
	}
}

func TestEntriesIsCopy(t *testing.T) {
	c := mustLoad(t, sampleYAML)
	es := c.Entries()
	es[0].Topic = "mutated"
	if e, _ := c.Match("climateControl", "onOffMode"); e.Topic != "power" {
		t.Errorf("Entries() must return a copy, internal topic changed to %q", e.Topic)
	}
}

func TestIsEnabled(t *testing.T) {
	tru, fls := true, false
	cases := map[string]struct {
		e    Entry
		want bool
	}{
		"nil default true": {Entry{}, true},
		"explicit true":    {Entry{Enabled: &tru}, true},
		"explicit false":   {Entry{Enabled: &fls}, false},
	}
	for name, tc := range cases {
		if got := tc.e.IsEnabled(); got != tc.want {
			t.Errorf("%s: IsEnabled() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestLocalizedName(t *testing.T) {
	c := mustLoad(t, sampleYAML)
	power, _ := c.Match("climateControl", "onOffMode")
	opMode, _ := c.Match("climateControl", "operationMode")

	if got := power.LocalizedName("de"); got != "Betrieb" {
		t.Errorf("LocalizedName(de) with German set: got %q, want %q", got, "Betrieb")
	}
	if got := power.LocalizedName("en"); got != "Power" {
		t.Errorf("LocalizedName(en): got %q, want %q", got, "Power")
	}
	// German not set -> fall back to English.
	if got := opMode.LocalizedName("de"); got != "Operation mode" {
		t.Errorf("LocalizedName(de) without German: got %q, want English fallback", got)
	}
	// Unknown language -> English.
	if got := power.LocalizedName("fr"); got != "Power" {
		t.Errorf("LocalizedName(fr): got %q, want English fallback", got)
	}
}

func TestLocalizedLabel(t *testing.T) {
	c := mustLoad(t, sampleYAML)
	opMode, _ := c.Match("climateControl", "operationMode")

	if got := opMode.LocalizedLabel("heating", "de"); got != "Heizen" {
		t.Errorf("LocalizedLabel(heating, de): got %q, want %q", got, "Heizen")
	}
	if got := opMode.LocalizedLabel("heating", "en"); got != "Heating" {
		t.Errorf("LocalizedLabel(heating, en): got %q, want %q", got, "Heating")
	}
	// German label missing -> English fallback.
	if got := opMode.LocalizedLabel("cooling", "de"); got != "Cooling" {
		t.Errorf("LocalizedLabel(cooling, de) without German: got %q, want %q", got, "Cooling")
	}
	// Unknown raw value -> the raw value itself.
	if got := opMode.LocalizedLabel("mystery", "de"); got != "mystery" {
		t.Errorf("LocalizedLabel(mystery): got %q, want raw value fallback", got)
	}
}

func TestCodeForLabel(t *testing.T) {
	c := mustLoad(t, sampleYAML)
	opMode, _ := c.Match("climateControl", "operationMode")

	cases := map[string]struct {
		in     string
		want   string
		wantOK bool
	}{
		"english label":    {"Heating", "heating", true},
		"german label":     {"Heizen", "heating", true},
		"raw value":        {"cooling", "cooling", true},
		"case insensitive": {"hEaTiNg", "heating", true},
		"unknown":          {"verylike", "", false},
		"empty":            {"", "", false},
	}
	for name, tc := range cases {
		got, ok := opMode.CodeForLabel(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: CodeForLabel(%q) = (%q, %v), want (%q, %v)",
				name, tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestLoadDuplicateTopic(t *testing.T) {
	doc := `
- match: {managementPointType: climateControl, characteristic: onOffMode}
  topic: power
  platform: switch
- match: {managementPointType: climateControl, characteristic: powerfulMode}
  topic: power
  platform: switch
`
	_, err := Load(strings.NewReader(doc))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load: got %v, want *ValidationError", err)
	}
	if !containsSubstr(verr.Issues, "duplicate topic") {
		t.Errorf("issues do not mention duplicate topic: %v", verr.Issues)
	}
}

func TestLoadAllowsSamePairDifferentSubValues(t *testing.T) {
	// Two entries on the same (mpType, characteristic) reading different
	// nested sub-values are allowed, distinguished by value_path/topic.
	doc := `
- match: {managementPointType: climateControl, characteristic: sensoryData}
  topic: room_temperature
  value_path: roomTemperature
  platform: sensor
- match: {managementPointType: climateControl, characteristic: sensoryData}
  topic: outdoor_temperature
  value_path: outdoorTemperature
  platform: sensor
`
	c, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.EntriesForType("climateControl"); len(got) != 2 {
		t.Fatalf("EntriesForType = %d entries, want 2", len(got))
	}
	if _, ok := c.ByTopic("outdoor_temperature"); !ok {
		t.Error("ByTopic(outdoor_temperature) not found")
	}
}

func TestLoadInvalidPlatform(t *testing.T) {
	doc := `
- match: {managementPointType: climateControl, characteristic: foo}
  topic: foo
  platform: gauge
`
	_, err := Load(strings.NewReader(doc))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load: got %v, want *ValidationError", err)
	}
	if !containsSubstr(verr.Issues, "platform") {
		t.Errorf("issues do not mention platform: %v", verr.Issues)
	}
}

func TestLoadMissingRequiredFields(t *testing.T) {
	doc := `
- match: {managementPointType: "", characteristic: ""}
  topic: ""
  platform: ""
`
	_, err := Load(strings.NewReader(doc))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load: got %v, want *ValidationError", err)
	}
	if len(verr.Issues) < 4 {
		t.Errorf("expected an issue per missing required field, got %d: %v",
			len(verr.Issues), verr.Issues)
	}
}

func TestLoadSelectWithoutValues(t *testing.T) {
	doc := `
- match: {managementPointType: climateControl, characteristic: foo}
  topic: foo
  platform: select
`
	_, err := Load(strings.NewReader(doc))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Load: got %v, want *ValidationError", err)
	}
	if !containsSubstr(verr.Issues, "select requires") {
		t.Errorf("issues do not mention select values requirement: %v", verr.Issues)
	}
}

func TestLoadEmptyDocument(t *testing.T) {
	c, err := Load(strings.NewReader("[]"))
	if err != nil {
		t.Fatalf("Load([]): unexpected error: %v", err)
	}
	if len(c.Entries()) != 0 {
		t.Errorf("expected empty catalog, got %d entries", len(c.Entries()))
	}
}

// TestLoadRepoCatalog loads the real characteristics.yaml from the repo root
// and verifies it is valid and that key entries resolve.
func TestLoadRepoCatalog(t *testing.T) {
	path := filepath.Join("..", "..", "characteristics.yaml")
	c, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", path, err)
	}
	if len(c.Entries()) == 0 {
		t.Fatal("repo catalog is empty")
	}

	power, ok := c.Match("climateControl", "onOffMode")
	if !ok {
		t.Fatal("repo catalog: climateControl/onOffMode not found")
	}
	if power.Platform != "switch" {
		t.Errorf("repo catalog onOffMode platform: got %q, want switch", power.Platform)
	}

	if _, ok := c.Match("climateControl", "operationMode"); !ok {
		t.Error("repo catalog: climateControl/operationMode not found")
	}
	// Room temperature is a nested sub-value of sensoryData; it is exposed
	// under the room_temperature topic.
	if _, ok := c.ByTopic("room_temperature"); !ok {
		t.Error("repo catalog: room_temperature topic not found")
	}
}

func containsSubstr(issues []string, want string) bool {
	for _, s := range issues {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
