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
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())

	// powerful on → econo cleared.
	c.enforceMutualExclusive(context.Background(), "a", "cc", "powerfulMode", "on")
	if cloud.patchCount() != 1 {
		t.Fatalf("expected econo clear, got %d patches", cloud.patchCount())
	}
	p := cloud.lastPatch(t)
	if p.characteristic != "econoMode" || p.value != "off" {
		t.Errorf("clear = %+v, want econoMode/off", p)
	}

	// Switching off does not clear the partner.
	cloud2 := &stubCloud{}
	c2 := newCoordinator(t, cloud2, newStubMQTT())
	c2.enforceMutualExclusive(context.Background(), "a", "cc", "powerfulMode", "off")
	if cloud2.patchCount() != 0 {
		t.Errorf("turning off should not clear partner, got %d", cloud2.patchCount())
	}
}
