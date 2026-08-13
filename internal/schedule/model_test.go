// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"errors"
	"strings"
	"testing"
)

func ptr(f float64) *float64 { return &f }

// block builds a minimal valid block for tests.
func block(id, start, end string, days ...string) Block {
	return Block{
		ID:     id,
		Days:   days,
		Start:  start,
		End:    end,
		Action: Action{Power: PowerOn, HVACMode: ModeHeat, Setpoint: ptr(21)},
	}
}

// doc builds a minimal valid document around the given blocks.
func doc(blocks ...Block) *Document {
	return &Document{
		Version: SchemaVersion,
		Schedules: []Schedule{{
			ID:      "werktag",
			Name:    "Werktag",
			Enabled: true,
			Targets: []Target{{DeviceID: "dev-1"}},
			Blocks:  blocks,
		}},
	}
}

func TestParseClock(t *testing.T) {
	cases := []struct {
		in      string
		allow24 bool
		want    int
		wantErr bool
	}{
		{in: "00:00", want: 0},
		{in: "05:30", want: 330},
		{in: "23:30", want: 1410},
		{in: "24:00", allow24: true, want: MinutesPerDay},
		{in: "24:00", wantErr: true},  // only valid as an end
		{in: "05:15", wantErr: true},  // off the 30-minute grid
		{in: "5:30", wantErr: true},   // not zero-padded
		{in: "25:00", wantErr: true},  // not a time of day
		{in: "05:60", wantErr: true},  // minute out of range
		{in: "", wantErr: true},       // empty
		{in: "0530", wantErr: true},   // no colon
		{in: "ab:cd", wantErr: true},  // not numeric
		{in: "05:30 ", wantErr: true}, // trailing space
	}
	for _, c := range cases {
		got, err := parseClock(c.in, c.allow24)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseClock(%q, %v) = %d, want error", c.in, c.allow24, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseClock(%q, %v): unexpected error %v", c.in, c.allow24, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseClock(%q, %v) = %d, want %d", c.in, c.allow24, got, c.want)
		}
	}
}

