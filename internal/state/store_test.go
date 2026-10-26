// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package state

import (
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	s := New(time.Now())
	changes := 0
	s.OnChange(func() { changes++ })

	// A value published before the first poll is kept for when the point
	// is described.
	s.Observe("d1", "climateControl", "room_temp", 21.5, time.Now())
	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("undescribed device shown: %+v", got)
	}
	s.SetPoints(map[string]string{"d1": "BRP069C4x"}, map[string][]Point{"d1": {
		{EmbeddedID: "climateControl", Topic: "room_temp", Name: "Room temperature"},
		{EmbeddedID: "climateControl", Topic: "on_off", Settable: true},
	}})
	s.Observe("d1", "", "online", true, time.Now())

	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Model != "BRP069C4x" || snap[0].Online == nil || !*snap[0].Online {
		t.Fatalf("snapshot = %+v", snap)
	}
	if p := snap[0].Points; len(p) != 2 || p[0].Topic != "on_off" || p[0].Value != nil || p[1].Value != 21.5 {
		t.Errorf("points = %+v", p)
	}
	if _, ok := s.Point("d1", "climateControl", "on_off"); !ok || !s.HasDevice("d1") || s.HasDevice("d2") {
		t.Error("lookup failed")
	}

	s.Observe("d1", "climateControl", "room_temp", nil, time.Now())
	if s.Snapshot()[0].Points[1].Value != nil {
		t.Error("nil did not clear the value")
	}
	// A poll that no longer returns a device drops it.
	s.SetPoints(map[string]string{}, nil)
	if len(s.Snapshot()) != 0 {
		t.Error("device kept")
	}
	if changes != 5 {
		t.Errorf("changes = %d, want 5", changes)
	}
}
