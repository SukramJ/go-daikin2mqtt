// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"slices"
	"testing"
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

	c.syncModeToGroup(context.Background(), "a", "cooling")

	if cloud.patchCount() != 1 {
		t.Fatalf("expected 1 propagated patch (to b only), got %d", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.deviceID != "b" || p.characteristic != "operationMode" || p.value != "cooling" {
		t.Errorf("propagated patch = %+v, want b/operationMode/cooling", p)
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
