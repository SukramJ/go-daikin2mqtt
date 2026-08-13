// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"strings"
	"testing"
	"time"
)

// slot builds a ring index from a weekday index and a clock time.
func slot(day int, clock string) int {
	m, err := parseClock(clock, false)
	if err != nil {
		panic(err)
	}
	return day*SlotsPerDay + m/SlotMinutes
}

// sched builds a schedule for the resolution tests.
func sched(id string, prio int, devices []string, blocks ...Block) Schedule {
	targets := make([]Target, 0, len(devices))
	for _, d := range devices {
		targets = append(targets, Target{DeviceID: d})
	}
	return Schedule{ID: id, Name: id, Enabled: true, Priority: prio, Targets: targets, Blocks: blocks}
}

// onBlock builds a heating block with a distinguishable setpoint.
func onBlock(id, start, end string, temp float64, days ...string) Block {
	return Block{
		ID: id, Days: days, Start: start, End: end,
		Action: Action{Power: PowerOn, HVACMode: ModeHeat, Setpoint: ptr(temp)},
	}
}

func TestResolveWrapsPastMidnight(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched("night", 0, []string{"dev"}, onBlock("b", "22:30", "05:30", 17.5, "mon")),
	}}
	w := Resolve(d, "dev")

	cases := []struct {
		name string
		slot int
		want bool // covered?
	}{
		{"just before start", slot(0, "22:00"), false},
		{"at start", slot(0, "22:30"), true},
		{"last slot of monday", slot(0, "23:30"), true},
		{"after midnight", slot(1, "00:00"), true},
		{"last covered slot", slot(1, "05:00"), true},
		{"at end (exclusive)", slot(1, "05:30"), false},
	}
	for _, c := range cases {
		got := w.At(c.slot) != nil
		if got != c.want {
			t.Errorf("%s: covered = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResolveWrapsPastWeekStart(t *testing.T) {
	// Sunday night runs into Monday morning — across the ring's seam.
	d := &Document{Schedules: []Schedule{
		sched("night", 0, []string{"dev"}, onBlock("b", "23:00", "05:30", 17.5, "sun")),
	}}
	w := Resolve(d, "dev")

	if w.At(slot(6, "23:00")) == nil {
		t.Error("sunday 23:00 must be covered")
	}
	if w.At(slot(0, "02:00")) == nil {
		t.Error("monday 02:00 must be covered by the sunday-night block")
	}
	if w.At(slot(0, "05:30")) != nil {
		t.Error("monday 05:30 is the exclusive end and must be free")
	}
}

func TestResolveWholeDayBlock(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched("holiday", 0, []string{"dev"}, onBlock("b", "00:00", "24:00", 15, "mon")),
	}}
	w := Resolve(d, "dev")
	for i := range SlotsPerDay {
		if w.At(slot(0, "00:00")+i) == nil {
			t.Fatalf("monday slot %d must be covered by the whole-day block", i)
		}
	}
	if w.At(slot(1, "00:00")) != nil {
		t.Error("tuesday must be free")
	}
}

func TestResolvePriority(t *testing.T) {
	base := sched("base", 0, []string{"dev"}, onBlock("day", "08:00", "16:00", 18.5, "mon"))
	office := sched("office", 10, []string{"dev"}, onBlock("work", "09:00", "12:00", 21.5, "mon"))
	holiday := sched("holiday", 20, []string{"dev"}, onBlock("frost", "00:00", "24:00", 15, "mon"))
	holiday.Enabled = false

	d := &Document{Schedules: []Schedule{base, office, holiday}}
	w := Resolve(d, "dev")

	if got := w.At(slot(0, "08:30")).ScheduleID; got != "base" {
		t.Errorf("08:30 → %s, want base", got)
	}
	if got := w.At(slot(0, "10:00")).ScheduleID; got != "office" {
		t.Errorf("10:00 → %s, want office (higher priority)", got)
	}
	if got := w.At(slot(0, "13:00")).ScheduleID; got != "base" {
		t.Errorf("13:00 → %s, want base again", got)
	}

	// Enabling the holiday layer must take the whole day.
	d.Schedules[2].Enabled = true
	w = Resolve(d, "dev")
	for i := range SlotsPerDay {
		if got := w.At(slot(0, "00:00") + i).ScheduleID; got != "holiday" {
			t.Fatalf("slot %d → %s, want holiday", i, got)
		}
	}
}