func TestFormatClock(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "00:00"},
		{330, "05:30"},
		{1410, "23:30"},
		{MinutesPerDay, "24:00"},
		{MinutesPerDay + 30, "00:30"}, // wraps
	}
	for _, c := range cases {
		if got := FormatClock(c.in); got != c.want {
			t.Errorf("FormatClock(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBlockDuration(t *testing.T) {
	cases := []struct {
		start, end string
		want       int
	}{
		{"05:30", "08:00", 150},
		{"22:30", "05:30", 420},           // wraps past midnight
		{"00:00", "24:00", MinutesPerDay}, // whole day
		{"23:00", "00:00", 60},            // ends exactly at midnight
	}
	for _, c := range cases {
		b := block("b", c.start, c.end, "mon")
		if got := b.Duration(); got != c.want {
			t.Errorf("Duration(%s→%s) = %d, want %d", c.start, c.end, got, c.want)
		}
	}
}

func TestSlug(t *testing.T) {
	// Transliteration must match hass.slugify (ä→a, not ae) so schedule entity
	// ids look like every other entity id the daemon publishes.
	cases := []struct{ in, want string }{
		{"Werktag", "werktag"},
		{"Bürozeit", "burozeit"},
		{"Urlaub / Reise", "urlaub_reise"},
		{"Außer Haus", "ausser_haus"},
		{"  Nacht  ", "nacht"},
		{"eg-wohnzimmer", "eg-wohnzimmer"},
		{"2. Stock", "2_stock"},
		{"...", ""},
	}
	for _, c := range cases {
		if got := Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUniqueSlug(t *testing.T) {
	taken := map[string]bool{"werktag": true, "werktag-2": true}
	if got := UniqueSlug("Werktag", taken); got != "werktag-3" {
		t.Errorf("UniqueSlug = %q, want werktag-3", got)
	}
	if got := UniqueSlug("Nacht", taken); got != "nacht" {
		t.Errorf("UniqueSlug = %q, want nacht", got)
	}
	// A name with nothing slug-worthy still yields a usable id.
	if got := UniqueSlug("!!!", nil); got != "schedule" {
		t.Errorf("UniqueSlug = %q, want schedule", got)
	}
}

func TestActionSignature(t *testing.T) {
	cases := []struct {
		a    Action
		want string
	}{
		{Action{Power: PowerOff}, "off"},
		// An "off" block ignores mode and setpoint, so they must not enter the
		// signature — otherwise two identical off-blocks would look different.
		{Action{Power: PowerOff, HVACMode: ModeHeat, Setpoint: ptr(21)}, "off"},
		{Action{Power: PowerOn, HVACMode: ModeHeat}, "on/heat"},
		{Action{Power: PowerOn, HVACMode: ModeHeat, Setpoint: ptr(21.5)}, "on/heat/21.5"},
	}
	for _, c := range cases {
		if got := c.a.Signature(); got != c.want {
			t.Errorf("Signature(%+v) = %q, want %q", c.a, got, c.want)
		}
	}
}

func TestActionPayloads(t *testing.T) {
	on := Action{Power: PowerOn, HVACMode: ModeCool, Setpoint: ptr(24)}
	if got := on.HVACPayload(); got != "cool" {
		t.Errorf("HVACPayload = %q, want cool", got)
	}
	if got := on.SetpointPayload(); got != "24" {
		t.Errorf("SetpointPayload = %q, want 24", got)
	}
	off := Action{Power: PowerOff, HVACMode: ModeCool}
	if got := off.HVACPayload(); got != "off" {
		t.Errorf("HVACPayload = %q, want off", got)
	}
	if got := (Action{Power: PowerOn}).SetpointPayload(); got != "" {
		t.Errorf("SetpointPayload without setpoint = %q, want empty", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		doc   *Document
		issue string // substring expected in the error; empty = must pass
	}{
		{name: "valid", doc: doc(block("b1", "05:30", "08:00", "mon", "fri"))},
		{
			name:  "unknown weekday",
			doc:   doc(block("b1", "05:30", "08:00", "montag")),
			issue: "unknown weekday",
		},
		{
			name:  "no weekday",
			doc:   doc(block("b1", "05:30", "08:00")),
			issue: "at least one weekday",
		},
		{
			name:  "duplicate weekday",
			doc:   doc(block("b1", "05:30", "08:00", "mon", "mon")),
			issue: "duplicate weekday",
		},
		{
			name:  "off-grid start",
			doc:   doc(block("b1", "05:20", "08:00", "mon")),
			issue: "30-minute boundary",
		},
		{
			name:  "identical start and end",
			doc:   doc(block("b1", "05:30", "05:30", "mon")),
			issue: "identical",
		},
		{
			name:  "duplicate block id",
			doc:   doc(block("b1", "05:30", "08:00", "mon"), block("b1", "09:00", "10:00", "mon")),
			issue: "duplicate block id",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.doc.Validate()
			if c.issue == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: want error containing %q, got nil", c.issue)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate: want *ValidationError, got %T", err)
			}
			if !strings.Contains(err.Error(), c.issue) {
				t.Errorf("Validate: error %q does not contain %q", err.Error(), c.issue)
			}
		})
	}
}

func TestValidateSchedule(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Document)
		issue string
	}{
		{name: "missing name", mut: func(d *Document) { d.Schedules[0].Name = "" }, issue: "name is required"},
		{name: "missing id", mut: func(d *Document) { d.Schedules[0].ID = "" }, issue: "id is required"},
		{name: "non-slug id", mut: func(d *Document) { d.Schedules[0].ID = "Werk Tag" }, issue: "must be a slug"},
		{name: "reserved id", mut: func(d *Document) { d.Schedules[0].ID = SchedulerDeviceID }, issue: "reserved"},
		{name: "no targets", mut: func(d *Document) { d.Schedules[0].Targets = nil }, issue: "at least one target"},
		{
			name:  "empty device id",
			mut:   func(d *Document) { d.Schedules[0].Targets = []Target{{}} },
			issue: "device_id is required",
		},
		{
			name:  "bad power",
			mut:   func(d *Document) { d.Schedules[0].Blocks[0].Action.Power = "maybe" },
			issue: "power must be",
		},
		{
			name:  "bad mode",
			mut:   func(d *Document) { d.Schedules[0].Blocks[0].Action.HVACMode = "warm" },
			issue: "hvac_mode must be",
		},
		{
			name:  "setpoint out of range",
			mut:   func(d *Document) { d.Schedules[0].Blocks[0].Action.Setpoint = ptr(215) },
			issue: "setpoint must be",
		},
		{
			name:  "bad on_end",
			mut:   func(d *Document) { d.Schedules[0].Blocks[0].OnEnd = "maybe" },
			issue: "on_end must be",
		},
		{name: "bad version", mut: func(d *Document) { d.Version = 99 }, issue: "unsupported version"},
		{
			// An off block needs no mode: it switches the device off and leaves
			// mode and temperature alone.
			name: "off block without mode passes",
			mut: func(d *Document) {
				d.Schedules[0].Blocks[0].Action = Action{Power: PowerOff}
			},
		},
		{
			name: "duplicate schedule id",
			mut: func(d *Document) {
				d.Schedules = append(d.Schedules, d.Schedules[0])
			},
			issue: "duplicate schedule id",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := doc(block("b1", "05:30", "08:00", "mon"))
			c.mut(d)
			err := d.Validate()
			if c.issue == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.issue) {
				t.Fatalf("Validate: want error containing %q, got %v", c.issue, err)
			}
		})
	}
}

