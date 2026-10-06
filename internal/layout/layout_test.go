// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package layout

import (
	"strings"
	"testing"

	hatopic "github.com/SukramJ/go-hamqtt/topic"
)

const (
	dev = "809d41d9-4d42-45fa-af6a-84b512143672"
	emb = "climateControl"
)

// TestTopicsAreTheMQTTSmartHomeStrings pins every topic this package composes
// as a literal, not as a re-derivation of the formula.
//
// The point of collapsing twelve fmt.Sprintf sites into one package (F3 of the
// ADR 0070 phase 8 measurement) is that a drift now happens in one place — so
// that one place has to be nailed down against the strings the README and the
// mqtt-smarthome 2.0 convention promise. A formula compared against itself
// proves nothing.
func TestTopicsAreTheMQTTSmartHomeStrings(t *testing.T) {
	t.Parallel()

	r := New("daikin")
	slot := r.Slot(dev, emb, "room_temperature")

	for _, c := range []struct{ name, got, want string }{
		{"name", r.String(), "daikin"},
		{"connected", r.Connected(), "daikin/connected"},
		{"info", r.Info(), "daikin/info"},
		{"command filter", r.CommandFilter(), "daikin/set/+/+/+"},
		{"online", r.Online(dev), "daikin/status/" + dev + "/online"},
		{"slot item", slot.Item(), dev + "/climateControl/room_temperature"},
		{"slot status", slot.State(), "daikin/status/" + dev + "/climateControl/room_temperature"},
		{"slot set", slot.Command(), "daikin/set/" + dev + "/climateControl/room_temperature"},
		{"slot attributes", slot.Attributes(), "daikin/status/" + dev + "/climateControl/room_temperature/attributes"},
		{"synthetic slot", r.Slot(dev, emb, "hvac_mode").State(), "daikin/status/" + dev + "/climateControl/hvac_mode"},
		{"refresh action", r.Slot(dev, emb, "refresh").Command(), "daikin/set/" + dev + "/climateControl/refresh"},
		{"climate attributes", r.Climate(dev, emb).Attributes(), "daikin/status/" + dev + "/climateControl/climate/attributes"},
		{"schedule status", r.Schedule("werktag").State(), "daikin/status/scheduler/werktag/enabled"},
		{"schedule set", r.Schedule("werktag").Command(), "daikin/set/scheduler/werktag/enabled"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestLegacyTopicsAreTheStringsTheInstalledBaseHasRetained pins the 0.13
// layout the start-up sweep recognises. It is the installed base's own
// retained tree, so it is pinned as literals too.
func TestLegacyTopicsAreTheStringsTheInstalledBaseHasRetained(t *testing.T) {
	t.Parallel()

	l := NewLegacy("daikin")
	slot := l.Slot(dev, emb, "room_temperature")
	for _, c := range []struct{ name, got, want string }{
		{"root", l.String(), "daikin"},
		{"bridge status", l.BridgeStatus(), "daikin/bridge/status"},
		{"slot base", slot.Base(), "daikin/" + dev + "/climateControl/room_temperature"},
		{"slot state", slot.State(), "daikin/" + dev + "/climateControl/room_temperature/state"},
		{"slot command", slot.Command(), "daikin/" + dev + "/climateControl/room_temperature/set"},
		{"slot attributes", slot.Attributes(), "daikin/" + dev + "/climateControl/room_temperature/attributes"},
		{"climate attributes", l.Climate(dev, emb).Attributes(), "daikin/" + dev + "/climateControl/climate/attributes"},
		{"schedule state", l.Schedule("werktag").State(), "daikin/scheduler/werktag/enabled/state"},
		{"schedule command", l.Schedule("werktag").Command(), "daikin/scheduler/werktag/enabled/set"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, leaf := range []string{"state", "set", "attributes"} {
		if !IsLegacyLeaf(leaf) {
			t.Errorf("IsLegacyLeaf(%q) = false", leaf)
		}
	}
	for _, leaf := range []string{"online", "enabled", "", "status"} {
		if IsLegacyLeaf(leaf) {
			t.Errorf("IsLegacyLeaf(%q) = true", leaf)
		}
	}
}

// TestEveryCommandTopicIsCoveredByTheCommandFilterShape pins the structural
// fact the whole layout rests on: every set item has exactly three levels
// below the function, so the one wildcard subscription "<name>/set/+/+/+"
// covers every command topic this bridge can advertise, and the router's three
// wildcards are the device, the management point and the leaf.
//
// A fourth level anywhere would be delivered by no subscription and parsed by
// nothing — silently, because Home Assistant reports a command it published as
// sent.
func TestEveryCommandTopicIsCoveredByTheCommandFilterShape(t *testing.T) {
	t.Parallel()

	r := New("daikin")
	for _, s := range []Slot{
		r.Slot("dev", "emb", "power"),
		r.Schedule("werktag"),
		r.Climate("dev", "emb"),
	} {
		if n := countLevels(s.Command()); n != 5 {
			t.Errorf("%q has %d levels, want 5 — the <name>/set/+/+/+ shape", s.Command(), n)
		}
		if n := countLevels(s.State()); n != 5 {
			t.Errorf("%q has %d levels, want 5", s.State(), n)
		}
		if n := countLevels(s.Attributes()); n != 6 {
			t.Errorf("%q has %d levels, want 6 — one item level below the slot", s.Attributes(), n)
		}
	}
	if n := countLevels(r.CommandFilter()); n != 5 {
		t.Errorf("command filter %q has %d levels, want 5", r.CommandFilter(), n)
	}
}

func countLevels(topic string) int { return strings.Count(topic, "/") + 1 }

// TestEveryStatusTopicSitsUnderAFunction is the property the migration sweep
// relies on to tell new topics from old ones: everything this layout renders
// has a function name at its second level, and the old layout's second level
// is a device id, the scheduler or `bridge`, none of which is one.
func TestEveryStatusTopicSitsUnderAFunction(t *testing.T) {
	t.Parallel()

	r := New("daikin")
	slot := r.Slot("dev", "emb", "power")
	for _, topic := range []string{
		r.Connected(), r.Info(), r.CommandFilter(), r.Online("dev"),
		slot.State(), slot.Command(), slot.Attributes(),
		r.Schedule("werktag").State(), r.Climate("dev", "emb").Attributes(),
	} {
		rest, ok := strings.CutPrefix(topic, "daikin/")
		if !ok {
			t.Errorf("%q is not under the name", topic)
			continue
		}
		fn, _, _ := strings.Cut(rest, "/")
		if !hatopic.IsFunction(fn) {
			t.Errorf("%q: second level %q is not an mqtt-smarthome function", topic, fn)
		}
	}
	for _, seg := range []string{SchedulerDeviceID, LegacyBridgeDevice, dev} {
		if hatopic.IsFunction(seg) {
			t.Errorf("old first-level item %q spells a function name", seg)
		}
	}
}

// TestNameIsHonoured proves the name is not baked in anywhere: MQTT_TOPIC is
// operator-settable, and a topic that ignored it would be published where
// nobody is listening. A multi-level name is kept verbatim and reported as
// outside spec §3.
func TestNameIsHonoured(t *testing.T) {
	t.Parallel()

	r := New("haus/klima")
	if got, want := r.Connected(), "haus/klima/connected"; got != want {
		t.Errorf("Connected = %q, want %q", got, want)
	}
	if got, want := r.Slot("d", "e", "t").State(), "haus/klima/status/d/e/t"; got != want {
		t.Errorf("State = %q, want %q", got, want)
	}
	if got, want := r.Schedule("s").Command(), "haus/klima/set/scheduler/s/enabled"; got != want {
		t.Errorf("Schedule command = %q, want %q", got, want)
	}
	if r.Conformant() {
		t.Error("a multi-level name reports conformant")
	}
	if !New("daikin").Conformant() {
		t.Error("a single-level name reports non-conformant")
	}
	if got, want := NewLegacy("haus/klima").BridgeStatus(), "haus/klima/bridge/status"; got != want {
		t.Errorf("legacy BridgeStatus = %q, want %q", got, want)
	}
}

// TestNewRefusesAnInvalidName pins that an invalid name never renders a topic:
// config.Validate refuses it first, and a caller that bypassed that gets a
// panic rather than a tree of empty strings.
func TestNewRefusesAnInvalidName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "dai+kin", "daikin/#", "$SYS"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%q) did not panic", name)
				}
			}()
			_ = New(name)
		}()
	}
}
