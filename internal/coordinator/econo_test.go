// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// The econo save/restore is exercised through reconcileEconoSuspend with an
// observed (anyPowerful, econoOn) group snapshot — the same shape the local and
// cloud drivers feed it. econo writes route to the cloud stub (econoMode on/off)
// since these coordinators are not locally active.

func TestEconoSuspendOnBoostStart(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}

	// econo on, a member enters powerful → econo suspended group-wide and saved.
	c.reconcileEconoSuspend(context.Background(), "a", true, true)

	patches := cloud.allPatches()
	if len(patches) != 2 {
		t.Fatalf("expected econo off on both members, got %d", len(patches))
	}
	for _, p := range patches {
		if p.characteristic != "econoMode" || p.value != "off" {
			t.Errorf("patch = %+v, want econoMode/off", p)
		}
	}
	if got := c.econoSuspend["OD1"]; !got.boosting || !got.pending {
		t.Errorf("suspend state = %+v, want boosting+pending", got)
	}
}

func TestEconoRestoreOnBoostEnd(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}
	c.econoSuspend = map[string]econoSuspendState{"OD1": {boosting: true, pending: true}}

	// Boost ends → econo restored group-wide (the hardware does not do this).
	c.reconcileEconoSuspend(context.Background(), "a", false, false)

	patches := cloud.allPatches()
	if len(patches) != 2 {
		t.Fatalf("expected econo on both members, got %d", len(patches))
	}
	for _, p := range patches {
		if p.characteristic != "econoMode" || p.value != "on" {
			t.Errorf("patch = %+v, want econoMode/on", p)
		}
	}
	if got := c.econoSuspend["OD1"]; got.boosting || got.pending {
		t.Errorf("suspend state = %+v, want cleared", got)
	}
}

func TestEconoNoSuspendWhenAlreadyOff(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc"}

	// Boost starts but econo was already off → nothing to suspend or restore.
	c.reconcileEconoSuspend(context.Background(), "a", true, false)
	if cloud.patchCount() != 0 {
		t.Fatalf("no econo write expected, got %d", cloud.patchCount())
	}
	c.reconcileEconoSuspend(context.Background(), "a", false, false)
	if cloud.patchCount() != 0 {
		t.Errorf("no restore expected, got %d", cloud.patchCount())
	}
	if c.econoSuspend["OD1"].pending {
		t.Errorf("pending should be false, got %+v", c.econoSuspend["OD1"])
	}
}

func TestEconoMultiMemberBoost(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1", "b": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc", "b": "cc"}

	// a boosts → exactly one suspend (econo off on a and b).
	c.reconcileEconoSuspend(context.Background(), "a", true, true)
	if cloud.patchCount() != 2 {
		t.Fatalf("after a boosts: %d patches, want 2 (suspend)", cloud.patchCount())
	}
	// b also boosts (group still boosting, econo already off) → no second suspend.
	c.reconcileEconoSuspend(context.Background(), "b", true, false)
	if cloud.patchCount() != 2 {
		t.Errorf("b also boosting: %d patches, want still 2", cloud.patchCount())
	}
	// a stops while b keeps boosting (group OR still true) → no restore yet.
	c.reconcileEconoSuspend(context.Background(), "a", true, false)
	if cloud.patchCount() != 2 {
		t.Errorf("a stops while b boosts: %d patches, want still 2", cloud.patchCount())
	}
	// b stops (last member) → exactly one restore (econo on on a and b).
	c.reconcileEconoSuspend(context.Background(), "b", false, false)
	if cloud.patchCount() != 4 {
		t.Errorf("last boost ends: %d patches, want 4 (restore)", cloud.patchCount())
	}
}