func TestResolveTieBreakByRecency(t *testing.T) {
	// Same priority, overlapping: the block that started later wins the shared
	// slots — the more recent instruction.
	d := &Document{Schedules: []Schedule{
		sched("a", 0, []string{"dev"}, onBlock("early", "06:00", "12:00", 20, "mon")),
		sched("b", 0, []string{"dev"}, onBlock("late", "09:00", "10:00", 23, "mon")),
	}}
	w := Resolve(d, "dev")

	if got := w.At(slot(0, "08:00")).BlockID; got != "early" {
		t.Errorf("08:00 → %s, want early", got)
	}
	if got := w.At(slot(0, "09:30")).BlockID; got != "late" {
		t.Errorf("09:30 → %s, want late (started more recently)", got)
	}
	if got := w.At(slot(0, "11:00")).BlockID; got != "early" {
		t.Errorf("11:00 → %s, want early again", got)
	}
}

func TestResolveOnlyTargetedDevices(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched("s", 0, []string{"dev-a"}, onBlock("b", "08:00", "16:00", 21, "mon")),
	}}
	if Resolve(d, "dev-a").At(slot(0, "10:00")) == nil {
		t.Error("targeted device must be covered")
	}
	if Resolve(d, "dev-b").At(slot(0, "10:00")) != nil {
		t.Error("untargeted device must not be covered")
	}
	if Resolve(nil, "dev-a").At(0) != nil {
		t.Error("nil document must resolve to an empty week")
	}
}

func TestResolveSkipsDisabled(t *testing.T) {
	s := sched("s", 0, []string{"dev"}, onBlock("b", "08:00", "16:00", 21, "mon"))
	s.Enabled = false
	d := &Document{Schedules: []Schedule{s}}
	if Resolve(d, "dev").At(slot(0, "10:00")) != nil {
		t.Error("a disabled schedule must not claim any slot")
	}
}

func TestNextChange(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched(
			"s", 0, []string{"dev"},
			onBlock("morning", "06:00", "08:00", 21.5, "mon"),
			onBlock("day", "08:00", "16:00", 18.5, "mon"),
		),
	}}
	w := Resolve(d, "dev")

	// From inside the first block the next change is the second block's start.
	got, ahead, ok := w.NextChange(slot(0, "06:30"))
	if !ok || got != slot(0, "08:00") || ahead != 3 {
		t.Errorf("NextChange from 06:30 = (%d, %d, %v), want (%d, 3, true)", got, ahead, ok, slot(0, "08:00"))
	}
	// From a gap the next change is the first block's start.
	got, _, ok = w.NextChange(slot(0, "05:00"))
	if !ok || got != slot(0, "06:00") {
		t.Errorf("NextChange from 05:00 = (%d, %v), want %d", got, ok, slot(0, "06:00"))
	}
	// The end of the last block is a change too (block → gap).
	got, _, ok = w.NextChange(slot(0, "15:00"))
	if !ok || got != slot(0, "16:00") {
		t.Errorf("NextChange from 15:00 = (%d, %v), want %d", got, ok, slot(0, "16:00"))
	}
}

func TestNextChangeUniformWeek(t *testing.T) {
	// A week that never changes has no switch point at all.
	empty := Resolve(&Document{}, "dev")
	if _, _, ok := empty.NextChange(0); ok {
		t.Error("an empty week must report no switch point")
	}
	d := &Document{Schedules: []Schedule{
		sched("s", 0, []string{"dev"}, onBlock("all", "00:00", "24:00", 20,
			"mon", "tue", "wed", "thu", "fri", "sat", "sun")),
	}}
	if _, _, ok := Resolve(d, "dev").NextChange(0); ok {
		t.Error("a fully covered week with one block must report no switch point")
	}
}

