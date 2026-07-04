// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// --- stubs -----------------------------------------------------------------

// patchCall records a single Patch invocation on stubCloud.
type patchCall struct {
	deviceID, embeddedID, characteristic, path string
	value                                      any
}

// stubCloud implements CloudClient. GetDevices returns canned JSON (or an
// injected error); Patch records every call's arguments.
type stubCloud struct {
	devices json.RawMessage
	getErr  error

	mu      sync.Mutex
	patches []patchCall
}

func (s *stubCloud) GetDevices(_ context.Context) (json.RawMessage, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.devices, nil
}

func (s *stubCloud) Patch(_ context.Context, deviceID, embeddedID, characteristic string, value any, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patches = append(s.patches, patchCall{
		deviceID:       deviceID,
		embeddedID:     embeddedID,
		characteristic: characteristic,
		value:          value,
		path:           path,
	})
	return nil
}

func (s *stubCloud) lastPatch(t *testing.T) patchCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.patches) == 0 {
		t.Fatalf("expected a Patch call, got none")
	}
	return s.patches[len(s.patches)-1]
}

func (s *stubCloud) patchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.patches)
}

// allPatches returns a copy of the recorded patches (race-safe under -race).
func (s *stubCloud) allPatches() []patchCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]patchCall(nil), s.patches...)
}

// publishedMsg is a recorded MQTT publish.
type publishedMsg struct {
	payload string
	retain  bool
}

// stubMQTT implements mqtt.Client. Publish stores topic->payload; Subscribe
// captures the handler so tests can simulate inbound /set messages.
type stubMQTT struct {
	mu        sync.Mutex
	published map[string]publishedMsg
	handler   mqtt.MessageHandler
	filter    string
}

func newStubMQTT() *stubMQTT {
	return &stubMQTT{published: map[string]publishedMsg{}}
}

func (m *stubMQTT) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published[topic] = publishedMsg{payload: string(payload), retain: retain}
	return nil
}

func (m *stubMQTT) Subscribe(_ context.Context, topicFilter string, _ mqtt.QoS, handler mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filter = topicFilter
	m.handler = handler
	return mqtt.SubscribeResult{}, nil
}

func (m *stubMQTT) Unsubscribe(_ context.Context, _ string) error { return nil }

func (m *stubMQTT) get(topic string) (publishedMsg, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.published[topic]
	return v, ok
}

func (m *stubMQTT) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.published)
}

// --- fixtures --------------------------------------------------------------

// testCatalogYAML mirrors the curated catalog shape for the entries the
// coordinator tests exercise.
const testCatalogYAML = `
- match: {managementPointType: climateControl, characteristic: onOffMode}
  topic: power
  name: Power
  platform: switch
  settable: true
  values:
    - {value: "on", label: "On"}
    - {value: "off", label: "Off"}
- match: {managementPointType: climateControl, characteristic: operationMode}
  topic: operation_mode
  name: Operation Mode
  platform: select
  settable: true
  values:
    - {value: heating, label: Heating, label_de: Heizen}
    - {value: cooling, label: Cooling, label_de: "Kühlen"}
- match: {managementPointType: climateControl, characteristic: sensoryData}
  topic: room_temperature
  name: Room Temperature
  platform: sensor
  value_path: roomTemperature
  unit: "°C"
  precision: 1
- match: {managementPointType: climateControl, characteristic: temperatureControl}
  topic: temperature_setpoint
  name: Temperature Setpoint
  platform: number
  settable: true
  value_path: operationModes/{mode}/setpoints/roomTemperature
  path: /operationModes/{mode}/setpoints/roomTemperature
  precision: 1
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: energy_total, name: Energy total, platform: sensor, unit: kWh, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: heating_energy_total, name: Heating energy total, platform: sensor, unit: kWh, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: cooling_energy_total, name: Cooling energy total, platform: sensor, unit: kWh, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_energy_total, name: Energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_heating_energy_total, name: Heating energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_cooling_energy_total, name: Cooling energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_power, name: Power (system), platform: sensor, unit: W, scope: outdoor, precision: 0}
`

func loadTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load(strings.NewReader(testCatalogYAML))
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	return cat
}

func testConfig() *config.Config {
	return &config.Config{
		MQTTTopic:            "daikin",
		Language:             "de",
		RefreshDayInterval:   600,
		RefreshNightInterval: 1800,
		DayStartHour:         7,
		DayEndHour:           22,
	}
}

