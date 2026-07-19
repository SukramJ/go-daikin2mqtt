// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
)

func TestGroupMembers(t *testing.T) {
	c := newCoordinator(t, &stubCloud{}, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1", "c": "OD2"}

	if got := c.groupMembers("a"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("group of a = %v, want [a b]", got)
	}
	if got := c.groupMembers("c"); !slices.Equal(got, []string{"c"}) {
		t.Errorf("group of c = %v, want [c]", got)
	}
	// Unknown / no-serial device is its own singleton group.
	if got := c.groupMembers("x"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("group of x = %v, want [x]", got)
	}
}

func TestSyncModeToGroup(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT()) // mode sync defaults on
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}
	// b is running in the opposite compressor direction → conflict, gets synced.
	c.powerCache = map[string]bool{"b": true}
	c.modeCache = map[string]string{"b/cc": "heating"}

	c.syncModeToGroup(context.Background(), "a", "cooling")

	if cloud.patchCount() != 1 {
		t.Fatalf("expected 1 propagated patch (to b only), got %d", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.deviceID != "b" || p.characteristic != "operationMode" || p.value != "cooling" {
		t.Errorf("propagated patch = %+v, want b/operationMode/cooling", p)
	}
	// The propagated write updates b's cached mode (last-wins must chain
	// correctly before the next poll refreshes the cache).
	if m, ok := c.cachedMode("b", "cc"); !ok || m != "cooling" {
		t.Errorf("cached mode of b = %q (%v), want cooling", m, ok)
	}
}

func TestSyncModeNeverWakesOffOrUnknownUnits(t *testing.T) {
	// b is known to be off, c has never been seen: neither may be written to.
	// (The local Faikin `mode` command force-powers a unit on, so a blind sync
	// would switch on the whole house.)
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1", "c": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc", "c": "cc"}
	c.powerCache = map[string]bool{"b": false}
	c.modeCache = map[string]string{"b/cc": "heating", "c/cc": "heating"}

	c.syncModeToGroup(context.Background(), "a", "cooling")

	if cloud.patchCount() != 0 {
		t.Errorf("off/unknown units must not be patched, got %v", cloud.allPatches())
	}
}

func TestSyncModeOnlyOnCompressorConflict(t *testing.T) {
	cases := []struct {
		name       string
		newMode    string // just-commanded mode on a
		memberMode string // b's current mode (b is running)
		wantSynced bool
	}{
		{"heating vs cooling", "cooling", "heating", true},
		{"cooling vs heating", "heating", "cooling", true},
		{"dry conflicts like cooling", "heating", "dry", true},
		{"same mode", "cooling", "cooling", false},
		{"same family", "cooling", "dry", false},
		{"auto member left alone", "cooling", "auto", false},
		{"fanOnly member left alone", "cooling", "fanOnly", false},
		{"auto origin syncs nothing", "auto", "heating", false},
		{"fanOnly origin syncs nothing", "fanOnly", "heating", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &stubCloud{}
			c := newCoordinator(t, cloud, newStubMQTT())
			c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
			c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}
			c.powerCache = map[string]bool{"b": true}
			c.modeCache = map[string]string{"b/cc": tc.memberMode}

			c.syncModeToGroup(context.Background(), "a", tc.newMode)

			want := 0
			if tc.wantSynced {
				want = 1
			}
			if cloud.patchCount() != want {
				t.Errorf("patches = %v, want %d", cloud.allPatches(), want)
			}
		})
	}
}

func TestSyncModeDisabled(t *testing.T) {
	off := false
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.deps.Cfg.MultiSplitModeSync = &off
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}

	c.syncModeToGroup(context.Background(), "a", "cooling")
	if cloud.patchCount() != 0 {
		t.Errorf("mode sync disabled should not patch, got %d", cloud.patchCount())
	}
}

