// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package layout

import "testing"

// TestTopicsAreTheStringsTheInstalledBaseAlreadyUses pins every topic this
// package composes as a literal, not as a re-derivation of the formula.
//
// The point of collapsing twelve fmt.Sprintf sites into one package (F3 of the
// ADR 0070 phase 8 measurement) is that a drift now happens in one place — so
// that one place has to be nailed down against an installed base that already
// has these exact strings retained on its broker. A formula compared against
// itself proves nothing.
func TestTopicsAreTheStringsTheInstalledBaseAlreadyUses(t *testing.T) {
	t.Parallel()

	const (
		dev = "809d41d9-4d42-45fa-af6a-84b512143672"
		emb = "climateControl"
	)
	r := New("daikin")
	slot := r.Slot(dev, emb, "room_temperature")

	for _, c := range []struct{ name, got, want string }{
		{"root", r.String(), "daikin"},
		{"bridge status", r.BridgeStatus(), "daikin/bridge/status"},
		{"command filter", r.CommandFilter(), "daikin/+/+/+/set"},
		{"slot base", slot.Base(), "daikin/" + dev + "/climateControl/room_temperature"},
		{"slot state", slot.State(), "daikin/" + dev + "/climateControl/room_temperature/state"},
		{"slot command", slot.Command(), "daikin/" + dev + "/climateControl/room_temperature/set"},
		{"slot attributes", slot.Attributes(), "daikin/" + dev + "/climateControl/room_temperature/attributes"},
		{"synthetic slot", r.Slot(dev, emb, "hvac_mode").State(), "daikin/" + dev + "/climateControl/hvac_mode/state"},
		{"climate attributes", r.Climate(dev, emb).Attributes(), "daikin/" + dev + "/climateControl/climate/attributes"},
		{"schedule state", r.Schedule("werktag").State(), "daikin/scheduler/werktag/enabled/state"},
		{"schedule command", r.Schedule("werktag").Command(), "daikin/scheduler/werktag/enabled/set"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestEverySlotTopicIsCoveredByTheCommandFilterShape pins the structural fact
// the whole layout rests on: a slot has exactly four levels below nothing, so
// the one wildcard subscription "<root>/+/+/+/set" covers every command topic
// this bridge can advertise, and parseSetTopic's len(parts) == 5 holds.
//
// A fifth level anywhere (a scope prefix, a group segment) would be delivered
// by no subscription and parsed by nothing — silently, because Home Assistant
// reports a command it published as sent.
func TestEverySlotTopicIsCoveredByTheCommandFilterShape(t *testing.T) {
	t.Parallel()

	r := New("daikin")
	for _, s := range []Slot{
		r.Slot("dev", "emb", "power"),
		r.Schedule("werktag"),
		r.Climate("dev", "emb"),
	} {
		for _, topic := range []string{s.State(), s.Command(), s.Attributes()} {
			if n := countLevels(topic); n != 5 {
				t.Errorf("%q has %d levels, want 5 — the <root>/+/+/+/<suffix> shape", topic, n)
			}
		}
	}
	if n := countLevels(r.CommandFilter()); n != 5 {
		t.Errorf("command filter %q has %d levels, want 5", r.CommandFilter(), n)
	}
	if n := countLevels(r.BridgeStatus()); n != 3 {
		t.Errorf("bridge status %q has %d levels, want 3 — it is not a slot", r.BridgeStatus(), n)
	}
}

func countLevels(topic string) int {
	n := 1
	for _, c := range topic {
		if c == '/' {
			n++
		}
	}
	return n
}

// TestRootIsAPrefixOfEverythingItComposes guards the orphan sweep:
// Discovery.IsOwnConfig accepts a retained config only when its state topic is
// under this daemon's root, so a topic composed here that escaped the root
// would make the daemon unable to recognise — and therefore unable to clear —
// its own configs.
func TestRootIsAPrefixOfEverythingItComposes(t *testing.T) {
	t.Parallel()

	r := New("daikin")
	slot := r.Slot("dev", "emb", "power")
	for _, topic := range []string{
		r.BridgeStatus(), r.CommandFilter(),
		slot.Base(), slot.State(), slot.Command(), slot.Attributes(),
		r.Schedule("werktag").State(), r.Climate("dev", "emb").Attributes(),
	} {
		if len(topic) <= len("daikin/") || topic[:len("daikin/")] != "daikin/" {
			t.Errorf("%q is not under the root", topic)
		}
	}
}

// TestRootIsHonoured proves the root is not baked in anywhere: MQTT_TOPIC is
// operator-settable, and a topic that ignored it would be published where
// nobody is listening.
func TestRootIsHonoured(t *testing.T) {
	t.Parallel()

	r := New("haus/klima")
	if got, want := r.BridgeStatus(), "haus/klima/bridge/status"; got != want {
		t.Errorf("BridgeStatus = %q, want %q", got, want)
	}
	if got, want := r.Slot("d", "e", "t").State(), "haus/klima/d/e/t/state"; got != want {
		t.Errorf("State = %q, want %q", got, want)
	}
	if got, want := r.Schedule("s").Command(), "haus/klima/scheduler/s/enabled/set"; got != want {
		t.Errorf("Schedule command = %q, want %q", got, want)
	}
}
