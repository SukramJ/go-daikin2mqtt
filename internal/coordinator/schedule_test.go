// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// stubScheduler implements the coordinator's scheduler interface.
type stubScheduler struct {
	doc      *schedule.Document
	toggles  []toggle
	toggling error
	woken    int
}

type toggle struct {
	id string
	on bool
}

func (s *stubScheduler) SetEnabled(id string, on bool) error {
	if s.toggling != nil {
		return s.toggling
	}
	s.toggles = append(s.toggles, toggle{id, on})
	return nil
}

func (s *stubScheduler) Document() *schedule.Document {
	if s.doc == nil {
		return schedule.NewDocument()
	}
	return s.doc.Clone()
}

func (s *stubScheduler) Wake() { s.woken++ }

func ptr(f float64) *float64 { return &f }

// drainOne pulls a single queued write request, failing if none is pending.
func drainOne(t *testing.T, c *Coordinator) writeReq {
	t.Helper()
	select {
	case req := <-c.writes:
		return req
	default:
		t.Fatal("expected a queued write request, got none")
		return writeReq{}
	}
}

func TestApplyScheduleQueuesModeBeforeSetpoint(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	c := newCoordinator(t, cloud, newStubMQTT())
	// A poll populates climateEmbedded, which the applier needs.
	c.pollOnce(context.Background())

	err := c.ApplySchedule(context.Background(), dev, "", schedule.Action{
		Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: ptr(21.5),
	})
	if err != nil {
		t.Fatalf("ApplySchedule: %v", err)
	}

	// Order matters: the setpoint's PATCH path contains {mode}, resolved from
	// modeCache, which the mode write updates via noteWrite.
	first := drainOne(t, c)
	if first.topic != "hvac_mode" || first.payload != "heat" {
		t.Errorf("first request = %+v, want hvac_mode/heat", first)
	}
	second := drainOne(t, c)
	if second.topic != setpointTopic || second.payload != "21.5" {
		t.Errorf("second request = %+v, want %s/21.5", second, setpointTopic)
	}
	if second.deviceID != dev || second.embeddedID != emb {
		t.Errorf("addressing = %s/%s, want %s/%s", second.deviceID, second.embeddedID, dev, emb)
	}
}

func TestApplyScheduleOffSkipsSetpoint(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, newStubMQTT())
	c.pollOnce(context.Background())

	// A setpoint alongside "off" is meaningless and must not be written: the
	// mode-scoped path could not be resolved for a unit being switched off.
	err := c.ApplySchedule(context.Background(), dev, "", schedule.Action{
		Power: schedule.PowerOff, Setpoint: ptr(21.5),
	})
	if err != nil {
		t.Fatalf("ApplySchedule: %v", err)
	}
	if got := drainOne(t, c); got.topic != "hvac_mode" || got.payload != "off" {
		t.Errorf("request = %+v, want hvac_mode/off", got)
	}
	select {
	case req := <-c.writes:
		t.Errorf("unexpected second request %+v", req)
	default:
	}
}

func TestApplyScheduleWithoutSetpoint(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, newStubMQTT())
	c.pollOnce(context.Background())

	// No setpoint means "leave the temperature alone".
	err := c.ApplySchedule(context.Background(), dev, "", schedule.Action{
		Power: schedule.PowerOn, HVACMode: schedule.ModeCool,
	})
	if err != nil {
		t.Fatalf("ApplySchedule: %v", err)
	}
	if got := drainOne(t, c); got.payload != "cool" {
		t.Errorf("request = %+v, want cool", got)
	}
	select {
	case req := <-c.writes:
		t.Errorf("unexpected setpoint request %+v", req)
	default:
	}
}

func TestApplyScheduleUnknownDevice(t *testing.T) {
	// No poll has run, so no management point is known.
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	err := c.ApplySchedule(context.Background(), "dev1", "", schedule.Action{
		Power: schedule.PowerOn, HVACMode: schedule.ModeHeat,
	})
	if !errors.Is(err, schedule.ErrDeviceUnknown) {
		t.Fatalf("ApplySchedule = %v, want ErrDeviceUnknown", err)
	}
	select {
	case req := <-c.writes:
		t.Errorf("nothing must be queued for an unknown device, got %+v", req)
	default:
	}
}

