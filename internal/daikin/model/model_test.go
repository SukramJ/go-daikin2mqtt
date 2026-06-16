// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func findMP(d Device, mpType string) (ManagementPoint, bool) {
	for _, mp := range d.ManagementPoints {
		if mp.Type == mpType {
			return mp, true
		}
	}
	return ManagementPoint{}, false
}

func TestParseDevices_FixedFanMode(t *testing.T) {
	devices, err := ParseDevices(loadFixture(t, "climate_fixedfanmode.json"))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}

	dev := devices[0]
	if dev.ID != "6f944461-08cb-4fee-979c-710ff66cea77" {
		t.Errorf("device ID = %q", dev.ID)
	}
	if dev.Model != "dx4" {
		t.Errorf("device Model = %q, want dx4", dev.Model)
	}
	if len(dev.Raw) == 0 {
		t.Error("device Raw is empty")
	}

	cc, ok := findMP(dev, "climateControl")
	if !ok {
		t.Fatal("no climateControl management point")
	}
	if cc.EmbeddedID != "climateControl" {
		t.Errorf("climateControl EmbeddedID = %q", cc.EmbeddedID)
	}
	if cc.Category != "primary" {
		t.Errorf("climateControl Category = %q, want primary", cc.Category)
	}

	// Reserved descriptive keys must not appear as characteristics.
	for _, k := range []string{"embeddedId", "managementPointType", "managementPointCategory"} {
		if _, exists := cc.Characteristics[k]; exists {
			t.Errorf("reserved key %q exposed as characteristic", k)
		}
	}

	// onOffMode is a settable string enum with value "off".
	onOff, ok := cc.Characteristics["onOffMode"]
	if !ok {
		t.Fatal("no onOffMode characteristic")
	}
	if !onOff.Settable {
		t.Error("onOffMode should be settable")
	}
	if s, ok := onOff.String(); !ok || s != "off" {
		t.Errorf("onOffMode value = %q, %v; want off, true", s, ok)
	}
	if len(onOff.Values) != 2 {
		t.Errorf("onOffMode Values = %v, want 2 entries", onOff.Values)
	}
	if onOff.IsObject() {
		t.Error("onOffMode should not be an object")
	}

	// operationMode value should be "heating".
	op := cc.Characteristics["operationMode"]
	if s, ok := op.String(); !ok || s != "heating" {
		t.Errorf("operationMode = %q, %v; want heating", s, ok)
	}

	// sensoryData is a nested object; roomTemperature lives inside it.
	sensory, ok := cc.Characteristics["sensoryData"]
	if !ok {
		t.Fatal("no sensoryData characteristic")
	}
	if !sensory.IsObject() {
		t.Error("sensoryData should be an object")
	}
	if _, ok := sensory.String(); ok {
		t.Error("sensoryData should not decode as scalar string")
	}

	var sd struct {
		RoomTemperature struct {
			Value float64 `json:"value"`
		} `json:"roomTemperature"`
	}
	if err := json.Unmarshal(sensory.Value, &sd); err != nil {
		t.Fatalf("decode sensoryData.value: %v", err)
	}
	if sd.RoomTemperature.Value != 19 {
		t.Errorf("roomTemperature = %v, want 19", sd.RoomTemperature.Value)
	}
}

func TestParseDevices_Altherma_MultiDeviceAndHotWater(t *testing.T) {
	devices, err := ParseDevices(loadFixture(t, "altherma.json"))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devices) != 5 {
		t.Fatalf("expected 5 devices, got %d", len(devices))
	}

	dev := devices[0]
	if dev.ID != "1ece521b-5401-4a42-acce-6f76fba246aa" {
		t.Errorf("device[0] ID = %q", dev.ID)
	}
	if dev.Model != "Altherma" {
		t.Errorf("device[0] Model = %q, want Altherma", dev.Model)
	}

	// Hot water tank: onOffMode "on" plus a nested temperatureControl.
	dhw, ok := findMP(dev, "domesticHotWaterTank")
	if !ok {
		t.Fatal("no domesticHotWaterTank management point")
	}
	onOff := dhw.Characteristics["onOffMode"]
	if s, ok := onOff.String(); !ok || s != "on" {
		t.Errorf("dhw onOffMode = %q, %v; want on", s, ok)
	}
	tc := dhw.Characteristics["temperatureControl"]
	if !tc.Settable {
		t.Error("temperatureControl should be settable")
	}
	if !tc.IsObject() {
		t.Error("temperatureControl should be an object")
	}

	// climateControl sensoryData carries roomTemperature with min/max/step
	// on its nested entry; the wrapper itself is an object.
	cc, ok := findMP(dev, "climateControl")
	if !ok {
		t.Fatal("no climateControl management point")
	}
	sensory := cc.Characteristics["sensoryData"]
	var sd struct {
		RoomTemperature struct {
			Value    float64  `json:"value"`
			MinValue *float64 `json:"minValue"`
			MaxValue *float64 `json:"maxValue"`
		} `json:"roomTemperature"`
	}
	if err := json.Unmarshal(sensory.Value, &sd); err != nil {
		t.Fatalf("decode sensoryData: %v", err)
	}
	if sd.RoomTemperature.Value != 21 {
		t.Errorf("roomTemperature = %v, want 21", sd.RoomTemperature.Value)
	}
}