func TestSyncModeLocalNeverWakesOffUnits(t *testing.T) {
	// Regression for the local-first path: Faikin's `command/<host>/mode`
	// force-powers the unit on (power=1 for any mode value except "off"), so the
	// mode sync used to switch on every other indoor unit of the group. An off
	// member must receive no mode command at all; a running, conflicting member
	// still gets one.
	cloud := &stubCloud{}
	faikin := newStubMQTT()
	cfg := &config.Config{
		MQTTTopic:         "daikin",
		Language:          "de",
		LocalMode:         true,
		LocalFaikinPrefix: "Faikout",
		LocalDeviceMap:    map[string]string{"a": "Klima SZ", "b": "Klima WZ", "c": "Klima KZ"},
	}
	c := New(Deps{
		Cfg: cfg, Client: cloud, MQTT: newStubMQTT(), FaikinMQTT: faikin,
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1", "c": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc", "c": "cc"}
	c.powerCache = map[string]bool{"b": true, "c": false} // b heats, c is off
	c.modeCache = map[string]string{"b/cc": "heating", "c/cc": "heating"}

	c.syncModeToGroup(context.Background(), "a", "cooling")

	if msg, ok := faikin.get("command/Klima WZ/mode"); !ok || msg.payload != "C" {
		t.Errorf("running conflicting member: mode command = %q (%v), want \"C\"", msg.payload, ok)
	}
	if msg, ok := faikin.get("command/Klima KZ/mode"); ok {
		t.Errorf("off member must not receive a mode command (would power it on), got %q", msg.payload)
	}
	if cloud.patchCount() != 0 {
		t.Errorf("no cloud patches expected in local mode, got %v", cloud.allPatches())
	}
}

func TestEnforceMutualExclusive(t *testing.T) {
	// econo on → powerful cleared (the only blind-clear direction; powerful→econo
	// is handled by reconcileEconoSuspend so econo can be restored later).
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.enforceMutualExclusive(context.Background(), "a", "cc", "econoMode", "on")
	if cloud.patchCount() != 1 {
		t.Fatalf("expected powerful clear, got %d patches", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.characteristic != "powerfulMode" || p.value != "off" {
		t.Errorf("clear = %+v, want powerfulMode/off", p)
	}

	// powerful on no longer triggers a blind econo clear here (reconcile owns it).
	cloudPw := &stubCloud{}
	cPw := newCoordinator(t, cloudPw, newStubMQTT())
	cPw.enforceMutualExclusive(context.Background(), "a", "cc", "powerfulMode", "on")
	if cloudPw.patchCount() != 0 {
		t.Errorf("powerful on should not blind-clear econo, got %d", cloudPw.patchCount())
	}

	// Switching off does not clear the partner.
	cloud2 := &stubCloud{}
	c2 := newCoordinator(t, cloud2, newStubMQTT())
	c2.enforceMutualExclusive(context.Background(), "a", "cc", "econoMode", "off")
	if cloud2.patchCount() != 0 {
		t.Errorf("turning off should not clear partner, got %d", cloud2.patchCount())
	}
}

func TestEnforceMutualExclusiveFansOutPowerfulClear(t *testing.T) {
	// econo is group-wide: turning it on clears powerful on every member and
	// resets the group's suspend state so an in-progress boost cannot undo it.
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}
	c.econoSuspend = map[string]econoSuspendState{"OD1": {boosting: true, pending: true}}

	c.enforceMutualExclusive(context.Background(), "a", "cc", "econoMode", "on")

	// powerful=off on origin (a) and fanned out to b.
	if cloud.patchCount() != 2 {
		t.Fatalf("expected powerful clear on both members, got %d patches", cloud.patchCount())
	}
	for _, p := range cloud.allPatches() {
		if p.characteristic != "powerfulMode" || p.value != "off" {
			t.Errorf("patch = %+v, want powerfulMode/off", p)
		}
	}
	if got := c.econoSuspend["OD1"]; got.boosting || got.pending {
		t.Errorf("suspend state = %+v, want reset", got)
	}
}
