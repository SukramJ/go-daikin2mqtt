// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package schedule implements the weekly programme that drives the climate
// devices from the daemon itself: a persisted set of schedules, each holding
// time blocks with a target state, resolved against a 336-slot week ring and
// applied at block boundaries.
//
// The package is deliberately free of I/O beyond its own store and of any
// dependency on the rest of the daemon: it hands a resolved [Action] to an
// [Applier], which the coordinator implements by feeding the regular write
// path. See docs/schedule-design.md for the rationale.
package schedule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Slot geometry. The week is held as a ring of 30-minute slots, which turns
// blocks crossing midnight (and the week boundary) into modulo arithmetic.
const (
	// SlotMinutes is the scheduling resolution.
	SlotMinutes = 30
	// SlotsPerDay is the number of slots in one day.
	SlotsPerDay = 24 * 60 / SlotMinutes
	// SlotsPerWeek is the size of the week ring.
	SlotsPerWeek = 7 * SlotsPerDay
	// MinutesPerDay is one full day in minutes; also the value "24:00" parses to.
	MinutesPerDay = 24 * 60
)

// SchemaVersion is the version written to (and expected in) the store file.
const SchemaVersion = 1

// SchedulerDeviceID is the reserved device id the per-schedule enable switch
// is published under (`<root>/scheduler/<scheduleID>/enabled/…`). It can never
// be a real ONECTA device id, and validation rejects it as a schedule id.
const SchedulerDeviceID = "scheduler"

// Power is a block's on/off intent.
type Power string

// Power values.
const (
	PowerOn  Power = "on"
	PowerOff Power = "off"
)

// HVACMode is the Home Assistant hvac mode a block selects. The values match
// the synthetic hvac_mode topic the coordinator already serves, so no
// translation table is needed on the way out.
type HVACMode string

// HVACMode values.
const (
	ModeHeat    HVACMode = "heat"
	ModeCool    HVACMode = "cool"
	ModeAuto    HVACMode = "auto"
	ModeDry     HVACMode = "dry"
	ModeFanOnly HVACMode = "fan_only"
)

// EndAction is what happens when a block ends and no other block takes over.
type EndAction string

// EndAction values.
const (
	// EndNone leaves the device as the block left it (the default).
	EndNone EndAction = "none"
	// EndOff switches the device off at the end of the block.
	EndOff EndAction = "off"
)

