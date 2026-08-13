// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"sort"
	"time"
)

// Claim is a block's hold on one slot of the week ring, carrying enough
// context to explain (in the UI and the status sensor) which schedule won.
type Claim struct {
	ScheduleID   string
	ScheduleName string
	BlockID      string
	Label        string
	Priority     int
	Action       Action
	OnEnd        EndAction
	// Start is the ring index the winning block started at, and Age how many
	// slots ago that was. Age breaks ties between equal priorities: the block
	// that started later is the more recent instruction.
	Start int
	Age   int
}

// Key identifies the winning block. Two adjacent slots with the same key
// belong to the same effective block, so a key change is a switch point.
func (c *Claim) Key() string {
	if c == nil {
		return ""
	}
	return c.ScheduleID + "/" + c.BlockID
}

// Week is the resolved week: one winning claim per 30-minute slot, nil where
// no block applies (a gap means "no intervention").
type Week [SlotsPerWeek]*Claim

// Resolve resolves the whole week for one target (an indoor device id or an
// outdoor key, see [Target.Key]): for every slot the claim of the enabled
// schedule with the highest priority (ties go to the block that started later).
func Resolve(d *Document, targetKey string) *Week {
	var w Week
	if d == nil {
		return &w
	}
	for i := range d.Schedules {
		s := &d.Schedules[i]
		if !s.Enabled || !s.Applies(targetKey) {
			continue
		}
		for j := range s.Blocks {
			claimBlock(&w, s, &s.Blocks[j])
		}
	}
	return &w
}

// claimBlock lets one block stake its claim on every slot it covers.
func claimBlock(w *Week, s *Schedule, b *Block) {
	dur := b.Duration()
	start := b.StartMinute()
	for _, day := range b.Days {
		di, ok := dayIndex(day)
		if !ok {
			continue
		}
		first := di*SlotsPerDay + start/SlotMinutes
		for n := range dur / SlotMinutes {
			slot := (first + n) % SlotsPerWeek
			cand := &Claim{
				ScheduleID:   s.ID,
				ScheduleName: s.Name,
				BlockID:      b.ID,
				Label:        b.Label,
				Priority:     s.Priority,
				Action:       b.Action,
				OnEnd:        b.OnEnd,
				Start:        first % SlotsPerWeek,
				Age:          n,
			}
			if beats(cand, w[slot]) {
				w[slot] = cand
			}
		}
	}
}

// beats reports whether cand wins the slot over cur. Higher priority always
// wins; at equal priority the block that started more recently (smaller Age)
// wins, which is what "last instruction wins" means on a ring.
func beats(cand, cur *Claim) bool {
	switch {
	case cur == nil:
		return true
	case cand.Priority != cur.Priority:
		return cand.Priority > cur.Priority
	default:
		return cand.Age < cur.Age
	}
}

// At returns the claim covering a slot index (nil in a gap).
func (w *Week) At(slot int) *Claim {
	if w == nil {
		return nil
	}
	return w[((slot%SlotsPerWeek)+SlotsPerWeek)%SlotsPerWeek]
}

// NextChange returns the number of slots from `from` to the next slot whose
// winner differs, and that slot's index. ok is false when the whole week
// resolves to the same block (or to nothing), i.e. there is no switch point.
func (w *Week) NextChange(from int) (slot, ahead int, ok bool) {
	if w == nil {
		return 0, 0, false
	}
	cur := w.At(from).Key()
	for n := 1; n <= SlotsPerWeek; n++ {
		s := (from + n) % SlotsPerWeek
		if w.At(s).Key() != cur {
			return s, n, true
		}
	}
	return 0, 0, false
}

// Segment is a run of consecutive slots resolving to the same block — one
// effective block of the calendar view.
type Segment struct {
	Claim *Claim
	// Day is the weekday the segment lies in (Monday = 0), FromMinute and
	// ToMinute its bounds within that day. A block crossing midnight yields
	// one segment per day, which is exactly how the calendar draws it.
	Day        int
	FromMinute int
	ToMinute   int
}

// Segments returns the effective blocks of one day, merging adjacent slots
// with the same winner. Gaps produce no segment.
func (w *Week) Segments(day int) []Segment {
	var out []Segment
	if w == nil || day < 0 || day >= 7 {
		return out
	}
	var run *Segment
	for i := range SlotsPerDay {
		c := w.At(day*SlotsPerDay + i)
		key := c.Key()
		if run != nil && run.Claim.Key() == key {
			run.ToMinute = (i + 1) * SlotMinutes
			continue
		}
		if run != nil {
			out = append(out, *run)
			run = nil
		}
		if c == nil {
			continue
		}
		run = &Segment{
			Claim:      c,
			Day:        day,
			FromMinute: i * SlotMinutes,
			ToMinute:   (i + 1) * SlotMinutes,
		}
	}
	if run != nil {
		out = append(out, *run)
	}
	return out
}

