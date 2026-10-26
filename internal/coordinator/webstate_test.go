// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/state"
)

// A poll describes the devices to the web UI's store, and every published
// status item reaches it, so the dashboard shows what MQTT carries.
func TestPollFeedsTheWebState(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	store := state.New(time.Now())
	c := New(Deps{
		Cfg:     testConfig(),
		Client:  &stubCloud{devices: devicesJSON(dev, emb)},
		MQTT:    newStubMQTT(),
		Catalog: loadTestCatalog(t),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   fixedClock(),
		State:   store,
	})
	c.pollOnce(context.Background())

	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].ID != dev {
		t.Fatalf("snapshot = %+v", snap)
	}
	var power *state.PointValue
	for i := range snap[0].Points {
		if snap[0].Points[i].Topic == "power" {
			power = &snap[0].Points[i]
		}
	}
	if power == nil || power.Platform != "switch" || !power.Settable || power.Characteristic != "onOffMode" {
		t.Fatalf("power point = %+v", power)
	}
	if _, ok := power.Value.(bool); !ok {
		t.Errorf("power value = %#v, want the published bool", power.Value)
	}
	if ok, known, last := c.Upstream(); !ok || !known || last.IsZero() {
		t.Errorf("Upstream = %v %v %v", ok, known, last)
	}
}

// Submit queues like an MQTT set, and says so when the queue is full.
func TestSubmit(t *testing.T) {
	c := New(Deps{Cfg: testConfig(), Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler)})
	if err := c.Submit("dev1", "climateControl", "power", "true"); err != nil {
		t.Fatal(err)
	}
	if req := <-c.writes; req.deviceID != "dev1" || req.embeddedID != "climateControl" || req.topic != "power" || req.payload != "true" {
		t.Errorf("queued %+v", req)
	}
	var err error
	for range cap(c.writes) + 1 {
		if err = c.Submit("dev1", "climateControl", "power", "true"); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrWriteQueueFull) {
		t.Errorf("full queue: %v", err)
	}
}
