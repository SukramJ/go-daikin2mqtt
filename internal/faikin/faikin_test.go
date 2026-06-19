// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package faikin

import (
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
	if !s.HasAC {
		t.Error("full document should set HasAC=true")
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

func TestTopics(t *testing.T) {
	if got := StateTopic("Klima SZ"); got != "state/Klima SZ" {
		t.Errorf("StateTopic = %q", got)
	}
	if got := CommandTopic("Faikout", "Klima SZ", "quiet"); got != "Faikout/Klima SZ/command/quiet" {
		t.Errorf("CommandTopic = %q", got)
	}
}

// osHeartbeat is a verbatim OS/heartbeat document Faikin interleaves on
// state/<host> — it carries no AC fields and must not be treated as state.
const osHeartbeat = `{"ts":"2026-06-19T15:00:00Z","id":"1020BA304320","up":true,"uptime":332961,"mqtt-up":16744,"mem":87788,"spi":2086892,"rssi":-54}`

func TestParseStateSkipsOSHeartbeat(t *testing.T) {
	s, err := ParseState("Klima GA", []byte(osHeartbeat))
	if err != nil {
		t.Fatal(err)
	}
	if s.HasAC {
		t.Error("OS heartbeat must set HasAC=false (no AC fields)")
	}
}

// realStatus is a verbatim `state/Klima SZ/status` (S21) payload captured live.
const realStatus = `{"protocol":"S21","online":true,"home":21.0,"heat":false,"outside":26.5,"hum":67.0,"Whheating":71000,"Whcooling":117300,"power":true,"mode":"C","temp":22.5,"demand":100,"fan":"A","swingh":false,"swingv":false,"econo":true,"powerful":false,"comfort":false,"streamer":false,"sensor":false,"quiet":false}`

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus("Klima SZ", []byte(realStatus))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ got, want any }{
		"HasAC":   {s.HasAC, true},
		"power":   {s.Power, true},
		"mode":    {s.Mode, "cool"}, // S21 "C" -> app "cool"
		"target":  {s.Target, 22.5}, // /status temp = setpoint
		"temp":    {s.Temp, 21.0},   // /status home = room temp
		"outside": {s.Outside, 26.5},
		"hum":     {s.Hum, 67.0},
		"quiet":   {s.Quiet, false},
		"econo":   {s.Econo, true},
		"demand":  {s.Demand, 100},
		"hamode":  {s.HAMode(), "cool"},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
}

func TestStatusTopic(t *testing.T) {
	if got := StatusTopic("Klima SZ"); got != "state/Klima SZ/status" {
		t.Errorf("StatusTopic = %q", got)
	}
}