// --- wall-clock mapping ----------------------------------------------------

// SlotAt returns the ring index of the slot containing t (Monday = day 0).
func SlotAt(t time.Time) int {
	day := (int(t.Weekday()) + 6) % 7 // Go: Sunday = 0 → Monday = 0
	return day*SlotsPerDay + (t.Hour()*60+t.Minute())/SlotMinutes
}

// SlotStart returns the wall-clock time at which the given slot next starts,
// at or after `from`. It is built with time.Date in loc rather than by adding
// durations, so a DST transition shifts the schedule with the wall clock
// instead of drifting an hour off.
func SlotStart(slot int, from time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	from = from.In(loc)
	slot = ((slot % SlotsPerWeek) + SlotsPerWeek) % SlotsPerWeek
	day := slot / SlotsPerDay
	minute := (slot % SlotsPerDay) * SlotMinutes

	curDay := (int(from.Weekday()) + 6) % 7
	delta := (day - curDay + 7) % 7
	t := time.Date(from.Year(), from.Month(), from.Day()+delta, 0, minute, 0, 0, loc)
	if t.Before(from) {
		t = time.Date(from.Year(), from.Month(), from.Day()+delta+7, 0, minute, 0, 0, loc)
	}
	return t
}

// SlotStartBefore returns the most recent wall-clock start of the given slot
// at or before `from` — used to decide whether a block start is still inside
// the catch-up window after a restart.
func SlotStartBefore(slot int, from time.Time, loc *time.Location) time.Time {
	t := SlotStart(slot, from, loc)
	if t.After(from) {
		t = t.AddDate(0, 0, -7)
	}
	return t
}

// --- conflicts -------------------------------------------------------------

// Conflict is a window in which the devices of one outdoor unit are scheduled
// into opposite compressor directions — something a multi-split cannot do.
type Conflict struct {
	// Group identifies the outdoor unit (its serial), as passed in.
	Group string
	// FromSlot is inclusive, ToSlot exclusive.
	FromSlot int
	ToSlot   int
	// Heating and Cooling list the device ids on each side, sorted.
	Heating []string
	Cooling []string
}

// Conflicts finds the windows in which members of one outdoor group are
// scheduled to heat and to cool at the same time. Devices that are scheduled
// off (or not scheduled at all) take no side.
//
// It reports intent, not outcome: the runtime mode sync still resolves the
// clash as "last write wins". The point is to warn the operator while editing,
// when the fix is cheap.
func Conflicts(d *Document, group string, deviceIDs []string) []Conflict {
	if d == nil || len(deviceIDs) < 2 {
		return nil
	}
	weeks := make(map[string]*Week, len(deviceIDs))
	for _, id := range deviceIDs {
		weeks[id] = Resolve(d, id)
	}

	var out []Conflict
	var run *Conflict
	for slot := range SlotsPerWeek {
		heat, cool := sides(weeks, deviceIDs, slot)
		if len(heat) == 0 || len(cool) == 0 {
			if run != nil {
				out = append(out, *run)
				run = nil
			}
			continue
		}
		if run == nil {
			run = &Conflict{Group: group, FromSlot: slot}
		}
		run.ToSlot = slot + 1
		run.Heating = union(run.Heating, heat)
		run.Cooling = union(run.Cooling, cool)
	}
	if run != nil {
		out = append(out, *run)
	}
	return out
}

// sides splits the devices scheduled to run in a slot into the heating and
// cooling camps. Dry counts as cooling: it runs the compressor in the cooling
// direction.
func sides(weeks map[string]*Week, deviceIDs []string, slot int) (heat, cool []string) {
	for _, id := range deviceIDs {
		c := weeks[id].At(slot)
		if c == nil || c.Action.Power == PowerOff {
			continue
		}
		switch c.Action.HVACMode {
		case ModeHeat:
			heat = append(heat, id)
		case ModeCool, ModeDry:
			cool = append(cool, id)
		case ModeAuto, ModeFanOnly:
			// auto lets the unit choose and fan_only does not run the
			// compressor: neither commits to a direction.
		}
	}
	return heat, cool
}

// union merges b into a, keeping it sorted and free of duplicates.
func union(a, b []string) []string {
	for _, v := range b {
		i := sort.SearchStrings(a, v)
		if i < len(a) && a[i] == v {
			continue
		}
		a = append(a, "")
		copy(a[i+1:], a[i:])
		a[i] = v
	}
	return a
}