// Weekday keys as stored, Monday first. The order is fixed in every language:
// a weekly heating programme is read as "weekdays vs. weekend", so a
// locale-dependent column order would make the same schedule look different in
// the English and German UI.
var weekdayKeys = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// dayIndex maps a stored weekday key to its ring index (Monday = 0).
func dayIndex(key string) (int, bool) {
	for i, k := range weekdayKeys {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

// DayKey returns the stored key of a Monday-based weekday index.
func DayKey(i int) string {
	if i < 0 || i >= len(weekdayKeys) {
		return ""
	}
	return weekdayKeys[i]
}

// Action is the target state a block establishes at its start. A nil Setpoint
// means "leave the temperature alone", which keeps a pure on/off or mode block
// from overwriting a manually chosen temperature.
type Action struct {
	Power    Power    `json:"power"`
	HVACMode HVACMode `json:"hvac_mode,omitempty"`
	Setpoint *float64 `json:"setpoint,omitempty"`
}

// HVACPayload renders the action as a payload for the synthetic hvac_mode
// topic: "off" when the block switches off, else the HA mode name.
func (a Action) HVACPayload() string {
	if a.Power == PowerOff {
		return "off"
	}
	return string(a.HVACMode)
}

// SetpointPayload renders the setpoint for the temperature_setpoint topic.
// Only valid when Setpoint is non-nil.
func (a Action) SetpointPayload() string {
	if a.Setpoint == nil {
		return ""
	}
	return strconv.FormatFloat(*a.Setpoint, 'f', -1, 64)
}

// Signature is a stable fingerprint of the action, used to suppress writes
// that would not change anything (see the engine's idempotence cache).
func (a Action) Signature() string {
	if a.Power == PowerOff {
		return "off"
	}
	s := "on/" + string(a.HVACMode)
	if a.Setpoint != nil {
		s += "/" + a.SetpointPayload()
	}
	return s
}

// Block is one time range with a target state. Days lists the weekdays the
// block starts on; End may be less than or equal to Start, in which case the
// block runs past midnight into the following day (and past Sunday into
// Monday).
type Block struct {
	ID     string    `json:"id"`
	Label  string    `json:"label,omitempty"`
	Days   []string  `json:"days"`
	Start  string    `json:"start"`
	End    string    `json:"end"`
	Action Action    `json:"action"`
	OnEnd  EndAction `json:"on_end,omitempty"`
}

// StartMinute returns the block's start as minutes past midnight.
func (b *Block) StartMinute() int {
	m, _ := parseClock(b.Start, false)
	return m
}

// Duration returns the block's length in minutes (always > 0; a block whose
// end is not after its start wraps into the next day).
func (b *Block) Duration() int {
	start, _ := parseClock(b.Start, false)
	end, _ := parseClock(b.End, true)
	d := end - start
	if d <= 0 {
		d += MinutesPerDay
	}
	return d
}

// Target is one device a schedule applies to. EmbeddedID is optional: left
// empty, the coordinator uses the device's climateControl management point,
// which is the right answer for every device that exposes a single zone.
type Target struct {
	DeviceID   string `json:"device_id"`
	EmbeddedID string `json:"embedded_id,omitempty"`
}

// Schedule is one weekly programme. Several schedules may target the same
// device; per slot the highest Priority wins (see ring.go).
type Schedule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	Targets  []Target `json:"targets"`
	Blocks   []Block  `json:"blocks"`
}

// Applies reports whether the schedule targets the given device.
func (s *Schedule) Applies(deviceID string) bool {
	for i := range s.Targets {
		if s.Targets[i].DeviceID == deviceID {
			return true
		}
	}
	return false
}

// EmbeddedFor returns the explicit embedded id configured for a device, or ""
// when the coordinator should resolve the climateControl point itself.
func (s *Schedule) EmbeddedFor(deviceID string) string {
	for i := range s.Targets {
		if s.Targets[i].DeviceID == deviceID {
			return s.Targets[i].EmbeddedID
		}
	}
	return ""
}

// Document is the persisted root object.
type Document struct {
	Version int `json:"version"`
	// Revision is bumped on every successful write and echoed by the API, so a
	// stale editor gets a 409 instead of silently overwriting a concurrent edit.
	Revision int `json:"revision"`
	// Timezone is the IANA zone the wall-clock times are interpreted in. Empty
	// means the daemon's local zone.
	Timezone  string     `json:"timezone,omitempty"`
	Schedules []Schedule `json:"schedules"`
}

// NewDocument returns an empty, valid document.
func NewDocument() *Document {
	return &Document{Version: SchemaVersion, Schedules: []Schedule{}}
}

// Clone returns a deep copy, so callers can hand a snapshot to the API layer
// without exposing the engine's live document to mutation.
func (d *Document) Clone() *Document {
	if d == nil {
		return NewDocument()
	}
	out := &Document{Version: d.Version, Revision: d.Revision, Timezone: d.Timezone}
	out.Schedules = make([]Schedule, len(d.Schedules))
	for i := range d.Schedules {
		s := &d.Schedules[i]
		c := *s
		c.Targets = append([]Target(nil), s.Targets...)
		c.Blocks = make([]Block, len(s.Blocks))
		for j := range s.Blocks {
			b := &s.Blocks[j]
			cb := *b
			cb.Days = append([]string(nil), b.Days...)
			if b.Action.Setpoint != nil {
				v := *b.Action.Setpoint
				cb.Action.Setpoint = &v
			}
			c.Blocks[j] = cb
		}
		out.Schedules[i] = c
	}
	return out
}

// Find returns the schedule with the given id.
func (d *Document) Find(id string) (*Schedule, bool) {
	for i := range d.Schedules {
		if d.Schedules[i].ID == id {
			return &d.Schedules[i], true
		}
	}
	return nil, false
}

// DeviceIDs returns every device id referenced by any schedule, sorted, so the
// engine knows which devices it has to evaluate.
func (d *Document) DeviceIDs() []string {
	seen := map[string]bool{}
	var out []string
	for i := range d.Schedules {
		for _, t := range d.Schedules[i].Targets {
			if t.DeviceID == "" || seen[t.DeviceID] {
				continue
			}
			seen[t.DeviceID] = true
			out = append(out, t.DeviceID)
		}
	}
	sort.Strings(out)
	return out
}

// --- validation ------------------------------------------------------------

// ValidationError aggregates every problem found in a document, mirroring
// config.ValidationError so callers can log them all at once.
type ValidationError struct {
	Issues []string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		return "schedule: " + e.Issues[0]
	}
	return fmt.Sprintf("schedule: %d validation issue(s):\n  - %s",
		len(e.Issues), strings.Join(e.Issues, "\n  - "))
}

