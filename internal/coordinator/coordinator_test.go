// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
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
	// patchErr, when set, is what every Patch returns after recording it.
	patchErr error

	mu      sync.Mutex
	patches []patchCall
	gets    int
}

func (s *stubCloud) GetDevices(_ context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.devices, nil
}

// getCount returns how often GetDevices was called (race-safe under -race).
func (s *stubCloud) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
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
	return s.patchErr
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
	// counts is how often each topic was published to, not just what it last
	// carried. A retained plane that is republished with the same bytes is
	// indistinguishable in `published`, and F13 is exactly a question about
	// whether a republish happens at all.
	counts  map[string]int
	handler mqtt.MessageHandler
	filter  string
}

func newStubMQTT() *stubMQTT {
	return &stubMQTT{published: map[string]publishedMsg{}, counts: map[string]int{}}
}

func (m *stubMQTT) Publish(_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published[topic] = publishedMsg{payload: string(payload), retain: retain}
	m.counts[topic]++
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

// countOf returns how often topic was published to.
func (m *stubMQTT) countOf(topic string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[topic]
}

// val returns the `val` of the status object last published to topic.
func (m *stubMQTT) val(t *testing.T, topic string) (any, bool) {
	t.Helper()
	msg, ok := m.get(topic)
	if !ok {
		return nil, false
	}
	return statusVal(t, msg.payload), true
}

// statusVal decodes an mqtt-smarthome 2.0 status object and returns its `val`,
// failing the test for a payload that is not one — `val`, `ts` and `lc` all
// present, `lc` not after `ts`.
func statusVal(t *testing.T, payload string) any {
	t.Helper()
	var obj struct {
		Val any    `json:"val"`
		TS  *int64 `json:"ts"`
		LC  *int64 `json:"lc"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil || obj.TS == nil || obj.LC == nil {
		t.Fatalf("payload %q is not a status object", payload)
	}
	if *obj.LC > *obj.TS {
		t.Errorf("payload %q: lc after ts", payload)
	}
	return obj.Val
}

func (m *stubMQTT) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.published)
}

// flakyPub is an mqtt.Publisher whose Publish fails while fail is set.
type flakyPub struct {
	*stubMQTT
	fmu sync.Mutex
	// failPrefix refuses every publish under it, which is what lets a test put
	// the daemon in the one state the migration cannot survive on its own: the
	// per-entity retractions applied, the device document refused.
	failPrefix string
}

func (f *flakyPub) setFailPrefix(p string) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.failPrefix = p
}

func (f *flakyPub) Publish(ctx context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, opts ...mqtt.PublishOption) error {
	f.fmu.Lock()
	prefix := f.failPrefix
	f.fmu.Unlock()
	if prefix != "" && strings.HasPrefix(topic, prefix) {
		return errors.New("broker down")
	}
	return f.stubMQTT.Publish(ctx, topic, payload, qos, retain, opts...)
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
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: fan_speed, name: Fan speed, platform: sensor, unit: rpm, precision: 0}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_energy_total, name: Energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_heating_energy_total, name: Heating energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_cooling_energy_total, name: Cooling energy total (system), platform: sensor, unit: kWh, scope: outdoor, precision: 3}
- {match: {managementPointType: climateControl, characteristic: faikinLocal}, topic: outdoor_power, name: Power (system), platform: sensor, unit: W, scope: outdoor, precision: 0}
- {match: {managementPointType: climateControl, characteristic: daemonRefresh}, topic: refresh, name: Refresh from cloud, platform: button, icon: mdi:cloud-refresh, scope: outdoor, settable: true}
- match: {managementPointType: climateControl, characteristic: daemonSchedule}
  topic: schedule_state
  name: Active schedule
  name_de: Aktiver Zeitplan
  platform: sensor
  category: diagnostic
  values:
    - {value: idle, label: No block, label_de: Kein Block}
- {match: {managementPointType: climateControl, characteristic: daemonScheduleNext}, topic: schedule_next_change, name: Next schedule change, name_de: Nächste Zeitplan-Schaltung, platform: sensor, device_class: timestamp, category: diagnostic}
- match: {managementPointType: climateControl, characteristic: daemonOutdoorSchedule}
  topic: outdoor_schedule_state
  name: Active outdoor schedule
  name_de: Aktiver Außengerät-Zeitplan
  platform: sensor
  category: diagnostic
  scope: outdoor
  values:
    - {value: idle, label: No block, label_de: Kein Block}
- {match: {managementPointType: climateControl, characteristic: daemonOutdoorScheduleNext}, topic: outdoor_schedule_next_change, name: Next outdoor schedule change, platform: sensor, device_class: timestamp, category: diagnostic, scope: outdoor}
- {match: {managementPointType: climateControl, characteristic: powerfulMode}, topic: powerful_mode, name: Powerful mode, platform: switch, settable: true}
- {match: {managementPointType: climateControl, characteristic: streamerMode}, topic: streamer, name: Streamer, platform: switch, settable: true}
- {match: {managementPointType: climateControl, characteristic: outdoorSilentMode}, topic: outdoor_silent, name: Outdoor silent, platform: switch, settable: true, scope: outdoor}
- {match: {managementPointType: climateControl, characteristic: econoMode}, topic: econo_mode, name: Econo mode, platform: switch, settable: true, scope: outdoor}
- {match: {managementPointType: climateControl, characteristic: demandControl}, topic: demand_control, name: Demand limit, platform: number, settable: true, scope: outdoor}
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

//  1. pollOnce publishes the resolved status items as mqtt-smarthome 2.0
//     status objects: power as a JSON boolean, numbers as JSON numbers, and the
//     select as its API token — not the localized label (Language=de), which
//     lives in discovery since 0.14.
func TestPollOncePublishesStateTopics(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.pollOnce(context.Background())

	cases := []struct {
		leaf string
		want any
	}{
		{"power", true},
		{"room_temperature", 20.0},
		{"temperature_setpoint", 22.5},
		{"operation_mode", "cooling"},
	}
	for _, tc := range cases {
		topic := "daikin/status/" + dev + "/" + emb + "/" + tc.leaf
		got, ok := m.val(t, topic)
		if !ok {
			t.Fatalf("missing publish for %q", topic)
		}
		if got != tc.want {
			t.Errorf("%s val = %#v, want %#v", topic, got, tc.want)
		}
		if msg, _ := m.get(topic); !msg.retain {
			t.Errorf("%s: expected retained publish", topic)
		}
	}
	// The device's reachability, from the ONECTA document (absent → up).
	if got, ok := m.val(t, "daikin/status/"+dev+"/online"); !ok || got != true {
		t.Errorf("online = %v (published %v), want true", got, ok)
	}
	// Exact bytes once: the key order and the integer milliseconds the spec
	// asks for, from the injected clock.
	ms := fixedClock()().UnixMilli()
	if msg, _ := m.get("daikin/status/" + dev + "/" + emb + "/power"); msg.payload !=
		`{"val":true,"ts":`+strconv.FormatInt(ms, 10)+`,"lc":`+strconv.FormatInt(ms, 10)+`}` {
		t.Errorf("power payload = %s", msg.payload)
	}
}

// 2. PublishOnline announces `<name>/connected` (retained) at 1 until the
// upstream has proved usable, and `<name>/info` when an instance is wired.
func TestPublishOnline(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.PublishOnline(context.Background())

	got, ok := m.get("daikin/connected")
	if !ok {
		t.Fatalf("missing connected publish")
	}
	if got.payload != "1" {
		t.Errorf("payload = %q, want %q", got.payload, "1")
	}
	if !got.retain {
		t.Errorf("connected should be retained")
	}
	if _, ok := m.get("daikin/bridge/status"); ok {
		t.Error("the 0.13 bridge/status topic is still published")
	}
}

// updateModeCache ownership: for a locally-mapped device the cloud snapshot
// only bootstraps missing cache entries — fresher local/write values must not
// be overwritten by a lagging poll (a stale "on" would let the mode sync
// write to, and on Faikin thereby wake, a unit just switched off).
func TestUpdateModeCacheLocalOwnership(t *testing.T) {
	c := localCoordinator(&stubCloud{}, newStubMQTT(), newStubMQTT()) // maps dev1

	mp := func(power, mode string) model.ManagementPoint {
		return model.ManagementPoint{
			Type:       "climateControl",
			EmbeddedID: "cc",
			Characteristics: map[string]model.Characteristic{
				"onOffMode":     {Value: json.RawMessage(`"` + power + `"`)},
				"operationMode": {Value: json.RawMessage(`"` + mode + `"`)},
			},
		}
	}
	devices := []model.Device{
		{ID: "dev1", ManagementPoints: []model.ManagementPoint{mp("on", "heating")}},  // mapped locally
		{ID: "other", ManagementPoints: []model.ManagementPoint{mp("on", "heating")}}, // cloud-only
	}

	// First poll: nothing cached yet → bootstrap fills the mapped device too.
	c.updateModeCache(devices)
	if on, known := c.powerState("dev1"); !known || !on {
		t.Fatalf("bootstrap powerState(dev1) = %v (known=%v), want on", on, known)
	}

	// The local feed turned dev1 off/cooling; the cloud still reports on/heating.
	c.mu.Lock()
	c.powerCache["dev1"] = false
	c.modeCache["dev1/cc"] = "cooling"
	c.powerCache["other"] = false
	c.mu.Unlock()
	c.updateModeCache(devices)

	if on, _ := c.powerState("dev1"); on {
		t.Errorf("stale cloud poll must not overwrite the local power state")
	}
	if m, _ := c.cachedMode("dev1", "cc"); m != "cooling" {
		t.Errorf("stale cloud poll must not overwrite the local mode, got %q", m)
	}
	if on, _ := c.powerState("other"); !on {
		t.Errorf("cloud-only device must keep following the poll")
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

// Non-finite numeric payloads (NaN/Inf parse fine via ParseFloat) must be
// rejected before reaching the cloud or the local firmware.
func TestHandleWriteRejectsNonFiniteNumbers(t *testing.T) {
	for _, payload := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
		t.Run(payload, func(t *testing.T) {
			cloud := &stubCloud{}
			m := newStubMQTT()
			c := newCoordinator(t, cloud, m)

			c.handleWrite(context.Background(), writeReq{
				deviceID:   "dev1",
				embeddedID: "climateControl",
				topic:      "temperature_setpoint",
				payload:    payload,
			})

			if n := cloud.patchCount(); n != 0 {
				t.Errorf("patch count = %d, want 0 for payload %q", n, payload)
			}
		})
	}
}

// Retained /set messages are stale commands the broker replays on every
// (re)subscribe; they must be dropped instead of re-applied.
func TestSubscribeWritesDropsRetained(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)
	if err := c.subscribeWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.commands.Stop(context.Background()) })

	// A retained /set is a stale command the broker replays on every
	// (re)subscribe; applying it would re-write hardware state on every
	// reconnect. The hand-written handler checked the flag; the router drops it
	// before a handler runs (publisher.CommandConfig.DeliverRetained). The
	// assertion is unchanged because the behaviour must be.
	m.handler(&mqtt.Message{Topic: "daikin/set/dev1/climateControl/power", Payload: []byte("on"), Retain: true})
	c.commands.WaitIdle()
	select {
	case req := <-c.writes:
		t.Fatalf("retained /set was queued: %+v", req)
	default:
	}

	// A live (non-retained) command still goes through.
	m.handler(&mqtt.Message{Topic: "daikin/set/dev1/climateControl/power", Payload: []byte("on"), Retain: false})
	c.commands.WaitIdle()
	select {
	case <-c.writes:
	default:
		t.Fatal("live /set command was not queued")
	}
}

// parseSetTopic takes the device, management point and leaf from the three
// `+` levels of the set route, and refuses anything that did not fill them.
func TestParseSetTopic(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	req, ok := c.parseSetTopic(publisher.Command{
		Topic: "daikin/set/dev1/climateControl/power", Wildcards: []string{"dev1", "climateControl", "power"},
	})
	if !ok {
		t.Fatalf("expected valid topic to parse")
	}
	if req.deviceID != "dev1" || req.embeddedID != "climateControl" || req.topic != "power" {
		t.Errorf("parsed req = %+v", req)
	}
	if got := c.setTopic(req); got != "daikin/set/dev1/climateControl/power" {
		t.Errorf("setTopic = %q", got)
	}

	for _, w := range [][]string{nil, {"dev1", "climateControl"}, {"a", "b", "c", "d"}} {
		if _, ok := c.parseSetTopic(publisher.Command{Topic: "daikin/set/x", Wildcards: w}); ok {
			t.Errorf("wildcards %v unexpectedly parsed", w)
		}
	}
}

// A failed discovery publish must not commit the signature — the next poll
// retries instead of permanently suppressing discovery.
func TestDiscoveryRetriedAfterPublishFailure(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	flaky := &flakyPub{stubMQTT: m, failPrefix: "homeassistant/device/"}
	c := New(Deps{
		Cfg:     testConfig(),
		Client:  cloud,
		MQTT:    flaky,
		Catalog: loadTestCatalog(t),
		HASS:    hass.New("homeassistant", "daikin", "en", flaky),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   fixedClock(),
	})
	// The default window is 2s of listening for retained documents the stub
	// broker never delivers; shrink it on this coordinator, as the other
	// coordinator tests do.
	c.collectWindow = 20 * time.Millisecond

	c.pollOnce(context.Background())
	c.mu.Lock()
	sig := c.lastDiscSig
	c.mu.Unlock()
	if sig != "" {
		t.Fatalf("lastDiscSig committed despite publish failure: %q", sig)
	}

	flaky.setFailPrefix("")
	c.pollOnce(context.Background())
	c.mu.Lock()
	sig = c.lastDiscSig
	c.mu.Unlock()
	if sig == "" {
		t.Fatal("lastDiscSig not committed after successful publish")
	}
	if flaky.count() == 0 {
		t.Fatal("no discovery configs published after recovery")
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

// TestDataSourceAttributesAreRepublishedEveryPoll pins F13 of the ADR 0070
// phase 8 measurement.
//
// publishDataSources used to run inside maybePublishDiscovery, AFTER the
// `changed` gate. The data_source attribute reports which path serves a device
// — cloud or local Faikin — and switching paths does not change the point set,
// so it does not change the discovery signature, so the attribute was never
// republished. It could be stale for as long as the point set was stable,
// which is normally forever.
//
// The assertion is on the SECOND poll, the one whose discovery signature has
// not moved: that is the poll the gate used to swallow.
func TestDataSourceAttributesAreRepublishedEveryPoll(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	cfg := testConfig()
	c := New(Deps{
		Cfg:     cfg,
		Client:  cloud,
		MQTT:    m,
		Catalog: loadTestCatalog(t),
		HASS:    hass.New("homeassistant", "daikin", "en", m),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   fixedClock(),
	})
	// The default window is 2s of listening for retained documents the stub
	// broker never delivers; shrink it on this coordinator, as the other
	// coordinator tests do.
	c.collectWindow = 20 * time.Millisecond

	const attrs = "daikin/status/dev1/climateControl/power/attributes"

	c.pollOnce(context.Background())
	if got := m.countOf(attrs); got != 1 {
		var cfgs []string
		for topic := range m.published {
			if strings.HasPrefix(topic, "homeassistant/") {
				cfgs = append(cfgs, topic)
			}
		}
		sort.Strings(cfgs)
		t.Fatalf("after poll 1, %s published %d times, want 1; configs seen: %v", attrs, got, cfgs)
	}
	// Pick a config topic that was actually published, so the gate assertion
	// below is about the gate rather than about this test's guess at a name.
	cfgTopic := ""
	for topic := range m.published {
		if strings.HasPrefix(topic, "homeassistant/") && strings.HasSuffix(topic, "/config") {
			cfgTopic = topic
			break
		}
	}
	if cfgTopic == "" {
		t.Fatal("no discovery config published")
	}

	c.pollOnce(context.Background())

	// The discovery signature has not moved, so the retained config is NOT
	// republished — that gate is correct and stays.
	if got := m.countOf(cfgTopic); got != 1 {
		t.Errorf("after poll 2, %s published %d times, want 1 — "+
			"the discovery gate should still suppress an unchanged config", cfgTopic, got)
	}
	// The attributes document is OFFERED every poll — that is F13's fix, and it
	// is why the call sits outside the discovery-signature gate. What reaches
	// the broker is a separate question, and since step 5 the answer is the
	// state plane's dedup gate: an unchanged retained value is compared, not
	// written. Asserting the write count here is asserting the dedup gate.
	if got := m.countOf(attrs); got != 1 {
		t.Errorf("after poll 2, %s published %d times, want 1 — "+
			"an unchanged retained value must not be re-written (the dedup gate)", attrs, got)
	}
	if v, ok := m.val(t, attrs); !ok || v.(map[string]any)["data_source"] != "cloud" {
		t.Errorf("%s val = %v, want the data_source document", attrs, v)
	}

	// And the half F13 actually protects: when the source CHANGES, the document
	// goes out again — offered every poll, so nothing has to change the point
	// set for it to be noticed. A gate that swallowed this would leave
	// data_source permanently stale, which is the defect F13 names.
	cfg.LocalMode = true
	cfg.LocalDeviceMap = map[string]string{dev: "Klima"}
	c.deps.FaikinMQTT = m
	c.pollOnce(context.Background())
	if got := m.countOf(attrs); got != 2 {
		t.Errorf("after the data source changed, %s published %d times, want 2", attrs, got)
	}
	if v, ok := m.val(t, attrs); !ok || v.(map[string]any)["data_source"] != "local" {
		t.Errorf("%s val = %v, want the local data_source document", attrs, v)
	}
}