func TestClone(t *testing.T) {
	orig := doc(block("b1", "05:30", "08:00", "mon"))
	orig.Revision = 3
	c := orig.Clone()

	c.Schedules[0].Name = "changed"
	c.Schedules[0].Blocks[0].Days[0] = "sun"
	*c.Schedules[0].Blocks[0].Action.Setpoint = 30
	c.Schedules[0].Targets[0].DeviceID = "other"

	if orig.Schedules[0].Name != "Werktag" {
		t.Error("Clone: name is shared with the original")
	}
	if orig.Schedules[0].Blocks[0].Days[0] != "mon" {
		t.Error("Clone: days slice is shared with the original")
	}
	if *orig.Schedules[0].Blocks[0].Action.Setpoint != 21 {
		t.Error("Clone: setpoint pointer is shared with the original")
	}
	if orig.Schedules[0].Targets[0].DeviceID != "dev-1" {
		t.Error("Clone: targets slice is shared with the original")
	}
	if c.Revision != 3 {
		t.Errorf("Clone: revision = %d, want 3", c.Revision)
	}
	// A nil document clones to an empty, usable one.
	if got := (*Document)(nil).Clone(); got == nil || len(got.Schedules) != 0 {
		t.Error("Clone of nil: want empty document")
	}
}

func TestDeviceIDs(t *testing.T) {
	d := doc(block("b1", "05:30", "08:00", "mon"))
	d.Schedules[0].Targets = []Target{{DeviceID: "b"}, {DeviceID: "a"}, {DeviceID: "b"}}
	d.Schedules = append(d.Schedules, Schedule{
		ID: "urlaub", Name: "Urlaub", Targets: []Target{{DeviceID: "c"}, {DeviceID: "a"}},
	})
	got := d.DeviceIDs()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("DeviceIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DeviceIDs = %v, want %v", got, want)
		}
	}
}

func TestEmbeddedFor(t *testing.T) {
	s := Schedule{Targets: []Target{{DeviceID: "a", EmbeddedID: "climateControlMainZone"}, {DeviceID: "b"}}}
	if got := s.EmbeddedFor("a"); got != "climateControlMainZone" {
		t.Errorf("EmbeddedFor(a) = %q", got)
	}
	if got := s.EmbeddedFor("b"); got != "" {
		t.Errorf("EmbeddedFor(b) = %q, want empty (resolve from the device)", got)
	}
	if got := s.EmbeddedFor("zzz"); got != "" {
		t.Errorf("EmbeddedFor(unknown) = %q, want empty", got)
	}
}

func TestApplies(t *testing.T) {
	s := Schedule{Targets: []Target{{DeviceID: "a"}, {DeviceID: "b"}}}
	if !s.Applies("a") || !s.Applies("b") {
		t.Error("Applies: want true for both targets")
	}
	if s.Applies("c") {
		t.Error("Applies: want false for an untargeted device")
	}
}
