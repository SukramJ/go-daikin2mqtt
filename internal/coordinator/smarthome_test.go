// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
)

// The mqtt-smarthome 2.0 instance surface (openccu-loom ADR 0083):
// `<name>/connected`, the set normaliser, `<name>/info` and the maintenance
// commands.

// routerMQTT is an mqtt.Client that keeps every subscription with its QoS and
// delivers a message to each matching one, as a broker would.
type routerMQTT struct {
	*stubMQTT
	mu   sync.Mutex
	subs map[string]routedSub
}

type routedSub struct {
	qos     mqtt.QoS
	handler mqtt.MessageHandler
}

func newRouterMQTT() *routerMQTT {
	return &routerMQTT{stubMQTT: newStubMQTT(), subs: map[string]routedSub{}}
}

func (r *routerMQTT) Subscribe(_ context.Context, filter string, qos mqtt.QoS, h mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[filter] = routedSub{qos: qos, handler: h}
	return mqtt.SubscribeResult{}, nil
}

func (r *routerMQTT) deliver(topic, payload string, retain bool) {
	r.mu.Lock()
	var hs []mqtt.MessageHandler
	for f, s := range r.subs {
		if publisher.MatchFilter(f, topic) {
			hs = append(hs, s.handler)
		}
	}
	r.mu.Unlock()
	for _, h := range hs {
		h(&mqtt.Message{Topic: topic, Payload: []byte(payload), Retain: retain})
	}
}

func (r *routerMQTT) qosOf(filter string) (mqtt.QoS, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.subs[filter]
	return s.qos, ok
}

// TestConnectedFollowsTheUpstream pins `<name>/connected` in cloud mode: 1 on
// connect until a poll proves the cloud usable, 2 while it is, 1 again on a
// failed poll, unchanged by a scan-ignore skip, re-announced at the decided
// level on every reconnect, and 0 on a graceful stop.
func TestConnectedFollowsTheUpstream(t *testing.T) {
	t.Parallel()
	cloud := &stubCloud{devices: devicesJSON("dev1", "climateControl")}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)
	ctx := context.Background()
	level := func() string {
		t.Helper()
		msg, ok := m.get("daikin/connected")
		if !ok || !msg.retain {
			t.Fatalf("connected = %+v (published %v), want a retained level", msg, ok)
		}
		return msg.payload
	}

	c.PublishOnline(ctx)
	if got := level(); got != "1" {
		t.Errorf("on connect, before any poll: %q, want 1", got)
	}
	c.pollOnce(ctx)
	if got := level(); got != "2" {
		t.Errorf("after a successful poll: %q, want 2", got)
	}
	cloud.getErr = client.ErrScanIgnore
	c.pollOnce(ctx)
	if got := level(); got != "2" {
		t.Errorf("after a scan-ignore skip: %q, want 2 — the skip says nothing about the cloud", got)
	}
	cloud.getErr = errors.New("cloud down")
	c.pollOnce(ctx)
	if got := level(); got != "1" {
		t.Errorf("after a failed poll: %q, want 1", got)
	}
	cloud.getErr = nil
	c.pollOnce(ctx)
	if got := level(); got != "2" {
		t.Errorf("after recovery: %q, want 2", got)
	}
	before := m.countOf("daikin/connected")
	c.PublishOnline(ctx) // a reconnect: the fresh runtime starts at 1
	if got := level(); got != "2" || m.countOf("daikin/connected") == before {
		t.Errorf("after a reconnect: %q, want 2 re-announced", got)
	}
	c.PublishOffline(ctx)
	if got := level(); got != "0" {
		t.Errorf("after a graceful stop: %q, want 0", got)
	}
}

