// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrNotFound is returned when a schedule id does not exist.
var ErrNotFound = errors.New("schedule: not found")

// ErrStaleRevision is returned when a write is based on an outdated revision,
// so a second editor cannot silently overwrite the first one's changes.
var ErrStaleRevision = errors.New("schedule: stale revision")

// Applier applies a resolved target state to one device. The coordinator
// implements it by feeding the daemon's regular write path, which is what
// makes the scheduler inherit multi-split mode sync, the local/cloud backend
// choice and the cloud lock.
//
// ErrDeviceUnknown signals "not yet, try again": the device has not been seen
// in a poll, so nothing was written and the engine must not record the action
// as applied.
type Applier interface {
	ApplySchedule(ctx context.Context, target Target, a Action) error
}

// ErrDeviceUnknown is returned by an [Applier] that does not (yet) know the
// device — before the first successful cloud poll, the daemon has neither the
// management point nor the current mode the setpoint path needs.
var ErrDeviceUnknown = errors.New("schedule: device unknown")

// StatePublisher receives the per-device scheduler status after every
// evaluation, for the two Home Assistant status sensors. Optional.
type StatePublisher interface {
	PublishScheduleState(ctx context.Context, target Target, st DeviceState)
	PublishScheduleSwitches(ctx context.Context, doc *Document)
}

// DeviceState is what the status sensors report: the block in force now and
// when the next change is due. Both are intent, not a device reading.
type DeviceState struct {
	// Active is the winning claim, or nil when no block applies right now.
	Active *Claim
	// NextChange is the wall-clock time of the next switch point; zero when the
	// week holds no further change.
	NextChange time.Time
}

// Options configure an [Engine].
type Options struct {
	Store  *Store
	Logger *slog.Logger
	// Clock defaults to time.Now; tests inject a controllable one.
	Clock func() time.Time
	// Timezone is the IANA zone the wall-clock times are read in. Empty uses
	// the document's zone, then the system zone.
	Timezone string
	// Catchup is how long after a missed block start the target state is still
	// applied. Zero uses DefaultCatchup.
	Catchup time.Duration
	Applier Applier
	States  StatePublisher
}

// DefaultCatchup is the default catch-up window: one slot. A restart inside
// the slot that just started still establishes its state; anything older is
// left alone, because a manual change may have happened since.
const DefaultCatchup = 30 * time.Minute

// heartbeat is how long the engine sleeps when the week holds no switch point
// at all — no schedules, or a single block covering every slot. Everything
// else is driven by an exact timer, and every external change (API, HA switch,
// a poll making devices known) calls Wake.
const heartbeat = time.Hour

// Engine evaluates the schedules and applies the resulting target states.
//
// It writes at switch points only: between two boundaries a manual change from
// Home Assistant, the Daikin app or the remote stays untouched. Two mechanisms
// keep that promise across restarts — an idempotence cache (an action equal to
// the last applied one is not written again) and the catch-up window (a block
// start older than Catchup is recorded but not applied).
type Engine struct {
	store   *Store
	log     *slog.Logger
	clock   func() time.Time
	catchup time.Duration
	applier Applier
	states  StatePublisher

	mu   sync.RWMutex
	doc  *Document
	loc  *time.Location
	last map[string]string // deviceID → last applied action signature

	// wake carries "re-evaluate now" requests (config changed, devices became
	// known). Capacity 1: a pending wake-up already covers any further one.
	wake chan struct{}
}

