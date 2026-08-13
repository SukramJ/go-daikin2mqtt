// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// fixtureCloud serves a canned device tree: three indoor units on one outdoor
// unit plus a Home Hub gateway (which has no climateControl point and must
// therefore not be offered as a schedulable device).
type fixtureCloud struct{}

func (fixtureCloud) GetDevices(context.Context) (json.RawMessage, error) {
	indoor := func(id, name string) string {
		return `{
      "id": "` + id + `",
      "deviceModel": "dx4",
      "managementPoints": [
        {
          "embeddedId": "climateControl",
          "managementPointType": "climateControl",
          "name": {"value": "` + name + `"},
          "onOffMode": {"value": "on", "settable": true},
          "operationMode": {"value": "heating", "settable": true,
            "values": ["heating", "cooling", "auto", "dry", "fanOnly"]},
          "sensoryData": {"value": {"roomTemperature": {"value": 21.5, "unit": "°C"}}},
          "temperatureControl": {"value": {"operationModes": {"heating": {"setpoints": {
            "roomTemperature": {"value": 21.5, "unit": "°C", "minValue": 16,
              "maxValue": 32, "stepValue": 0.5, "settable": true}}}}}, "settable": true}
        },
        {
          "embeddedId": "outdoorUnit",
          "managementPointType": "outdoorUnit",
          "serialNumber": {"value": "0J723746"},
          "modelInfo": {"value": "3MXM68A"}
        }
      ]
    }`
	}
	doc := "[" + strings.Join([]string{
		indoor("dev-sz", "Schlafzimmer"),
		indoor("dev-wz", "Wohnzimmer"),
		indoor("dev-ga", "Galerie"),
		`{
      "id": "dev-hub",
      "deviceModel": "homehub",
      "managementPoints": [
        {"embeddedId": "gateway", "managementPointType": "gateway",
         "softwareVersion": {"value": "1.2.3"}}
      ]
    }`,
	}, ",") + "]"
	return json.RawMessage(doc), nil
}

func (fixtureCloud) Patch(context.Context, string, string, string, any, string) error { return nil }
func (fixtureCloud) RateLimit() client.RateLimit                                      { return client.RateLimit{} }

// TestManualUIServer serves the real SPA against fixture data so the schedule
// UI can be exercised in a browser. It is skipped unless UI_SERVER is set, so
// it never blocks `make check`.
//
//	UI_SERVER=1 go test ./internal/web/ -run TestManualUIServer -timeout 30m
func TestManualUIServer(t *testing.T) {
	if os.Getenv("UI_SERVER") == "" {
		t.Skip("set UI_SERVER=1 to serve the UI for manual inspection")
	}

	store := schedule.NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	seed := schedule.NewDocument()
	sp := func(f float64) *float64 { return &f }
	bp := func(b bool) *bool { return &b }
	seed.Schedules = []schedule.Schedule{
		{
			ID: "werktag", Name: "Werktag", Enabled: true, Priority: 0,
			Targets: []schedule.Target{{DeviceID: "dev-sz"}, {DeviceID: "dev-wz"}, {DeviceID: "dev-ga"}},
			Blocks: []schedule.Block{
				{
					ID: "b1", Label: "Aufstehen", Days: []string{"mon", "tue", "wed", "thu", "fri"},
					Start: "05:30", End: "08:00",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(21.5)},
				},
				{
					ID: "b2", Label: "Absenkung", Days: []string{"mon", "tue", "wed", "thu", "fri"},
					Start: "08:00", End: "16:30",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(18.5)},
				},
				{
					ID: "b3", Label: "Abend", Days: []string{"mon", "tue", "wed", "thu", "fri"},
					Start: "16:30", End: "22:30",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(21)},
				},
				{
					ID: "b4", Label: "Nacht", Days: []string{"mon", "tue", "wed", "thu", "fri"},
					Start: "22:30", End: "05:30",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(17.5)},
				},
			},
		},
		{
			ID: "homeoffice", Name: "Homeoffice", Enabled: true, Priority: 10,
			Targets: []schedule.Target{{DeviceID: "dev-wz"}},
			Blocks: []schedule.Block{
				{
					ID: "b1", Label: "Schreibtisch", Days: []string{"mon", "wed"},
					Start: "08:00", End: "16:30",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(21.5)},
				},
			},
		},
		{
			ID: "nachtruhe", Name: "Nachtruhe", Type: schedule.TypeOutdoor, Enabled: true, Priority: 0,
			Targets: []schedule.Target{{OutdoorSerial: "0J723746"}},
			Blocks: []schedule.Block{
				{
					ID: "b1", Label: "leise", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
					Start: "22:00", End: "06:00",
					Action: schedule.Action{OutdoorSilent: bp(true), Demand: sp(70)},
				},
			},
		},
		{
			ID: "urlaub", Name: "Urlaub", Enabled: false, Priority: 20,
			Targets: []schedule.Target{{DeviceID: "dev-sz"}, {DeviceID: "dev-wz"}, {DeviceID: "dev-ga"}},
			Blocks: []schedule.Block{
				{
					ID: "b1", Label: "Frostschutz", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
					Start: "00:00", End: "24:00",
					Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: sp(15)},
				},
			},
		},
	}
	if err := store.Save(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng, err := schedule.NewEngine(schedule.Options{
		Store: store, Logger: slog.New(slog.DiscardHandler), Timezone: "Europe/Berlin",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	cat, err := catalog.LoadFile("../../characteristics.yaml")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	lang := os.Getenv("UI_LANG")
	if lang == "" {
		lang = "de"
	}
	srv := New(Deps{
		Cfg:      &config.Config{Language: lang, ScheduleEnable: true, WebEnable: true},
		Client:   fixtureCloud{},
		Catalog:  cat,
		Schedule: eng,
		Groups: func() map[string][]string {
			return map[string][]string{"0J723746": {"dev-sz", "dev-wz", "dev-ga"}}
		},
		Logger: slog.New(slog.DiscardHandler),
	})

	ln, err := net.Listen("tcp", "127.0.0.1:18099")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Log("UI available at http://127.0.0.1:18099/ — press Ctrl-C or wait for the timeout")
	time.Sleep(25 * time.Minute)
	_ = hs.Close()
}