func TestEconoSuspendDisabledByFlag(t *testing.T) {
	off := false
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.deps.Cfg.EnforceMutualExclusive = &off
	c.outdoorSerial = map[string]string{"a": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc"}

	c.reconcileEconoSuspend(context.Background(), "a", true, true)
	if cloud.patchCount() != 0 {
		t.Errorf("disabled flag should not write, got %d", cloud.patchCount())
	}
	if _, ok := c.econoSuspend["OD1"]; ok {
		t.Errorf("disabled flag should not touch suspend state")
	}
}

func TestEconoSingletonGroup(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.climateEmbedded = map[string]string{"solo": "cc"}
	// No outdoorSerial → groupKey == deviceID, fan-out is a no-op.

	c.reconcileEconoSuspend(context.Background(), "solo", true, true) // suspend
	if cloud.patchCount() != 1 {
		t.Fatalf("singleton suspend: %d patches, want 1", cloud.patchCount())
	}
	c.reconcileEconoSuspend(context.Background(), "solo", false, false) // restore
	if cloud.patchCount() != 2 {
		t.Fatalf("singleton restore: %d patches, want 2", cloud.patchCount())
	}
	if p := cloud.lastPatch(t); p.characteristic != "econoMode" || p.value != "on" {
		t.Errorf("restore = %+v, want econoMode/on", p)
	}
}

func TestEconoRestartDuringBoost(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc"}

	// First reconcile after a restart sees a boost already running with econo off
	// (the hardware suspended it) → nothing is saved, so no spurious restore later.
	c.reconcileEconoSuspend(context.Background(), "a", true, false)
	if cloud.patchCount() != 0 {
		t.Fatalf("restart mid-boost should not write, got %d", cloud.patchCount())
	}
	if c.econoSuspend["OD1"].pending {
		t.Errorf("pending should be false after restart mid-boost")
	}
	c.reconcileEconoSuspend(context.Background(), "a", false, false) // boost ends
	if cloud.patchCount() != 0 {
		t.Errorf("no restore expected after lost state, got %d", cloud.patchCount())
	}
}

func TestEconoNoFeedbackLoop(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.outdoorSerial = map[string]string{"a": "OD1"}
	c.climateEmbedded = map[string]string{"a": "cc"}

	c.reconcileEconoSuspend(context.Background(), "a", true, true)   // suspend
	c.reconcileEconoSuspend(context.Background(), "a", false, false) // restore
	n := cloud.patchCount()
	// The restore's own econo=on, observed later while not boosting, is not an
	// edge → no write, so the machine never loops.
	c.reconcileEconoSuspend(context.Background(), "a", false, true)
	if extra := cloud.patchCount() - n; extra != 0 {
		t.Errorf("read-back of restored econo must not write, got %d extra", extra)
	}
}

func TestEconoCloudSkipsLocalActive(t *testing.T) {
	cloud := &stubCloud{}
	cfg := &config.Config{
		MQTTTopic: "daikin", LocalMode: true,
		LocalDeviceMap: map[string]string{"a": "Klima A"},
	}
	c := New(Deps{
		Cfg: cfg, Client: cloud, MQTT: newStubMQTT(), FaikinMQTT: newStubMQTT(),
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.climateEmbedded = map[string]string{"a": "cc"}

	// "a" is locally controlled, so the cloud snapshot (stale) must not drive it.
	devs := []model.Device{{
		ID: "a",
		ManagementPoints: []model.ManagementPoint{{
			Type: "climateControl",
			Characteristics: map[string]model.Characteristic{
				"powerfulMode": {Value: json.RawMessage(`"on"`)},
				"econoMode":    {Value: json.RawMessage(`"on"`)},
			},
		}},
	}}
	c.reconcileEconoSuspendCloud(context.Background(), devs)
	if cloud.patchCount() != 0 {
		t.Errorf("locally-active group must be skipped by cloud reconcile, got %d", cloud.patchCount())
	}
}

func TestEconoConcurrentEdge(t *testing.T) {
	cloud := &stubCloud{}
	c := newCoordinator(t, cloud, newStubMQTT())
	c.climateEmbedded = map[string]string{"solo": "cc"}

	// Two goroutines observe the same boost-start edge; the c.mu-guarded
	// read-modify-write must yield exactly one suspend (run with -race).
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.reconcileEconoSuspend(context.Background(), "solo", true, true)
		}()
	}
	wg.Wait()
	if cloud.patchCount() != 1 {
		t.Errorf("concurrent boost-start edge: %d econo writes, want exactly 1", cloud.patchCount())
	}
}
