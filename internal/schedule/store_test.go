// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// A nested path also exercises the directory creation.
	s := NewStore(filepath.Join(dir, "sub", "schedules.json"))

	in := doc(block("b1", "05:30", "08:00", "mon", "tue"))
	in.Revision = 7
	in.Timezone = "Europe/Berlin"
	if err := s.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Revision != 7 || out.Timezone != "Europe/Berlin" || out.Version != SchemaVersion {
		t.Fatalf("Load: got %+v", out)
	}
	if len(out.Schedules) != 1 || len(out.Schedules[0].Blocks) != 1 {
		t.Fatalf("Load: schedules = %+v", out.Schedules)
	}
	b := out.Schedules[0].Blocks[0]
	if b.Start != "05:30" || b.End != "08:00" || len(b.Days) != 2 {
		t.Errorf("Load: block = %+v", b)
	}
	if b.Action.Setpoint == nil || *b.Action.Setpoint != 21 {
		t.Errorf("Load: setpoint = %v", b.Action.Setpoint)
	}
}

func TestStoreLoadMissingFile(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nope.json"))
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load of a missing file must not fail: %v", err)
	}
	if got == nil || len(got.Schedules) != 0 || got.Version != SchemaVersion {
		t.Fatalf("Load of a missing file = %+v, want an empty document", got)
	}
}

func TestStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	if err := NewStore(path).Save(doc(block("b1", "05:30", "08:00", "mon"))); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}
}

func TestStoreSaveReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	s := NewStore(path)

	first := doc(block("b1", "05:30", "08:00", "mon"))
	if err := s.Save(first); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second := doc(block("b2", "18:00", "22:00", "sat"))
	second.Revision = 2
	if err := s.Save(second); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out.Schedules[0].Blocks) != 1 || out.Schedules[0].Blocks[0].ID != "b2" {
		t.Errorf("Load after replace: %+v", out.Schedules[0].Blocks)
	}
	// No temp files must survive a successful save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestStoreSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	bad := doc(block("b1", "05:20", "08:00", "mon")) // off the 30-minute grid
	if err := NewStore(path).Save(bad); err == nil {
		t.Fatal("Save: want a validation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Save: an invalid document must not create the file")
	}
	if err := NewStore(path).Save(nil); err == nil {
		t.Error("Save(nil): want an error")
	}
}

func TestStoreLoadRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, body, issue string
	}{
		{name: "not json", body: "{", issue: "decode"},
		{
			name:  "invalid content",
			body:  `{"version":1,"schedules":[{"id":"x","name":"","enabled":true,"targets":[],"blocks":[]}]}`,
			issue: "invalid store",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := NewStore(path).Load()
			if err == nil || !strings.Contains(err.Error(), c.issue) {
				t.Fatalf("Load: want error containing %q, got %v", c.issue, err)
			}
		})
	}
}

func TestStoreLoadDefaultsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	// A hand-written file may omit version and schedules entirely.
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", got.Version, SchemaVersion)
	}
	if got.Schedules == nil {
		t.Error("schedules must be an empty slice, not nil")
	}
}
