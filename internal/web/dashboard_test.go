// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-daikin2mqtt/internal/coordinator"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/state"
)

// fakeBridge records what the dashboard queues.
type fakeBridge struct {
	submitted []string
	err       error
}

func (f *fakeBridge) Submit(deviceID, embeddedID, topic, payload string) error {
	f.submitted = append(f.submitted, deviceID+"|"+embeddedID+"|"+topic+"|"+payload)
	return f.err
}

func (f *fakeBridge) Upstream() (cloudOK, devicesKnown bool, lastPoll time.Time) {
	return true, true, time.Now()
}

func dashboardDeps(t *testing.T) (Deps, *fakeBridge) {
	t.Helper()
	store := state.New(time.Now())
	minT, maxT, step := 16.0, 30.0, 0.5
	store.SetPoints(map[string]string{"dev1": "BRP069C4x"}, map[string][]state.Point{"dev1": {
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "power", Characteristic: "onOffMode", Name: "Power", NameDE: "Ein/Aus", Platform: "switch", Settable: true},
		{
			EmbeddedID: "climateControl", MPType: "climateControl", Topic: "operation_mode", Characteristic: "operationMode", Name: "Mode", Platform: "select", Settable: true,
			Options: []state.Option{{Value: "heating", Label: "Heat", LabelDE: "Heizen"}, {Value: "cooling", Label: "Cool"}},
		},
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "temperature_setpoint", Characteristic: "temperatureControl", Platform: "number", Unit: "°C", Min: &minT, Max: &maxT, Step: &step, Settable: true},
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "room_temperature", Characteristic: "sensoryData", Platform: "sensor", Unit: "°C"},
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "energy_today", Characteristic: "consumptionData", Platform: "sensor", Unit: "kWh"},
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "error_state", Characteristic: "isInErrorState", Platform: "binary_sensor", Category: "diagnostic"},
		{EmbeddedID: "climateControl", MPType: "climateControl", Topic: "refresh", Characteristic: "daemonRefresh", Platform: "button", Settable: true},
		{EmbeddedID: "gateway", MPType: "gateway", Topic: "ip_address", Characteristic: "ipAddress", Platform: "sensor"},
	}})
	now := time.Now()
	store.Observe("dev1", "climateControl", "power", true, now)
	store.Observe("dev1", "climateControl", "room_temperature", 21.5, now)
	store.Observe("dev1", "climateControl", "error_state", true, now)
	store.Observe("dev1", "", "online", true, now)
	fb := &fakeBridge{}
	d := baseDeps()
	d.State, d.Bridge = store, fb
	d.Client = &fakeClient{rl: client.RateLimit{LimitDay: 200, RemainingDay: 12, LimitMinute: 20, RemainingMinute: 20, Updated: now}}
	return d, fb
}

func TestDashboardSnapshot(t *testing.T) {
	d, _ := dashboardDeps(t)
	ts := newTestServer(d)
	defer ts.Close()

	var snap bridgeui.Snapshot
	getJSONInto(t, ts.URL+"/api/snapshot", &snap)
	if len(snap.Devices) != 1 || snap.Devices[0].Title != "BRP069C4x" || snap.Devices[0].Online == nil || !*snap.Devices[0].Online {
		t.Fatalf("devices = %+v", snap.Devices)
	}
	groups := map[string]bridgeui.Entry{}
	for _, e := range snap.Devices[0].Entries {
		groups[e.Key] = e
		if e.Key == "climateControl/refresh" {
			t.Error("a button is shown as a value")
		}
	}
	for key, want := range map[string]string{
		"climateControl/power":            groupClimate,
		"climateControl/energy_today":     groupEnergy,
		"climateControl/error_state":      groupDiagnostic,
		"gateway/ip_address":              groupDiagnostic,
		"climateControl/room_temperature": groupClimate,
	} {
		if groups[key].Group != want {
			t.Errorf("%s in %q, want %q", key, groups[key].Group, want)
		}
	}
	// Language de: the German name.
	if e := groups["climateControl/power"]; e.Name != "Ein/Aus" || e.Value != true {
		t.Errorf("power = %+v", e)
	}
	if groups["climateControl/error_state"].Tone != "err" {
		t.Error("a raised error state is not flagged")
	}
}