func TestApplyScheduleExplicitEmbeddedIDSkipsLookup(t *testing.T) {
	// An explicit embedded id works even before the first poll.
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	err := c.ApplySchedule(context.Background(), "dev1", "zone2", schedule.Action{
		Power: schedule.PowerOn, HVACMode: schedule.ModeHeat,
	})
	if err != nil {
		t.Fatalf("ApplySchedule: %v", err)
	}
	if got := drainOne(t, c); got.embeddedID != "zone2" {
		t.Errorf("embeddedID = %q, want zone2", got.embeddedID)
	}
}

func TestSchedulerEnableRouting(t *testing.T) {
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	sched := &stubScheduler{}
	c.AttachScheduler(sched)

	cases := []struct {
		payload string
		want    bool
	}{
		{"on", true},
		{"ON", true},
		{"true", true},
		{"1", true},
		{"off", false},
		{"nonsense", false},
	}
	for _, tc := range cases {
		c.handleWrite(context.Background(), writeReq{
			deviceID:   schedule.SchedulerDeviceID,
			embeddedID: "werktag",
			topic:      ScheduleEnabledTopic,
			payload:    tc.payload,
		})
	}
	if len(sched.toggles) != len(cases) {
		t.Fatalf("toggles = %d, want %d", len(sched.toggles), len(cases))
	}
	for i, tc := range cases {
		if sched.toggles[i].id != "werktag" || sched.toggles[i].on != tc.want {
			t.Errorf("payload %q → %+v, want werktag=%v", tc.payload, sched.toggles[i], tc.want)
		}
	}
}

func TestSchedulerWriteIgnoresUnknownTopic(t *testing.T) {
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	sched := &stubScheduler{}
	c.AttachScheduler(sched)

	c.handleWrite(context.Background(), writeReq{
		deviceID:   schedule.SchedulerDeviceID,
		embeddedID: "werktag",
		topic:      "something_else",
		payload:    "on",
	})
	if len(sched.toggles) != 0 {
		t.Errorf("unknown scheduler topic must be ignored, got %+v", sched.toggles)
	}

	// Without an attached engine the command is dropped, not applied.
	c2 := newCoordinator(t, &stubCloud{}, newStubMQTT())
	c2.handleWrite(context.Background(), writeReq{
		deviceID:   schedule.SchedulerDeviceID,
		embeddedID: "werktag",
		topic:      ScheduleEnabledTopic,
		payload:    "on",
	})
}

func TestPublishScheduleState(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	m := newStubMQTT()
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, m)
	c.pollOnce(context.Background())

	next := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC)
	c.PublishScheduleState(context.Background(), dev, schedule.DeviceState{
		Active: &schedule.Claim{
			ScheduleID: "werktag", ScheduleName: "Werktag", BlockID: "b1", Label: "Nacht",
		},
		NextChange: next,
	})

	got, ok := m.get("daikin/" + dev + "/" + emb + "/" + ScheduleStateTopic + "/state")
	if !ok || got.payload != "Werktag · Nacht" {
		t.Errorf("state = %+v, want 'Werktag · Nacht'", got)
	}
	if !got.retain {
		t.Error("the state must be retained")
	}
	got, ok = m.get("daikin/" + dev + "/" + emb + "/" + ScheduleNextTopic + "/state")
	if !ok || got.payload != next.Format(time.RFC3339) {
		t.Errorf("next change = %+v, want %s", got, next.Format(time.RFC3339))
	}
}

func TestPublishScheduleStateIdleIsLocalized(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	m := newStubMQTT()
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, m)
	c.pollOnce(context.Background())

	// The test config is German, so the idle label comes from label_de — the
	// only string in this sensor the daemon produces itself.
	c.PublishScheduleState(context.Background(), dev, schedule.DeviceState{})

	got, ok := m.get("daikin/" + dev + "/" + emb + "/" + ScheduleStateTopic + "/state")
	if !ok || got.payload != "Kein Block" {
		t.Errorf("idle state = %+v, want 'Kein Block'", got)
	}
	// With no next change the topic is published empty rather than left stale.
	got, ok = m.get("daikin/" + dev + "/" + emb + "/" + ScheduleNextTopic + "/state")
	if !ok || got.payload != "" {
		t.Errorf("next change = %+v, want empty", got)
	}
}