func TestDataPoints_Flattening(t *testing.T) {
	devices, err := ParseDevices(loadFixture(t, "gas.json"))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	dev := devices[0]

	dps := dev.DataPoints()

	// Count characteristics directly and compare with the flattened total.
	var want int
	for _, mp := range dev.ManagementPoints {
		want += len(mp.Characteristics)
	}
	if want == 0 {
		t.Fatal("device has no characteristics")
	}
	if len(dps) != want {
		t.Errorf("DataPoints len = %d, want %d", len(dps), want)
	}

	// Every data point must reference the owning device and carry a name.
	var foundOnOff bool
	for _, dp := range dps {
		if dp.DeviceID != dev.ID {
			t.Errorf("data point DeviceID = %q, want %q", dp.DeviceID, dev.ID)
		}
		if dp.Name == "" {
			t.Error("data point has empty Name")
		}
		if dp.MPType == "climateControl" && dp.Name == "onOffMode" {
			foundOnOff = true
			if s, ok := dp.Characteristic.String(); !ok || s != "on" {
				t.Errorf("climateControl onOffMode = %q, %v; want on", s, ok)
			}
		}
	}
	if !foundOnOff {
		t.Error("did not find climateControl onOffMode data point")
	}
}

func TestAccessors(t *testing.T) {
	mk := func(v string) Characteristic {
		return Characteristic{Value: json.RawMessage(v)}
	}

	// String.
	if s, ok := mk(`"hello"`).String(); !ok || s != "hello" {
		t.Errorf("String = %q, %v", s, ok)
	}
	if _, ok := mk(`42`).String(); ok {
		t.Error("String should fail on number")
	}

	// Float.
	if f, ok := mk(`21.5`).Float(); !ok || f != 21.5 {
		t.Errorf("Float = %v, %v", f, ok)
	}
	if _, ok := mk(`"21.5"`).Float(); ok {
		t.Error("Float should fail on string")
	}

	// Bool.
	if b, ok := mk(`true`).Bool(); !ok || !b {
		t.Errorf("Bool = %v, %v", b, ok)
	}
	if _, ok := mk(`"true"`).Bool(); ok {
		t.Error("Bool should fail on string")
	}

	// IsObject for object, array and scalar.
	if !mk(`{"a":1}`).IsObject() {
		t.Error("object should be IsObject")
	}
	if !mk(`[1,2]`).IsObject() {
		t.Error("array should be IsObject")
	}
	if mk(`"x"`).IsObject() {
		t.Error("string should not be IsObject")
	}
	if mk(`  {"a":1}`).IsObject() != true {
		t.Error("leading whitespace before object should still be IsObject")
	}

	// Empty value: all accessors report failure.
	empty := Characteristic{}
	if _, ok := empty.String(); ok {
		t.Error("empty String should fail")
	}
	if _, ok := empty.Float(); ok {
		t.Error("empty Float should fail")
	}
	if _, ok := empty.Bool(); ok {
		t.Error("empty Bool should fail")
	}
	if empty.IsObject() {
		t.Error("empty should not be IsObject")
	}
}

func TestParseDevices_InvalidJSON(t *testing.T) {
	if _, err := ParseDevices(json.RawMessage(`{"not":"an array"}`)); err == nil {
		t.Error("expected error for non-array JSON")
	}
	if _, err := ParseDevices(json.RawMessage(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseDevices_EmptyArray(t *testing.T) {
	devices, err := ParseDevices(json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devices))
	}
}