func TestSegments(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched(
			"s", 0, []string{"dev"},
			onBlock("morning", "06:00", "08:00", 21.5, "mon"),
			onBlock("night", "22:30", "05:30", 17.5, "mon"),
		),
	}}
	w := Resolve(d, "dev")

	mon := w.Segments(0)
	if len(mon) != 2 {
		t.Fatalf("monday segments = %d, want 2: %+v", len(mon), mon)
	}
	if mon[0].FromMinute != 360 || mon[0].ToMinute != 480 {
		t.Errorf("first segment = %d..%d, want 360..480", mon[0].FromMinute, mon[0].ToMinute)
	}
	if mon[1].FromMinute != 1350 || mon[1].ToMinute != MinutesPerDay {
		t.Errorf("second segment = %d..%d, want 1350..1440", mon[1].FromMinute, mon[1].ToMinute)
	}

	// The night block continues into Tuesday as its own segment.
	tue := w.Segments(1)
	if len(tue) != 1 {
		t.Fatalf("tuesday segments = %d, want 1: %+v", len(tue), tue)
	}
	if tue[0].FromMinute != 0 || tue[0].ToMinute != 330 {
		t.Errorf("tuesday segment = %d..%d, want 0..330", tue[0].FromMinute, tue[0].ToMinute)
	}
	if tue[0].Claim.BlockID != "night" {
		t.Errorf("tuesday segment block = %s, want night", tue[0].Claim.BlockID)
	}

	if got := w.Segments(9); got != nil {
		t.Errorf("Segments(9) = %+v, want nil for an out-of-range day", got)
	}
}

func TestSlotAt(t *testing.T) {
	cases := []struct {
		when string
		want int
	}{
		{"2026-08-10T00:00:00Z", 0},                // Monday 00:00
		{"2026-08-10T05:45:00Z", slot(0, "05:30")}, // rounds down into its slot
		{"2026-08-16T23:59:00Z", SlotsPerWeek - 1}, // Sunday 23:30 slot
	}
	for _, c := range cases {
		ts, err := time.Parse(time.RFC3339, c.when)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := SlotAt(ts); got != c.want {
			t.Errorf("SlotAt(%s) = %d, want %d", c.when, got, c.want)
		}
	}
}

func TestSlotStart(t *testing.T) {
	loc := time.UTC
	// Wednesday 2026-08-12, 10:00.
	from := time.Date(2026, 8, 12, 10, 0, 0, 0, loc)

	// Later the same day.
	got := SlotStart(slot(2, "18:00"), from, loc)
	if want := time.Date(2026, 8, 12, 18, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("same day: %v, want %v", got, want)
	}
	// Earlier the same weekday → next week.
	got = SlotStart(slot(2, "06:00"), from, loc)
	if want := time.Date(2026, 8, 19, 6, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("wrap to next week: %v, want %v", got, want)
	}
	// A later weekday this week.
	got = SlotStart(slot(4, "07:30"), from, loc)
	if want := time.Date(2026, 8, 14, 7, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("later weekday: %v, want %v", got, want)
	}
	// Exactly now counts as "not before", so it is not pushed a week out.
	got = SlotStart(slot(2, "10:00"), from, loc)
	if !got.Equal(from) {
		t.Errorf("exact now: %v, want %v", got, from)
	}
}

func TestSlotStartBefore(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 8, 12, 10, 0, 0, 0, loc) // Wednesday
	got := SlotStartBefore(slot(2, "06:00"), from, loc)
	if want := time.Date(2026, 8, 12, 6, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("SlotStartBefore = %v, want %v", got, want)
	}
	// A slot later today last started a week ago.
	got = SlotStartBefore(slot(2, "18:00"), from, loc)
	if want := time.Date(2026, 8, 5, 18, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("SlotStartBefore (later today) = %v, want %v", got, want)
	}
}

func TestSlotStartAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Spring forward: 2026-03-29 02:00 → 03:00, so 02:30 does not exist.
	// time.Date normalises it to 03:30 CEST; the schedule follows the wall
	// clock rather than drifting an hour.
	from := time.Date(2026, 3, 29, 1, 0, 0, 0, loc)
	got := SlotStart(slot(6, "02:30"), from, loc)
	if h, m := got.Hour(), got.Minute(); h != 3 || m != 30 {
		t.Errorf("missing hour: got %v (%02d:%02d), want 03:30 local", got, h, m)
	}

	// Autumn: 2026-10-25 03:00 → 02:00, so 02:30 happens twice. The first
	// occurrence is the one a wall-clock schedule fires on.
	from = time.Date(2026, 10, 25, 1, 0, 0, 0, loc)
	got = SlotStart(slot(6, "02:30"), from, loc)
	if h, m := got.Hour(), got.Minute(); h != 2 || m != 30 {
		t.Errorf("doubled hour: got %v (%02d:%02d), want 02:30 local", got, h, m)
	}
	if !got.After(from) {
		t.Errorf("doubled hour: %v must be after %v", got, from)
	}

	// A day with a DST transition is still exactly one calendar day away: the
	// next Monday 06:00 must be Monday, not Sunday 23:00 or Monday 07:00.
	from = time.Date(2026, 3, 28, 12, 0, 0, 0, loc) // Saturday
	got = SlotStart(slot(0, "06:00"), from, loc)
	if got.Day() != 30 || got.Hour() != 6 {
		t.Errorf("across the transition: got %v, want 2026-03-30 06:00 local", got)
	}
}