// fixedClock returns a clock pinned at a stable day-time instant.
func fixedClock() func() time.Time {
	t := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// devicesJSON builds a one-device gateway-devices payload with a single
// climateControl management point carrying the characteristics the tests use.
func devicesJSON(deviceID, embeddedID string) json.RawMessage {
	doc := `[
      {
        "id": "` + deviceID + `",
        "deviceModel": "test-model",
        "managementPoints": [
          {
            "embeddedId": "` + embeddedID + `",
            "managementPointType": "climateControl",
            "onOffMode": {"value": "on", "settable": true},
            "operationMode": {"value": "cooling", "settable": true,
              "values": ["heating", "cooling"]},
            "sensoryData": {"value": {
              "roomTemperature": {"value": 20, "unit": "°C"}
            }},
            "temperatureControl": {"value": {
              "operationModes": {
                "cooling": {"setpoints": {
                  "roomTemperature": {"value": 22.5, "unit": "°C",
                    "minValue": 16, "maxValue": 32, "stepValue": 0.5,
                    "settable": true}
                }}
              }
            }, "settable": true}
          }
        ]
      }
    ]`
	return json.RawMessage(doc)
}

func newCoordinator(t *testing.T, cloud *stubCloud, m *stubMQTT) *Coordinator {
	t.Helper()
	return New(Deps{
		Cfg:     testConfig(),
		Client:  cloud,
		MQTT:    m,
		Catalog: loadTestCatalog(t),
		HASS:    nil,
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   fixedClock(),
	})
}

// --- tests -----------------------------------------------------------------

//  1. pollOnce publishes the resolved state topics, including the localized
//     select label (German) which is the core of the localization check.
func TestPollOncePublishesStateTopics(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.pollOnce(context.Background())

	cases := []struct {
		suffix string
		want   string
	}{
		{"power/state", "on"},
		{"room_temperature/state", "20.0"},
		{"temperature_setpoint/state", "22.5"},
		{"operation_mode/state", "Kühlen"}, // localized label (Language=de)
	}
	for _, tc := range cases {
		topic := "daikin/" + dev + "/" + emb + "/" + tc.suffix
		got, ok := m.get(topic)
		if !ok {
			t.Fatalf("missing publish for %q", topic)
		}
		if got.payload != tc.want {
			t.Errorf("%s = %q, want %q", topic, got.payload, tc.want)
		}
		if !got.retain {
			t.Errorf("%s: expected retained publish", topic)
		}
	}
}

// 2. PublishOnline marks the bridge available (retained).
func TestPublishOnline(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.PublishOnline(context.Background())

	got, ok := m.get("daikin/bridge/status")
	if !ok {
		t.Fatalf("missing bridge status publish")
	}
	if got.payload != "online" {
		t.Errorf("payload = %q, want %q", got.payload, "online")
	}
	if !got.retain {
		t.Errorf("bridge status should be retained")
	}
}

//  3. Write path for a nested, mode-scoped setpoint: {mode} is substituted
//     from the mode cache and the value is coerced to float64.
func TestHandleWriteNestedSetpoint(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	devices, err := model.ParseDevices(cloud.devices)
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	c.updateModeCache(devices)

	c.handleWrite(context.Background(), writeReq{
		deviceID:   dev,
		embeddedID: emb,
		topic:      "temperature_setpoint",
		payload:    "21.5",
	})

	p := cloud.lastPatch(t)
	if p.deviceID != dev || p.embeddedID != emb {
		t.Errorf("device/embedded = %q/%q, want %q/%q", p.deviceID, p.embeddedID, dev, emb)
	}
	if p.characteristic != "temperatureControl" {
		t.Errorf("characteristic = %q, want %q", p.characteristic, "temperatureControl")
	}
	if p.path != "/operationModes/cooling/setpoints/roomTemperature" {
		t.Errorf("path = %q, want substituted mode path", p.path)
	}
	f, ok := p.value.(float64)
	if !ok {
		t.Fatalf("value type = %T, want float64", p.value)
	}
	if f != 21.5 {
		t.Errorf("value = %v, want 21.5", f)
	}
}

//  4. Write path for a select: a German label is reverse-mapped to the raw
//     API code via the catalog (CodeForLabel).
func TestHandleWriteSelectLabelReverseMap(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.handleWrite(context.Background(), writeReq{
		deviceID:   dev,
		embeddedID: emb,
		topic:      "operation_mode",
		payload:    "Heizen", // German label
	})

	p := cloud.lastPatch(t)
	if p.characteristic != "operationMode" {
		t.Errorf("characteristic = %q, want %q", p.characteristic, "operationMode")
	}
	if s, ok := p.value.(string); !ok || s != "heating" {
		t.Errorf("value = %#v, want \"heating\"", p.value)
	}
}

// 5. Write to an unknown topic must not produce a Patch.
func TestHandleWriteUnknownTopic(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.handleWrite(context.Background(), writeReq{
		deviceID:   "dev1",
		embeddedID: "climateControl",
		topic:      "does_not_exist",
		payload:    "whatever",
	})

	if n := cloud.patchCount(); n != 0 {
		t.Errorf("patch count = %d, want 0 for unknown topic", n)
	}
}

// parseSetTopic accepts well-formed /set topics and rejects malformed ones.
func TestParseSetTopic(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	req, ok := c.parseSetTopic("daikin/dev1/climateControl/power/set", "on")
	if !ok {
		t.Fatalf("expected valid topic to parse")
	}
	if req.deviceID != "dev1" || req.embeddedID != "climateControl" ||
		req.topic != "power" || req.payload != "on" {
		t.Errorf("parsed req = %+v", req)
	}

	bad := []string{
		"daikin/dev1/climateControl/power/state", // not /set
		"daikin/dev1/climateControl/power",       // too few segments
		"daikin/dev1/climateControl/power/x/set", // too many segments
		"other/dev1/climateControl/power/set",    // wrong root
	}
	for _, topic := range bad {
		if _, ok := c.parseSetTopic(topic, "v"); ok {
			t.Errorf("topic %q unexpectedly parsed", topic)
		}
	}
}

//  7. pollOnce handles the known transient/auth errors gracefully: no panic
//     and no state publishes (only logging).
func TestPollOnceErrorHandling(t *testing.T) {
	for name, err := range map[string]error{
		"scan_ignore":     client.ErrScanIgnore,
		"rate_limited":    client.ErrRateLimited,
		"reauth_required": auth.ErrReauthRequired,
	} {
		t.Run(name, func(t *testing.T) {
			cloud := &stubCloud{getErr: err}
			m := newStubMQTT()
			c := newCoordinator(t, cloud, m)

			c.pollOnce(context.Background())

			if n := m.count(); n != 0 {
				t.Errorf("published %d topics, want 0 on error", n)
			}
		})
	}
}