// TestConnectedInLocalModeFollowsTheFaikinLink pins the local-mode half: 2
// needs the device topology (one successful cloud poll) and the Faikin link;
// once both hold, a cloud outage no longer takes the level down, and a Faikin
// link that drops does, without waiting for a poll.
func TestConnectedInLocalModeFollowsTheFaikinLink(t *testing.T) {
	t.Parallel()
	var faikinUp atomic.Bool
	faikinUp.Store(true)
	cloud := &stubCloud{devices: devicesJSON("dev1", "climateControl")}
	m := newStubMQTT()
	cfg := testConfig()
	cfg.LocalMode = true
	cfg.LocalDeviceMap = config.DeviceMap{"dev1": "Klima"}
	c := New(Deps{
		Cfg: cfg, Client: cloud, MQTT: m, FaikinMQTT: newStubMQTT(), Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(), FaikinConnected: faikinUp.Load,
	})
	ctx := context.Background()
	level := func() string { t.Helper(); msg, _ := m.get("daikin/connected"); return msg.payload }

	c.PublishOnline(ctx)
	c.updateConnected(ctx)
	if got := level(); got != "1" {
		t.Errorf("before the topology is known: %q, want 1", got)
	}
	c.pollOnce(ctx)
	if got := level(); got != "2" {
		t.Errorf("topology known, Faikin up: %q, want 2", got)
	}
	cloud.getErr = errors.New("cloud down")
	c.pollOnce(ctx)
	if got := level(); got != "2" {
		t.Errorf("cloud down in local mode: %q, want 2 — the mapped units are read over Faikin", got)
	}
	faikinUp.Store(false)
	c.updateConnected(ctx) // what watchUpstream does on its tick
	if got := level(); got != "1" {
		t.Errorf("Faikin link down: %q, want 1", got)
	}
}

// TestSetItemsAreNormalisedAndSubscribedAtQoS1 drives the set route through
// the real router: subscribed at QoS 1, `{"val": x}` unwrapped to x, and an
// empty, malformed, structured or retained set never reaching the write queue.
func TestSetItemsAreNormalisedAndSubscribedAtQoS1(t *testing.T) {
	t.Parallel()
	m := newRouterMQTT()
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: m, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	if err := c.subscribeWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.commands.Stop(context.Background()) })

	if q, ok := m.qosOf("daikin/set/+/+/+"); !ok || q != mqtt.QoS1 {
		t.Fatalf("set subscription QoS = %v (subscribed %v), want 1", q, ok)
	}
	take := func() (writeReq, bool) {
		c.commands.WaitIdle()
		select {
		case r := <-c.writes:
			return r, true
		default:
			return writeReq{}, false
		}
	}

	const topic = "daikin/set/dev1/climateControl/power"
	for _, tc := range []struct{ payload, want string }{
		{`{"val":"on"}`, "on"}, {`{"val":true}`, "true"}, {"off", "off"}, {" 21.5 ", "21.5"},
	} {
		payload, want := tc.payload, tc.want
		m.deliver(topic, payload, false)
		req, ok := take()
		if !ok || req.payload != want || req.deviceID != "dev1" || req.embeddedID != "climateControl" || req.topic != "power" {
			t.Errorf("set %q queued %+v (ok=%v), want payload %q on dev1/climateControl/power", payload, req, ok, want)
		}
	}
	for _, payload := range []string{"", "   ", `{"val":null}`, `{"val":`, `{"mode":"x"}`} {
		m.deliver(topic, payload, false)
		if req, ok := take(); ok {
			t.Errorf("set %q was queued: %+v", payload, req)
		}
	}
	m.deliver(topic, "on", true)
	if req, ok := take(); ok {
		t.Errorf("a retained set was queued: %+v", req)
	}
	// The refresh action item: any non-empty payload fires it.
	m.deliver("daikin/set/dev1/climateControl/refresh", "PRESS", false)
	if req, ok := take(); !ok || req.topic != "refresh" {
		t.Errorf("refresh PRESS queued %+v (ok=%v)", req, ok)
	}
}

// TestSetConversions pins spec §5.3's conversions on the write path: booleans
// in all their spellings, enum tokens in any case (labels still accepted), and
// numbers rounded to the live step and clamped to the live range.
func TestSetConversions(t *testing.T) {
	t.Parallel()
	cloud := &stubCloud{devices: devicesJSON("dev1", "climateControl")}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.pollOnce(context.Background()) // live bounds: setpoint 16..32, step 0.5
	entry := func(topic string) writeReq {
		return writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: topic}
	}
	for _, tc := range []struct {
		topic, payload string
		want           any
	}{
		{"power", "true", "on"},
		{"power", "ON", "on"},
		{"power", "1", "on"},
		{"power", "Yes", "on"},
		{"power", "false", "off"},
		{"power", "off", "off"},
		{"power", "0", "off"},
		{"power", "no", "off"},
		{"power", "maybe", nil},
		{"operation_mode", "cooling", "cooling"},
		{"operation_mode", "COOLING", "cooling"},
		{"operation_mode", "Heizen", "heating"},
		{"operation_mode", "Cooling", "cooling"},
		{"operation_mode", "turbo", nil},
		{"temperature_setpoint", "21.3", 21.5},
		{"temperature_setpoint", "40", 32.0},
		{"temperature_setpoint", "-5", 16.0},
		{"temperature_setpoint", "NaN", nil},
	} {
		e, _ := c.deps.Catalog.ByTopic(tc.topic)
		req := entry(tc.topic)
		req.payload = tc.payload
		got, ok := c.coerceWriteValue(e, req)
		switch {
		case tc.want == nil && ok:
			t.Errorf("%s %q accepted as %v, want rejected", tc.topic, tc.payload, got)
		case tc.want != nil && (!ok || got != tc.want):
			t.Errorf("%s %q = %v (ok=%v), want %v", tc.topic, tc.payload, got, ok, tc.want)
		}
	}
}