func TestPublishScheduleStateWithoutLabel(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	m := newStubMQTT()
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, m)
	c.pollOnce(context.Background())

	c.PublishScheduleState(context.Background(), dev, schedule.DeviceState{
		Active: &schedule.Claim{ScheduleID: "s", ScheduleName: "Urlaub"},
	})
	got, _ := m.get("daikin/" + dev + "/" + emb + "/" + ScheduleStateTopic + "/state")
	if got.payload != "Urlaub" {
		t.Errorf("state = %q, want 'Urlaub'", got.payload)
	}
}

func TestPublishScheduleStateUnknownDevice(t *testing.T) {
	m := newStubMQTT()
	c := newCoordinator(t, &stubCloud{}, m)
	// No poll: nothing must be published for a device we cannot address.
	c.PublishScheduleState(context.Background(), "dev1", schedule.DeviceState{})
	if n := m.count(); n != 0 {
		t.Errorf("published %d messages for an unknown device, want 0", n)
	}
}

func TestPublishScheduleSwitches(t *testing.T) {
	m := newStubMQTT()
	c := newCoordinator(t, &stubCloud{}, m)

	doc := schedule.NewDocument()
	doc.Schedules = []schedule.Schedule{
		{ID: "werktag", Name: "Werktag", Enabled: true},
		{ID: "urlaub", Name: "Urlaub", Enabled: false},
	}
	c.PublishScheduleSwitches(context.Background(), doc)

	for _, tc := range []struct{ id, want string }{{"werktag", "on"}, {"urlaub", "off"}} {
		got, ok := m.get("daikin/" + schedule.SchedulerDeviceID + "/" + tc.id + "/" + ScheduleEnabledTopic + "/state")
		if !ok || got.payload != tc.want {
			t.Errorf("%s = %+v, want %s", tc.id, got, tc.want)
		}
		if !got.retain {
			t.Errorf("%s must be retained", tc.id)
		}
	}

	// A nil document is a no-op rather than a panic.
	c.PublishScheduleSwitches(context.Background(), nil)
}

func TestSchedulePointsOnlyWithEngine(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, newStubMQTT())
	c.pollOnce(context.Background())

	devices, err := model.ParseDevices(devicesJSON(dev, emb))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if got := c.schedulePoints(devices); got != nil {
		t.Errorf("without an engine there must be no schedule entities, got %d", len(got))
	}

	c.AttachScheduler(&stubScheduler{})
	points := c.schedulePoints(devices)
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2", len(points))
	}
	seen := map[string]bool{}
	for _, p := range points {
		seen[p.Topic] = true
		if p.DeviceID != dev || p.EmbeddedID != emb {
			t.Errorf("point addressing = %s/%s", p.DeviceID, p.EmbeddedID)
		}
	}
	if !seen[ScheduleStateTopic] || !seen[ScheduleNextTopic] {
		t.Errorf("topics = %v, want both status sensors", seen)
	}
}

func TestScheduleSignatureTracksSchedules(t *testing.T) {
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	if got := c.scheduleSignature(); got != "" {
		t.Errorf("signature without an engine = %q, want empty", got)
	}

	doc := schedule.NewDocument()
	doc.Schedules = []schedule.Schedule{{ID: "werktag", Name: "Werktag"}}
	sched := &stubScheduler{doc: doc}
	c.AttachScheduler(sched)

	before := c.scheduleSignature()
	// Renaming changes the published entity name, so discovery must be redone.
	doc.Schedules[0].Name = "Arbeitstag"
	if after := c.scheduleSignature(); after == before {
		t.Error("renaming a schedule must change the discovery signature")
	}
	// Adding one too.
	doc.Schedules = append(doc.Schedules, schedule.Schedule{ID: "urlaub", Name: "Urlaub"})
	if after := c.scheduleSignature(); after == before {
		t.Error("adding a schedule must change the discovery signature")
	}
}

func TestPollWakesScheduler(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	c := newCoordinator(t, &stubCloud{devices: devicesJSON(dev, emb)}, newStubMQTT())
	sched := &stubScheduler{}
	c.AttachScheduler(sched)

	c.pollOnce(context.Background())
	if sched.woken == 0 {
		t.Error("a completed poll must wake the engine so pending blocks can be applied")
	}
}