func TestDashboardItems(t *testing.T) {
	d, _ := dashboardDeps(t)
	ts := newTestServer(d)
	defer ts.Close()

	var items []bridgeui.Item
	getJSONInto(t, ts.URL+"/api/items", &items)
	byKey := map[string]bridgeui.Item{}
	for _, it := range items {
		byKey[it.Key] = it
	}
	if len(items) != 4 {
		t.Errorf("items = %+v", items)
	}
	if it := byKey["climateControl/operation_mode"]; it.Component != "select" || len(it.Options) != 2 ||
		it.Options[0].Label != "Heizen" || it.Options[1].Label != "Cool" {
		t.Errorf("mode = %+v", it)
	}
	if it := byKey["climateControl/temperature_setpoint"]; it.Min == nil || *it.Min != 16 || it.Step == nil || *it.Step != 0.5 {
		t.Errorf("setpoint = %+v", it)
	}
	if it := byKey["climateControl/refresh"]; it.Component != "button" || it.Payload == "" {
		t.Errorf("refresh = %+v", it)
	}
}

func TestDashboardWrite(t *testing.T) {
	d, fb := dashboardDeps(t)
	ts := newTestServer(d)
	defer ts.Close()

	write := func(body string) int {
		t.Helper()
		res, err := http.Post(ts.URL+"/api/write", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := write(`{"device":"dev1","key":"climateControl/temperature_setpoint","value":21.5}`); code != http.StatusOK {
		t.Errorf("write -> %d", code)
	}
	if len(fb.submitted) != 1 || fb.submitted[0] != "dev1|climateControl|temperature_setpoint|21.5" {
		t.Errorf("submitted %v", fb.submitted)
	}
	for body, want := range map[string]int{
		`{"device":"nope","key":"climateControl/power","value":true}`:         http.StatusNotFound,
		`{"device":"dev1","key":"climateControl/room_temperature","value":1}`: http.StatusBadRequest,
		`{"device":"dev1","key":"power","value":true}`:                        http.StatusBadRequest,
	} {
		if code := write(body); code != want {
			t.Errorf("%s -> %d, want %d", body, code, want)
		}
	}
	fb.err = coordinator.ErrWriteQueueFull
	if code := write(`{"device":"dev1","key":"climateControl/power","value":false}`); code != http.StatusServiceUnavailable {
		t.Errorf("full queue -> %d, want 503", code)
	}
}

func TestDashboardHealthAndLayout(t *testing.T) {
	d, _ := dashboardDeps(t)
	d.Cfg.ScheduleEnable = true
	ts := newTestServer(d)
	defer ts.Close()

	var h bridgeui.Health
	getJSONInto(t, ts.URL+"/api/health", &h)
	labels := map[string]bridgeui.Tile{}
	for _, tile := range h.Tiles {
		labels[tile.Label] = tile
	}
	if h.Status != "ok" || labels["health.auth"].Text != "val.ok" || labels["health.rateDay"].Value != "12 / 200" || labels["health.rateDay"].Tone != "warn" {
		t.Errorf("health = %+v", h)
	}

	var ui struct {
		Layout bridgeui.Layout `json:"layout"`
	}
	getJSONInto(t, ts.URL+"/api/ui", &ui)
	hrefs := make([]string, 0, len(ui.Layout.Links))
	for _, l := range ui.Layout.Links {
		hrefs = append(hrefs, l.Href)
	}
	if strings.Join(hrefs, " ") != "tools.html#sec-auth tools.html#sec-devices tools.html#sec-schedules tools.html#sec-patch" {
		t.Errorf("links = %v", hrefs)
	}
}

// Every label the dashboard adds exists in both languages.
func TestDashboardBundlesMatch(t *testing.T) {
	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		t.Fatal(err)
	}
	for k := range tr["de"] {
		if _, ok := tr["en"][k]; !ok {
			t.Errorf("en lacks %s", k)
		}
	}
	for k := range tr["en"] {
		if _, ok := tr["de"][k]; !ok {
			t.Errorf("de lacks %s", k)
		}
	}
}

func getJSONInto(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s -> %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