var (
	validPower = map[Power]bool{PowerOn: true, PowerOff: true}
	validModes = map[HVACMode]bool{
		ModeHeat: true, ModeCool: true, ModeAuto: true, ModeDry: true, ModeFanOnly: true,
	}
	validEnd = map[EndAction]bool{EndNone: true, EndOff: true}
)

// Setpoint bounds. Deliberately wide: they exist to catch a misplaced decimal
// point or a Fahrenheit value, not to model a specific unit's capabilities —
// the device rejects what it cannot do, and Altherma water setpoints legitimately
// sit far from room temperatures.
const (
	MinSetpoint = 5.0
	MaxSetpoint = 60.0
)

// Validate checks the whole document and returns a [*ValidationError] listing
// every problem. Unknown device ids are deliberately not an error: the cloud
// may be unreachable, and a temporary outage must not invalidate schedules.
func (d *Document) Validate() error {
	var issues []string
	add := func(format string, args ...any) {
		issues = append(issues, fmt.Sprintf(format, args...))
	}

	if d.Version != 0 && d.Version != SchemaVersion {
		add("unsupported version %d (this build understands %d)", d.Version, SchemaVersion)
	}

	seenID := map[string]bool{}
	for i := range d.Schedules {
		s := &d.Schedules[i]
		id := s.ID
		if id == "" {
			add("schedule %d: id is required", i)
			id = fmt.Sprintf("#%d", i)
		}
		if s.ID == SchedulerDeviceID {
			add("schedule %d: id %q is reserved", i, SchedulerDeviceID)
		}
		if s.ID != "" && Slug(s.ID) != s.ID {
			add("%s: id must be a slug (lower-case, [a-z0-9_-])", id)
		}
		if seenID[s.ID] {
			add("%s: duplicate schedule id", id)
		}
		seenID[s.ID] = true
		if strings.TrimSpace(s.Name) == "" {
			add("%s: name is required", id)
		}
		if len(s.Targets) == 0 {
			add("%s: at least one target device is required", id)
		}
		for j := range s.Targets {
			if s.Targets[j].DeviceID == "" {
				add("%s: target %d: device_id is required", id, j)
			}
		}
		seenBlock := map[string]bool{}
		for j := range s.Blocks {
			validateBlock(add, id, j, &s.Blocks[j], seenBlock)
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// validateBlock checks one block, reporting problems through add.
func validateBlock(add func(string, ...any), schedID string, idx int, b *Block, seen map[string]bool) {
	where := fmt.Sprintf("%s: block %d", schedID, idx)
	if b.ID == "" {
		add("%s: id is required", where)
	}
	if seen[b.ID] {
		add("%s: duplicate block id %q", where, b.ID)
	}
	seen[b.ID] = true

	if len(b.Days) == 0 {
		add("%s: at least one weekday is required", where)
	}
	seenDay := map[string]bool{}
	for _, day := range b.Days {
		if _, ok := dayIndex(day); !ok {
			add("%s: unknown weekday %q (want mon..sun)", where, day)
			continue
		}
		if seenDay[day] {
			add("%s: duplicate weekday %q", where, day)
		}
		seenDay[day] = true
	}

	start, errStart := parseClock(b.Start, false)
	if errStart != nil {
		add("%s: start %v", where, errStart)
	}
	end, errEnd := parseClock(b.End, true)
	if errEnd != nil {
		add("%s: end %v", where, errEnd)
	}
	if errStart == nil && errEnd == nil && start == end {
		add("%s: start and end are identical (%s); use 00:00–24:00 for a whole day", where, b.Start)
	}

	if !validPower[b.Action.Power] {
		add("%s: power must be on or off, got %q", where, b.Action.Power)
	}
	if b.Action.Power == PowerOn && !validModes[b.Action.HVACMode] {
		add("%s: hvac_mode must be one of [auto cool dry fan_only heat], got %q", where, b.Action.HVACMode)
	}
	if sp := b.Action.Setpoint; sp != nil {
		if *sp < MinSetpoint || *sp > MaxSetpoint {
			add("%s: setpoint must be %.0f..%.0f °C, got %g", where, MinSetpoint, MaxSetpoint, *sp)
		}
	}
	if b.OnEnd != "" && !validEnd[b.OnEnd] {
		add("%s: on_end must be none or off, got %q", where, b.OnEnd)
	}
}

// parseClock parses "HH:MM" into minutes past midnight. Times must sit on the
// 30-minute grid. allow24 permits the special value "24:00" (= end of day),
// which only makes sense for a block's end.
func parseClock(s string, allow24 bool) (int, error) {
	h, m, ok := splitClock(s)
	if !ok {
		return 0, fmt.Errorf("must be HH:MM, got %q", s)
	}
	if h == 24 && m == 0 {
		if !allow24 {
			return 0, fmt.Errorf("must be 00:00..23:30, got %q", s)
		}
		return MinutesPerDay, nil
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("must be a valid time of day, got %q", s)
	}
	if m%SlotMinutes != 0 {
		return 0, fmt.Errorf("must be on a %d-minute boundary, got %q", SlotMinutes, s)
	}
	return h*60 + m, nil
}

// splitClock splits "HH:MM" into its numeric parts without allocating.
func splitClock(s string) (h, m int, ok bool) {
	colon := strings.IndexByte(s, ':')
	if colon < 0 || len(s) != 5 || colon != 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(s[:colon])
	mm, err2 := strconv.Atoi(s[colon+1:])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return hh, mm, true
}

// FormatClock renders minutes past midnight as "HH:MM" ("24:00" for a full day).
func FormatClock(minute int) string {
	if minute == MinutesPerDay {
		return "24:00"
	}
	minute = ((minute % MinutesPerDay) + MinutesPerDay) % MinutesPerDay
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// --- slugs -----------------------------------------------------------------

// umlautReplacer transliterates German umlauts, so "Bürozeit" slugs to
// "burozeit" rather than losing the non-ASCII runes. It deliberately matches
// hass.slugify (which in turn matches Home Assistant's own slugify: ä→a, not
// ae), so a schedule's entity id looks like every other entity id this daemon
// publishes. Keep the two in sync.
var umlautReplacer = strings.NewReplacer(
	"ä", "a", "ö", "o", "ü", "u", "ß", "ss",
)

// Slug derives a stable, ASCII, language-independent id from a display name:
// lower-cased, umlauts transliterated, any run of other characters reduced to a
// single underscore. A schedule's id is generated once at creation and then
// frozen — renaming changes only the name, so the Home Assistant entity id
// survives the rename (HA never renames a registered entity).
func Slug(name string) string {
	s := umlautReplacer.Replace(strings.ToLower(strings.TrimSpace(name)))
	var b strings.Builder
	pendingSep := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)
		case r == '-':
			// Keep hyphens: they are valid in HA object ids and operators use
			// them in names like "eg-wohnzimmer".
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteByte('-')
		default:
			pendingSep = true
		}
	}
	return b.String()
}

// UniqueSlug returns a slug for name that does not collide with taken,
// appending -2, -3, … as needed. An empty or fully non-ASCII name falls back
// to "schedule".
func UniqueSlug(name string, taken map[string]bool) string {
	base := Slug(name)
	if base == "" {
		base = "schedule"
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		cand := base + "-" + strconv.Itoa(n)
		if !taken[cand] {
			return cand
		}
	}
}