func TestConflicts(t *testing.T) {
	// One outdoor unit, two indoor units: the bedroom cools at night while the
	// living room heats — the compressor cannot do both.
	d := &Document{Schedules: []Schedule{
		sched("heat", 0, []string{"living"}, onBlock("night", "22:00", "06:00", 17.5, "mon")),
		sched("cool", 0, []string{"bed"}, Block{
			ID: "cool", Days: []string{"mon"}, Start: "23:00", End: "04:00",
			Action: Action{Power: PowerOn, HVACMode: ModeCool, Setpoint: ptr(25)},
		}),
	}}

	got := Conflicts(d, "0J723746", []string{"living", "bed"})
	if len(got) != 1 {
		t.Fatalf("conflicts = %d, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.FromSlot != slot(0, "23:00") || c.ToSlot != slot(1, "04:00") {
		t.Errorf("window = %d..%d, want %d..%d", c.FromSlot, c.ToSlot, slot(0, "23:00"), slot(1, "04:00"))
	}
	if strings.Join(c.Heating, ",") != "living" || strings.Join(c.Cooling, ",") != "bed" {
		t.Errorf("sides = heat %v / cool %v", c.Heating, c.Cooling)
	}
}

func TestConflictsIgnoresOffAndNeutralModes(t *testing.T) {
	cases := []struct {
		name string
		bed  Action
		want int
	}{
		{"cooling conflicts", Action{Power: PowerOn, HVACMode: ModeCool, Setpoint: ptr(25)}, 1},
		{"dry conflicts too", Action{Power: PowerOn, HVACMode: ModeDry}, 1},
		{"off takes no side", Action{Power: PowerOff}, 0},
		{"fan_only takes no side", Action{Power: PowerOn, HVACMode: ModeFanOnly}, 0},
		{"auto takes no side", Action{Power: PowerOn, HVACMode: ModeAuto}, 0},
		{"heating agrees", Action{Power: PowerOn, HVACMode: ModeHeat, Setpoint: ptr(21)}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Document{Schedules: []Schedule{
				sched("heat", 0, []string{"living"}, onBlock("day", "08:00", "16:00", 21, "mon")),
				sched("other", 0, []string{"bed"}, Block{
					ID: "b", Days: []string{"mon"}, Start: "09:00", End: "12:00", Action: c.bed,
				}),
			}}
			if got := Conflicts(d, "g", []string{"living", "bed"}); len(got) != c.want {
				t.Errorf("conflicts = %d, want %d: %+v", len(got), c.want, got)
			}
		})
	}
}

func TestConflictsNeedsTwoDevices(t *testing.T) {
	d := &Document{Schedules: []Schedule{
		sched("s", 0, []string{"only"}, onBlock("b", "08:00", "16:00", 21, "mon")),
	}}
	if got := Conflicts(d, "g", []string{"only"}); got != nil {
		t.Errorf("a single-device group cannot conflict, got %+v", got)
	}
	if got := Conflicts(nil, "g", []string{"a", "b"}); got != nil {
		t.Errorf("nil document = %+v, want nil", got)
	}
}

func TestClaimKey(t *testing.T) {
	var nilClaim *Claim
	if got := nilClaim.Key(); got != "" {
		t.Errorf("nil claim key = %q, want empty", got)
	}
	c := &Claim{ScheduleID: "s", BlockID: "b"}
	if got := c.Key(); got != "s/b" {
		t.Errorf("key = %q, want s/b", got)
	}
}