// NewEngine builds an engine and loads the current document. A store error is
// returned; a missing file is not an error (it yields an empty document).
func NewEngine(o Options) (*Engine, error) {
	if o.Store == nil {
		return nil, errors.New("schedule: store is required")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	clock := o.Clock
	if clock == nil {
		clock = time.Now
	}
	catchup := o.Catchup
	if catchup <= 0 {
		catchup = DefaultCatchup
	}

	doc, err := o.Store.Load()
	if err != nil {
		return nil, err
	}
	e := &Engine{
		store:   o.Store,
		log:     log,
		clock:   clock,
		catchup: catchup,
		applier: o.Applier,
		states:  o.States,
		doc:     doc,
		last:    map[string]string{},
		wake:    make(chan struct{}, 1),
	}
	e.loc = resolveLocation(o.Timezone, doc.Timezone, log)
	return e, nil
}

// resolveLocation picks the time zone: the explicit option wins, then the
// document's, then the system zone. An unknown zone falls back to local with a
// warning rather than failing the daemon — a typo in a config field must not
// take the bridge down.
func resolveLocation(optZone, docZone string, log *slog.Logger) *time.Location {
	for _, name := range []string{optZone, docZone} {
		if name == "" {
			continue
		}
		loc, err := time.LoadLocation(name)
		if err == nil {
			return loc
		}
		log.Warn("schedule.unknown_timezone",
			slog.String("timezone", name), slog.String("err", err.Error()))
	}
	return time.Local
}

// Location returns the zone the schedules are interpreted in.
func (e *Engine) Location() *time.Location {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loc
}

// Document returns a deep copy of the current document.
func (e *Engine) Document() *Document {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.doc.Clone()
}

// Wake asks the engine to re-evaluate now. It never blocks. The coordinator
// calls it after a poll (devices became known) and the API after every change.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Replace validates, persists and installs a new document. When expectRev is
// non-negative it must match the current revision, otherwise ErrStaleRevision
// is returned and nothing is written. The stored revision is bumped on success.
func (e *Engine) Replace(doc *Document, expectRev int) (*Document, error) {
	if doc == nil {
		return nil, errors.New("schedule: nil document")
	}
	e.mu.Lock()
	if expectRev >= 0 && expectRev != e.doc.Revision {
		cur := e.doc.Revision
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: have %d, sent %d", ErrStaleRevision, cur, expectRev)
	}
	next := doc.Clone()
	next.Version = SchemaVersion
	next.Revision = e.doc.Revision + 1
	if next.Timezone == "" {
		next.Timezone = e.doc.Timezone
	}
	if err := e.store.Save(next); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	e.doc = next
	out := next.Clone()
	e.mu.Unlock()

	e.log.Info("schedule.updated",
		slog.Int("revision", out.Revision), slog.Int("schedules", len(out.Schedules)))
	e.Wake()
	return out, nil
}

// SetEnabled flips one schedule's enable switch and persists it. It is the
// path both the HA switch and the API toggle take.
func (e *Engine) SetEnabled(id string, on bool) error {
	e.mu.Lock()
	s, ok := e.doc.Find(id)
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("%w: schedule %q", ErrNotFound, id)
	}
	if s.Enabled == on {
		e.mu.Unlock()
		return nil // already there; no write, no revision bump
	}
	next := e.doc.Clone()
	ns, _ := next.Find(id)
	ns.Enabled = on
	next.Revision = e.doc.Revision + 1
	if err := e.store.Save(next); err != nil {
		e.mu.Unlock()
		return err
	}
	e.doc = next
	e.mu.Unlock()

	e.log.Info("schedule.toggled", slog.String("schedule", id), slog.Bool("enabled", on))
	e.Wake()
	return nil
}

// Delete removes a schedule and persists the result.
func (e *Engine) Delete(id string) error {
	e.mu.Lock()
	next := e.doc.Clone()
	idx := -1
	for i := range next.Schedules {
		if next.Schedules[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		e.mu.Unlock()
		return fmt.Errorf("%w: schedule %q", ErrNotFound, id)
	}
	next.Schedules = append(next.Schedules[:idx], next.Schedules[idx+1:]...)
	next.Revision = e.doc.Revision + 1
	if err := e.store.Save(next); err != nil {
		e.mu.Unlock()
		return err
	}
	e.doc = next
	e.mu.Unlock()

	e.log.Info("schedule.deleted", slog.String("schedule", id))
	e.Wake()
	return nil
}

// Run evaluates the schedules and sleeps until the next switch point, waking
// early on Wake. It returns when ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	e.log.Info("schedule.started",
		slog.String("timezone", e.Location().String()),
		slog.Duration("catchup", e.catchup),
		slog.String("store", e.store.Path()))

	for {
		e.Evaluate(ctx)

		d := e.untilNext()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		case <-e.wake:
			timer.Stop()
		}
	}
}

// Evaluate resolves every targeted device and applies what is due. Exported so
// tests (and a manual trigger) can run one cycle without the loop.
func (e *Engine) Evaluate(ctx context.Context) {
	e.mu.RLock()
	doc := e.doc
	loc := e.loc
	e.mu.RUnlock()

	if e.states != nil {
		e.states.PublishScheduleSwitches(ctx, doc.Clone())
	}

	now := e.clock().In(loc)
	cur := SlotAt(now)
	for _, key := range doc.TargetKeys() {
		week := Resolve(doc, key)
		e.evaluateTarget(ctx, doc, key, week, cur, now, loc)
	}
}

