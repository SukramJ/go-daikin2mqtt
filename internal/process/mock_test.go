// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package process

import (
	"os"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// resolveMock loads the real repo catalog and a mock fixture, returning the
// resolved points indexed by topic.
func resolveMock(t *testing.T, fixture string) map[string]Point {
	t.Helper()
	cat, err := catalog.LoadFile("../../characteristics.yaml")
	if err != nil {
		t.Fatalf("LoadFile catalog: %v", err)
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	devices, err := model.ParseDevices(data)
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	byTopic := map[string]Point{}
	points := Resolve(devices, cat)
	for i := range points {
		byTopic[points[i].Topic] = points[i]
	}
	return byTopic
}

// TestResolveAlthermaMock verifies the catalog covers an Altherma air-to-water
// unit (domestic hot water + leaving-water control) using the official mock
// API example response.
func TestResolveAlthermaMock(t *testing.T) {
	pts := resolveMock(t, "testdata/altherma-air-to-water-wlan.json")
	if len(pts) == 0 {
		t.Fatal("no points resolved from altherma fixture")
	}

	if p, ok := pts["dhw_tank_temperature"]; !ok {
		t.Error("dhw_tank_temperature missing")
	} else if f, _ := p.Value.(float64); f <= 0 {
		t.Errorf("dhw_tank_temperature = %v, want a positive temperature", p.Value)
	}

	if p, ok := pts["dhw_temperature_setpoint"]; !ok {
		t.Error("dhw_temperature_setpoint missing")
	} else if !p.Settable {
		t.Error("dhw_temperature_setpoint should be settable")
	}

	// Altherma controls leaving-water temperature rather than room temp.
	if _, ok := pts["leaving_water_setpoint"]; !ok {
		t.Error("leaving_water_setpoint missing for altherma")
	}
	// The room-temperature setpoint should NOT resolve for this unit.
	if _, ok := pts["temperature_setpoint"]; ok {
		t.Error("temperature_setpoint should not resolve for an altherma unit")
	}
}

// TestResolveAirToAirMock verifies the air-to-air (room temperature) path
// against the mock example.
func TestResolveAirToAirMock(t *testing.T) {
	pts := resolveMock(t, "testdata/air-to-air-dx4.json")
	for _, topic := range []string{"power", "operation_mode", "room_temperature", "temperature_setpoint"} {
		if _, ok := pts[topic]; !ok {
			t.Errorf("air-to-air: expected topic %q to resolve", topic)
		}
	}
}

// TestResolveEnergyMock verifies consumptionData is summed into energy
// sensors with kWh units.
func TestResolveEnergyMock(t *testing.T) {
	pts := resolveMock(t, "testdata/air-to-air-dx4.json")
	for _, topic := range []string{"cooling_energy_daily", "cooling_energy_weekly", "heating_energy_daily"} {
		p, ok := pts[topic]
		if !ok {
			t.Errorf("energy: expected topic %q to resolve", topic)
			continue
		}
		if _, isFloat := p.Value.(float64); !isFloat {
			t.Errorf("%s value = %T, want float64", topic, p.Value)
		}
		if p.Unit != "kWh" {
			t.Errorf("%s unit = %q, want kWh", topic, p.Unit)
		}
		if p.Entry.StateClass != "total_increasing" {
			t.Errorf("%s state_class = %q, want total_increasing", topic, p.Entry.StateClass)
		}
	}
}

// TestResolveAirPurifierMock verifies the air purifier select.
func TestResolveAirPurifierMock(t *testing.T) {
	pts := resolveMock(t, "testdata/airpurifier.json")
	p, ok := pts["air_purification_mode"]
	if !ok {
		t.Fatal("air_purification_mode missing")
	}
	if !p.Settable {
		t.Error("air_purification_mode should be settable")
	}
}

// TestResolveHydroInfoMock verifies indoorUnitHydro info sensors (altherma).
func TestResolveHydroInfoMock(t *testing.T) {
	pts := resolveMock(t, "testdata/altherma-air-to-water-wlan.json")
	if _, ok := pts["indoor_hydro_model"]; !ok {
		t.Error("indoor_hydro_model missing for altherma")
	}
}

// TestSumEnergySlice unit-checks the period slicing math.
func TestSumEnergySlice(t *testing.T) {
	// 24-entry daily array: [0..11]=yesterday, [12..23]=today.
	daily := make([]*float64, 24)
	for i := range daily {
		v := float64(i)
		daily[i] = &v
	}
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	got, ok := sumEnergySlice(daily, "daily", now)
	if !ok {
		t.Fatal("daily not ok")
	}
	// sum 12..23 = (12+23)*12/2 = 210
	if got != 210 {
		t.Errorf("daily sum = %v, want 210", got)
	}

	// monthly: index 11+month(=6) = 17 -> value 17 (using same 24-array shape).
	monthly := make([]*float64, 24)
	for i := range monthly {
		v := float64(i)
		monthly[i] = &v
	}
	got, _ = sumEnergySlice(monthly, "monthly", now)
	if got != 17 {
		t.Errorf("monthly = %v, want 17 (index 11+6)", got)
	}
}
