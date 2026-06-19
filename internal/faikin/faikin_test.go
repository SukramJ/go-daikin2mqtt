// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package faikin

import (
	"encoding/json"
	"testing"
)

// realState is a verbatim `state/Klima SZ` payload captured from a live
// Faikout module (revk firmware, app "Faikout").
const realState = `{"ts":"2026-06-19T10:37:06Z","id":"1020BA304320","app":"Faikout","version":"158decaf","online":true,"power":true,"target":22.50,"actarget":22.50,"temp":21.00,"hum":66.00,"achome":21.00,"outside":28.00,"liquid":13.00,"demand":100,"energy":772600,"energyheat":71000,"energycool":117300,"consumption":0,"fanfreq":0.0,"comp":19,"mode":"cool","fan":"auto","streamer":false,"quiet":false,"econo":true,"comfort":false,"powerful":false,"sensor":false,"swing":"off","preset":"eco","autoe":true}`

func TestParseState(t *testing.T) {
	s, err := ParseState("Klima SZ", []byte(realState))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "Klima SZ" {
		t.Errorf("Host = %q", s.Host)
	}
	cases := map[string]struct{ got, want any }{
		"power":      {s.Power, true},
		"mode":       {s.Mode, "cool"},
		"target":     {s.Target, 22.5},
		"temp":       {s.Temp, 21.0},
		"hum":        {s.Hum, 66.0},
		"outside":    {s.Outside, 28.0},
		"fan":        {s.Fan, "auto"},
		"swing":      {s.Swing, "off"},
		"quiet":      {s.Quiet, false},
		"econo":      {s.Econo, true},
		"powerful":   {s.Powerful, false},
		"demand":     {s.Demand, 100},
		"energy":     {s.Energy, int64(772600)},
		"energyheat": {s.EnergyHeat, int64(71000)},
		"energycool": {s.EnergyCool, int64(117300)},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
}

func TestHAMode(t *testing.T) {
	cases := []struct {
		power bool
		mode  string
		want  string
	}{
		{true, "cool", "cool"},
		{true, "heat", "heat"},
		{true, "auto", "heat_cool"},
		{true, "dry", "dry"},
		{true, "fan", "fan_only"},
		{false, "cool", "off"}, // power off wins
		{true, "weird", "off"}, // unknown mode
	}
	for _, c := range cases {
		s := &State{Power: c.power, Mode: c.mode}
		if got := s.HAMode(); got != c.want {
			t.Errorf("HAMode(power=%v mode=%q) = %q, want %q", c.power, c.mode, got, c.want)
		}
	}
}

func TestControlForHAMode(t *testing.T) {
	// off → power:false only
	c, ok := ControlForHAMode("off")
	if !ok || c.Power == nil || *c.Power || c.Mode != nil {
		t.Fatalf("off → %+v ok=%v", c, ok)
	}
	// heat_cool → power:true, mode:auto
	c, ok = ControlForHAMode("heat_cool")
	if !ok || c.Power == nil || !*c.Power || c.Mode == nil || *c.Mode != "auto" {
		t.Fatalf("heat_cool → %+v ok=%v", c, ok)
	}
	if _, ok := ControlForHAMode("bogus"); ok {
		t.Error("bogus mode should not map")
	}

	// Only the set fields are marshalled (partial command).
	raw, _ := c.JSON()
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, has := m["temp"]; has {
		t.Errorf("partial control must omit unset fields, got %s", raw)
	}
	if m["power"] != true || m["mode"] != "auto" {
		t.Errorf("control JSON = %s", raw)
	}
}

func TestTopics(t *testing.T) {
	if got := StateTopic("Klima SZ"); got != "state/Klima SZ" {
		t.Errorf("StateTopic = %q", got)
	}
	if got := CommandTopic("Faikout", "Klima SZ"); got != "Faikout/Klima SZ/command/control" {
		t.Errorf("CommandTopic = %q", got)
	}
}
