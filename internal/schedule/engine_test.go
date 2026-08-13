// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// fakeClock is a controllable clock for the engine tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// applyCall records one ApplySchedule invocation.
type applyCall struct {
	target Target
	action Action
}

// stubApplier records the applications and can fail on demand.
type stubApplier struct {
	mu    sync.Mutex
	calls []applyCall
	err   error
}

func (s *stubApplier) ApplySchedule(_ context.Context, target Target, a Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, applyCall{target, a})
	return nil
}

func (s *stubApplier) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubApplier) last() applyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return applyCall{}
	}
	return s.calls[len(s.calls)-1]
}

func (s *stubApplier) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// stubStates records the published device states.
type stubStates struct {
	mu       sync.Mutex
	states   map[string]DeviceState
	switches int
}

func (s *stubStates) PublishScheduleState(_ context.Context, target Target, st DeviceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]DeviceState{}
	}
	s.states[target.Key()] = st
}

func (s *stubStates) PublishScheduleSwitches(_ context.Context, _ *Document) {
	s.mu.Lock()
	s.switches++
	s.mu.Unlock()
}

func (s *stubStates) get(deviceID string) DeviceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[deviceID]
}

// testEngine builds an engine over a temp store with the given document.
func testEngine(t *testing.T, doc *Document, now time.Time) (*Engine, *stubApplier, *stubStates, *fakeClock) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	if doc != nil {
		if err := store.Save(doc); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	clock := &fakeClock{now: now}
	applier := &stubApplier{}
	states := &stubStates{}
	e, err := NewEngine(Options{
		Store:   store,
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   clock.Now,
		Applier: applier,
		States:  states,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// The tests reason in wall-clock UTC; a machine-local zone would make the
	// weekday/slot arithmetic depend on where the test runs.
	e.loc = time.UTC
	return e, applier, states, clock
}

// weekdayDoc builds a document with a morning and a day block on Monday.
func weekdayDoc() *Document {
	return &Document{
		Version: SchemaVersion,
		Schedules: []Schedule{
			sched(
				"werktag", 0, []string{"dev-1"},
				onBlock("morning", "06:00", "08:00", 21.5, "mon"),
				onBlock("day", "08:00", "16:00", 18.5, "mon"),
			),
		},
	}
}

// monday returns a UTC time on Monday 2026-08-10.
func monday(hour, minute int) time.Time {
	return time.Date(2026, 8, 10, hour, minute, 0, 0, time.UTC)
}

// --- tests -----------------------------------------------------------------

func TestEngineAppliesAtBlockStart(t *testing.T) {
	e, applier, _, _ := testEngine(t, weekdayDoc(), monday(6, 0))
	e.Evaluate(context.Background())

	if applier.n() != 1 {
		t.Fatalf("applications = %d, want 1", applier.n())
	}
	got := applier.last()
	if got.target.DeviceID != "dev-1" || got.action.HVACMode != ModeHeat || *got.action.Setpoint != 21.5 {
		t.Errorf("applied %+v, want dev-1 heat 21.5", got)
	}
}

func TestEngineIsIdempotent(t *testing.T) {
	e, applier, _, clock := testEngine(t, weekdayDoc(), monday(6, 0))
	e.Evaluate(context.Background())
	// Same block, later in it: nothing to do.
	clock.Set(monday(6, 30))
	e.Evaluate(context.Background())
	clock.Set(monday(7, 30))
	e.Evaluate(context.Background())

	if applier.n() != 1 {
		t.Fatalf("applications = %d, want 1 (idempotence)", applier.n())
	}

	// The next block has a different target state and must be applied.
	clock.Set(monday(8, 0))
	e.Evaluate(context.Background())
	if applier.n() != 2 {
		t.Fatalf("applications = %d, want 2 after the switch point", applier.n())
	}
	if sp := *applier.last().action.Setpoint; sp != 18.5 {
		t.Errorf("setpoint = %v, want 18.5", sp)
	}
}

func TestEngineIdenticalAdjacentBlocksDoNotRewrite(t *testing.T) {
	// Two blocks with the same target state: the boundary is a switch point in
	// the ring, but nothing changes, so no write must happen.
	doc := &Document{Schedules: []Schedule{
		sched(
			"s", 0, []string{"dev-1"},
			onBlock("a", "06:00", "08:00", 21, "mon"),
			onBlock("b", "08:00", "10:00", 21, "mon"),
		),
	}}
	e, applier, _, clock := testEngine(t, doc, monday(6, 0))
	e.Evaluate(context.Background())
	clock.Set(monday(8, 0))
	e.Evaluate(context.Background())

	if applier.n() != 1 {
		t.Errorf("applications = %d, want 1 — the second block sets the same state", applier.n())
	}
}

func TestEngineCatchupWindow(t *testing.T) {
	cases := []struct {
		name      string
		at        time.Time
		wantApply bool
	}{
		{"at the block start", monday(6, 0), true},
		{"just inside the window", monday(6, 25), true},
		{"at the window edge", monday(6, 30), true},
		{"past the window", monday(6, 31), false},
		{"hours later", monday(7, 30), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A fresh engine each time: this models a daemon restarting at `at`.
			e, applier, _, _ := testEngine(t, weekdayDoc(), c.at)
			e.Evaluate(context.Background())
			if got := applier.n() > 0; got != c.wantApply {
				t.Errorf("applied = %v, want %v", got, c.wantApply)
			}
		})
	}
}

func TestEngineSeededStateIsNotAppliedLater(t *testing.T) {
	// Restart deep inside a block: the state is recorded, not written, so a
	// manual change made in the meantime survives — and a later evaluation in
	// the same block must not write either.
	e, applier, _, clock := testEngine(t, weekdayDoc(), monday(7, 0))
	e.Evaluate(context.Background())
	clock.Set(monday(7, 30))
	e.Evaluate(context.Background())
	if applier.n() != 0 {
		t.Fatalf("applications = %d, want 0", applier.n())
	}
	// The next block boundary is a fresh instruction and must be applied.
	clock.Set(monday(8, 0))
	e.Evaluate(context.Background())
	if applier.n() != 1 {
		t.Errorf("applications = %d, want 1 at the next block start", applier.n())
	}
}

func TestEngineGapForgetsLastAction(t *testing.T) {
	// Two identical blocks with a gap between them: after the gap the same
	// state must be established again, because the device may have been
	// changed by hand in between.
	doc := &Document{Schedules: []Schedule{
		sched(
			"s", 0, []string{"dev-1"},
			onBlock("a", "06:00", "07:00", 21, "mon"),
			onBlock("b", "09:00", "10:00", 21, "mon"),
		),
	}}
	e, applier, _, clock := testEngine(t, doc, monday(6, 0))
	e.Evaluate(context.Background())
	clock.Set(monday(8, 0)) // gap
	e.Evaluate(context.Background())
	clock.Set(monday(9, 0))
	e.Evaluate(context.Background())

	if applier.n() != 2 {
		t.Errorf("applications = %d, want 2 (the gap clears the cache)", applier.n())
	}
}

func TestEngineDeviceUnknownIsRetried(t *testing.T) {
	e, applier, _, clock := testEngine(t, weekdayDoc(), monday(6, 0))
	applier.setErr(ErrDeviceUnknown)
	e.Evaluate(context.Background())
	if applier.n() != 0 {
		t.Fatalf("a failed apply must not be recorded")
	}

	// The device becomes known within the catch-up window: the next evaluation
	// (triggered by the poll waking the engine) applies it.
	applier.setErr(nil)
	clock.Set(monday(6, 20))
	e.Evaluate(context.Background())
	if applier.n() != 1 {
		t.Errorf("applications = %d, want 1 once the device is known", applier.n())
	}
}

func TestEngineApplyErrorIsNotRecorded(t *testing.T) {
	e, applier, _, clock := testEngine(t, weekdayDoc(), monday(6, 0))
	applier.setErr(errors.New("broker down"))
	e.Evaluate(context.Background())
	applier.setErr(nil)
	clock.Set(monday(6, 10))
	e.Evaluate(context.Background())
	if applier.n() != 1 {
		t.Errorf("applications = %d, want 1 — a failed write must be retried", applier.n())
	}
}

func TestEnginePublishesState(t *testing.T) {
	e, _, states, _ := testEngine(t, weekdayDoc(), monday(6, 30))
	e.Evaluate(context.Background())

	st := states.get("dev-1")
	if st.Active == nil || st.Active.BlockID != "morning" {
		t.Fatalf("active claim = %+v, want the morning block", st.Active)
	}
	want := monday(8, 0)
	if !st.NextChange.Equal(want) {
		t.Errorf("next change = %v, want %v", st.NextChange, want)
	}
	if states.switches == 0 {
		t.Error("switch states must be published too")
	}
}

func TestEnginePublishesEmptyStateInGap(t *testing.T) {
	e, _, states, _ := testEngine(t, weekdayDoc(), monday(20, 0))
	e.Evaluate(context.Background())

	st := states.get("dev-1")
	if st.Active != nil {
		t.Errorf("active claim = %+v, want nil in a gap", st.Active)
	}
	// The next change is the following Monday's first block.
	if st.NextChange.IsZero() {
		t.Error("next change must be set even in a gap")
	}
}

func TestEngineOffBlockAppliesPowerOff(t *testing.T) {
	doc := &Document{Schedules: []Schedule{
		sched("s", 0, []string{"dev-1"}, Block{
			ID: "off", Days: []string{"mon"}, Start: "22:00", End: "23:00",
			Action: Action{Power: PowerOff},
		}),
	}}
	e, applier, _, _ := testEngine(t, doc, monday(22, 0))
	e.Evaluate(context.Background())

	if applier.n() != 1 {
		t.Fatalf("applications = %d, want 1", applier.n())
	}
	if got := applier.last().action.HVACPayload(); got != "off" {
		t.Errorf("payload = %q, want off", got)
	}
}

func TestEnginePassesExplicitEmbeddedID(t *testing.T) {
	doc := weekdayDoc()
	doc.Schedules[0].Targets[0].EmbeddedID = "climateControlMainZone"
	e, applier, _, _ := testEngine(t, doc, monday(6, 0))
	e.Evaluate(context.Background())

	if got := applier.last().target.EmbeddedID; got != "climateControlMainZone" {
		t.Errorf("embeddedID = %q, want climateControlMainZone", got)
	}
}

func TestEngineSetEnabled(t *testing.T) {
	e, applier, _, _ := testEngine(t, weekdayDoc(), monday(6, 0))

	if err := e.SetEnabled("werktag", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	doc := e.Document()
	if doc.Schedules[0].Enabled {
		t.Error("schedule must be disabled")
	}
	if doc.Revision != 1 {
		t.Errorf("revision = %d, want 1", doc.Revision)
	}

	// Disabling again is a no-op: no write, no revision bump.
	if err := e.SetEnabled("werktag", false); err != nil {
		t.Fatalf("SetEnabled (repeat): %v", err)
	}
	if got := e.Document().Revision; got != 1 {
		t.Errorf("revision after a no-op = %d, want 1", got)
	}

	// A disabled schedule claims nothing, so nothing is applied.
	e.Evaluate(context.Background())
	if applier.n() != 0 {
		t.Errorf("applications = %d, want 0 while disabled", applier.n())
	}

	// The change must be on disk.
	reloaded, err := e.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Schedules[0].Enabled {
		t.Error("the disabled state must be persisted")
	}

	if err := e.SetEnabled("nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnabled(unknown) = %v, want ErrNotFound", err)
	}
}

func TestEngineReplaceRevisionGuard(t *testing.T) {
	e, _, _, _ := testEngine(t, weekdayDoc(), monday(6, 0))

	next := e.Document()
	next.Schedules[0].Name = "Neu"
	got, err := e.Replace(next, 0)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got.Revision != 1 {
		t.Errorf("revision = %d, want 1", got.Revision)
	}
	if e.Document().Schedules[0].Name != "Neu" {
		t.Error("the new name must be installed")
	}

	// A second editor holding the old revision must be rejected.
	stale := e.Document()
	stale.Schedules[0].Name = "Konkurrenz"
	if _, err := e.Replace(stale, 0); !errors.Is(err, ErrStaleRevision) {
		t.Errorf("Replace with a stale revision = %v, want ErrStaleRevision", err)
	}
	if e.Document().Schedules[0].Name != "Neu" {
		t.Error("a rejected write must not change anything")
	}

	// -1 skips the check (used by internal callers).
	if _, err := e.Replace(stale, -1); err != nil {
		t.Errorf("Replace(-1): %v", err)
	}

	// An invalid document is rejected before it reaches the disk.
	bad := e.Document()
	bad.Schedules[0].Blocks[0].Start = "06:20"
	if _, err := e.Replace(bad, -1); err == nil {
		t.Error("Replace with an invalid document must fail")
	}
	if _, err := e.Replace(nil, -1); err == nil {
		t.Error("Replace(nil) must fail")
	}
}

func TestEngineDelete(t *testing.T) {
	e, _, _, _ := testEngine(t, weekdayDoc(), monday(6, 0))
	if err := e.Delete("werktag"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(e.Document().Schedules) != 0 {
		t.Error("the schedule must be gone")
	}
	if err := e.Delete("werktag"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(unknown) = %v, want ErrNotFound", err)
	}
}

func TestEngineUntilNext(t *testing.T) {
	e, _, _, clock := testEngine(t, weekdayDoc(), monday(6, 30))
	if got, want := e.untilNext(), 90*time.Minute; got != want {
		t.Errorf("untilNext = %v, want %v", got, want)
	}

	// With no switch point at all the engine falls back to the heartbeat.
	empty, _, _, _ := testEngine(t, NewDocument(), monday(6, 30))
	if got := empty.untilNext(); got != heartbeat {
		t.Errorf("untilNext (no schedules) = %v, want %v", got, heartbeat)
	}

	// Right at a switch point the sleep is clamped instead of spinning.
	clock.Set(monday(8, 0))
	if got := e.untilNext(); got < time.Second {
		t.Errorf("untilNext at a switch point = %v, want at least 1s", got)
	}
}

func TestEngineRunStopsWithContext(t *testing.T) {
	e, applier, _, _ := testEngine(t, weekdayDoc(), monday(6, 0))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// The first evaluation happens immediately; wait for it before cancelling.
	deadline := time.After(2 * time.Second)
	for applier.n() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not evaluate within 2s")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestEngineWakeTriggersEvaluation(t *testing.T) {
	e, applier, _, clock := testEngine(t, weekdayDoc(), monday(20, 0)) // gap: nothing applied
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()

	// Move into a block and wake the engine; it must not wait out the timer.
	clock.Set(monday(6, 0))
	e.Wake()

	deadline := time.After(2 * time.Second)
	for applier.n() == 0 {
		select {
		case <-deadline:
			t.Fatal("Wake did not trigger an evaluation within 2s")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

func TestNewEngineRequiresStore(t *testing.T) {
	if _, err := NewEngine(Options{}); err == nil {
		t.Error("NewEngine without a store must fail")
	}
}

func TestResolveLocation(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if got := resolveLocation("", "", log); got != time.Local {
		t.Errorf("no zone = %v, want local", got)
	}
	if got := resolveLocation("Nonsense/Zone", "", log); got != time.Local {
		t.Errorf("unknown zone = %v, want a fallback to local", got)
	}
	if got := resolveLocation("UTC", "", log); got.String() != "UTC" {
		t.Errorf("explicit zone = %v, want UTC", got)
	}
	// The document's zone applies when no option is given.
	if got := resolveLocation("", "UTC", log); got.String() != "UTC" {
		t.Errorf("document zone = %v, want UTC", got)
	}
}