// evaluateTarget applies the due action for one target and publishes its state.
func (e *Engine) evaluateTarget(ctx context.Context, doc *Document, targetKey string, week *Week, cur int, now time.Time, loc *time.Location) {
	claim := week.At(cur)

	// The target carries the identity the applier needs (device id plus
	// optional embedded id, or an outdoor serial). It is looked up from the
	// winning schedule, since that is the one whose block is being applied.
	target := targetOf(doc, claim, targetKey)

	st := DeviceState{Active: claim}
	if next, _, ok := week.NextChange(cur); ok {
		st.NextChange = SlotStart(next, now, loc)
	}
	if e.states != nil {
		e.states.PublishScheduleState(ctx, target, st)
	}

	if claim == nil {
		// A gap means "no intervention". Forget the last action so the next
		// block start is applied even if it repeats the previous one.
		e.forget(targetKey)
		return
	}

	sig := claim.Action.Signature()
	if e.lastSignature(targetKey) == sig {
		return // already in force — nothing to write
	}

	// The single rule that covers both the timer firing and a restart: apply
	// when the block start is recent enough. On a timer tick the age is ~0; a
	// daemon restarted hours into a block records the state without writing,
	// so a manual change made in the meantime survives.
	started := SlotStartBefore(claim.Start, now, loc)
	age := now.Sub(started)
	if age > e.catchup {
		e.remember(targetKey, sig)
		e.log.Debug("schedule.seeded",
			slog.String("target", targetKey), slog.String("schedule", claim.ScheduleID),
			slog.Duration("age", age))
		return
	}

	if e.applier == nil {
		e.remember(targetKey, sig)
		return
	}
	if err := e.applier.ApplySchedule(ctx, target, claim.Action); err != nil {
		// A target the daemon has not seen yet is not a failure: the next poll
		// wakes the engine and the catch-up window covers the delay.
		if errors.Is(err, ErrDeviceUnknown) {
			e.log.Debug("schedule.target_not_ready", slog.String("target", targetKey))
			return
		}
		e.log.Warn("schedule.apply_failed",
			slog.String("target", targetKey), slog.String("err", err.Error()))
		return
	}
	e.remember(targetKey, sig)
	e.log.Info("schedule.applied",
		slog.String("target", targetKey),
		slog.String("schedule", claim.ScheduleID),
		slog.String("block", claim.BlockID),
		slog.String("action", sig))
}

// targetOf resolves a target key back to the full target of the winning
// schedule, falling back to a key-only target when the schedule is gone (which
// can only happen if the document changed underneath).
func targetOf(doc *Document, claim *Claim, key string) Target {
	if claim != nil {
		if s, ok := doc.Find(claim.ScheduleID); ok {
			if t, ok := s.TargetFor(key); ok {
				return t
			}
		}
	}
	if serial, ok := OutdoorSerialOf(key); ok {
		return Target{OutdoorSerial: serial}
	}
	return Target{DeviceID: key}
}

// untilNext returns how long to sleep: exactly until the earliest next switch
// point across all devices. Only when there is none at all (no schedules, or a
// week that never changes) does it fall back to the heartbeat — capping a known
// switch point would turn the timer back into polling.
func (e *Engine) untilNext() time.Duration {
	e.mu.RLock()
	doc := e.doc
	loc := e.loc
	e.mu.RUnlock()

	now := e.clock().In(loc)
	cur := SlotAt(now)
	var best time.Time
	for _, key := range doc.TargetKeys() {
		week := Resolve(doc, key)
		next, _, ok := week.NextChange(cur)
		if !ok {
			continue
		}
		if t := SlotStart(next, now, loc); best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		return heartbeat
	}
	d := best.Sub(now)
	if d < time.Second {
		// Never spin: a switch point at or just before "now" has been handled
		// by the evaluation that preceded this call.
		d = time.Second
	}
	return d
}

func (e *Engine) lastSignature(deviceID string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.last[deviceID]
}

func (e *Engine) remember(deviceID, sig string) {
	e.mu.Lock()
	e.last[deviceID] = sig
	e.mu.Unlock()
}

func (e *Engine) forget(deviceID string) {
	e.mu.Lock()
	delete(e.last, deviceID)
	e.mu.Unlock()
}