// TestARejectedSetIsLoggedWithTopicAndPayload pins spec §3.3's MUST: a rejected
// or failed request is logged at warn with its topic and payload.
func TestARejectedSetIsLoggedWithTopicAndPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cloud := &stubCloud{}
	c := New(Deps{
		Cfg: testConfig(), Client: cloud, MQTT: newStubMQTT(), Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)), Clock: fixedClock(),
	})
	c.handleWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "power", payload: "maybe"})
	cloud.patchErr = errors.New("cloud says no")
	c.handleWrite(context.Background(), writeReq{deviceID: "dev1", embeddedID: "climateControl", topic: "power", payload: "on"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := map[string]string{"coordinator.write_bad_value": "maybe", "coordinator.patch_failed": "on"}
	for _, l := range lines {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		payload, ok := want[msg]
		if !ok {
			continue
		}
		if rec["level"] != "WARN" || rec["topic"] != "daikin/set/dev1/climateControl/power" || rec["payload"] != payload {
			t.Errorf("%s logged %v", msg, rec)
		}
		delete(want, msg)
	}
	if len(want) != 0 {
		t.Errorf("not logged: %v\n%s", want, buf.String())
	}
}

// TestInstanceInfoAndMaintenance wires a publisher.Instance the way main does
// and pins what the coordinator owes it: `<name>/info` on every connect, and
// the maintenance route on the same router as the set route, at QoS 1.
func TestInstanceInfoAndMaintenance(t *testing.T) {
	t.Parallel()
	m := newRouterMQTT()
	var level slog.LevelVar
	var restarts atomic.Int32
	supervised := false
	inst := publisher.NewInstance(hagomqtt.Split(m, m), publisher.InstanceConfig{
		Layout:        layout.New("daikin").SmartHome(),
		Name:          "go-daikin2mqtt",
		Version:       "9.9.9",
		Extra:         map[string]any{"mode": "cloud"},
		SetLogLevel:   publisher.LevelVarSetter(&level),
		Supervised:    func() bool { return supervised },
		Shutdown:      func() { restarts.Add(1) },
		StatsInterval: publisher.StatsInterval(0),
		Logger:        slog.New(slog.DiscardHandler),
	})
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: m, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(), Instance: inst,
	})
	ctx := context.Background()
	c.PublishOnline(ctx)
	msg, ok := m.get("daikin/info")
	if !ok || !msg.retain {
		t.Fatalf("info = %+v (published %v), want retained", msg, ok)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(msg.payload), &info); err != nil {
		t.Fatalf("info is not JSON: %s", msg.payload)
	}
	for k, v := range map[string]any{"name": "go-daikin2mqtt", "version": "9.9.9", "spec": "2.0", "mode": "cloud", "maintenance": true} {
		if info[k] != v {
			t.Errorf("info.%s = %v, want %v", k, info[k], v)
		}
	}

	if err := c.subscribeWrites(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.commands.Stop(context.Background()) })
	if q, ok := m.qosOf("daikin/maintenance/set/#"); !ok || q != mqtt.QoS1 {
		t.Fatalf("maintenance subscription QoS = %v (subscribed %v), want 1", q, ok)
	}
	m.deliver("daikin/maintenance/set/loglevel", "debug", false)
	c.commands.WaitIdle()
	if level.Level() != slog.LevelDebug {
		t.Errorf("log level = %v after loglevel debug", level.Level())
	}
	m.deliver("daikin/maintenance/set/restart", "", false)
	c.commands.WaitIdle()
	time.Sleep(20 * time.Millisecond) // the shutdown is started on its own goroutine
	if restarts.Load() != 0 {
		t.Error("an unsupervised daemon restarted")
	}
	supervised = true
	m.deliver("daikin/maintenance/set/restart", "", false)
	c.commands.WaitIdle()
	deadline := time.Now().Add(2 * time.Second)
	for restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if restarts.Load() != 1 {
		t.Errorf("a supervised restart ran the shutdown %d times, want 1", restarts.Load())
	}
}
