// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// movableClock is an injectable clock a test can advance, for the refresh
// throttle (which compares "now" against the last poll).
type movableClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMovableClock() *movableClock {
	return &movableClock{t: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *movableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newRefreshCoordinator builds a coordinator on a movable clock and a poll
// interval long enough that only a manual refresh can trigger a second poll.
func newRefreshCoordinator(t *testing.T, cloud *stubCloud, m *stubMQTT, clk *movableClock) *Coordinator {
	t.Helper()
	cfg := testConfig()
	cfg.RefreshDayInterval = 3600
	cfg.RefreshNightInterval = 3600
	return New(Deps{
		Cfg:     cfg,
		Client:  cloud,
		MQTT:    m,
		Catalog: loadTestCatalog(t),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   clk.now,
	})
}

// A press on the refresh button writes nothing to the device — it queues a poll
// request for the poll loop.
func TestRefreshWriteQueuesPollAndDoesNotPatch(t *testing.T) {
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.handleWrite(context.Background(), writeReq{
		deviceID:   "dev1",
		embeddedID: "climateControl",
		topic:      "refresh",
		payload:    "PRESS",
	})

	if n := cloud.patchCount(); n != 0 {
		t.Errorf("patch count = %d, want 0: the refresh button must not write to the device", n)
	}
	select {
	case <-c.refresh:
	default:
		t.Fatal("refresh button press did not queue a poll request")
	}
}

// The poll loop wakes on a refresh request instead of waiting out the (here:
// hour-long) interval.
func TestRefreshTriggersPoll(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	clk := newMovableClock()
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newRefreshCoordinator(t, cloud, m, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.pollLoop(ctx)
	}()

	waitForGets(t, cloud, 1) // the loop's initial poll
	clk.advance(refreshMinInterval)
	c.requestRefresh()
	waitForGets(t, cloud, 2) // the manual refresh

	cancel()
	<-done
}

// A refresh arriving within refreshMinInterval of the last poll is dropped: the
// ONECTA daily request quota must not be spendable by button presses.
func TestRefreshThrottledAfterRecentPoll(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	clk := newMovableClock()
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newRefreshCoordinator(t, cloud, m, clk)

	c.pollOnce(context.Background()) // records lastPoll

	clk.advance(refreshMinInterval - time.Second)
	c.requestRefresh()
	select {
	case <-c.refresh:
		t.Fatal("refresh within the throttle window was not dropped")
	default:
	}

	clk.advance(2 * time.Second) // now past refreshMinInterval
	c.requestRefresh()
	select {
	case <-c.refresh:
	default:
		t.Fatal("refresh after the throttle window was dropped")
	}
}

// Presses arriving while a poll is in flight coalesce into a single extra cycle
// (the request channel holds one) rather than queueing up cloud requests.
func TestRefreshRequestsCoalesce(t *testing.T) {
	clk := newMovableClock()
	cloud := &stubCloud{}
	m := newStubMQTT()
	c := newRefreshCoordinator(t, cloud, m, clk)

	c.requestRefresh()
	c.requestRefresh()

	select {
	case <-c.refresh:
	default:
		t.Fatal("no refresh request queued")
	}
	select {
	case <-c.refresh:
		t.Fatal("second press queued a second poll instead of coalescing")
	default:
	}
}

// The button is a discovery-only point: it must appear in the point set (so HA
// gets a config) but publish no state topic, since HA's MQTT button has none.
func TestRefreshPointPublishesNoState(t *testing.T) {
	const dev, emb = "dev1", "climateControl"
	cloud := &stubCloud{devices: devicesJSON(dev, emb)}
	m := newStubMQTT()
	c := newCoordinator(t, cloud, m)

	c.pollOnce(context.Background())

	devices, err := model.ParseDevices(cloud.devices)
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	points := c.refreshPoints(devices)
	if len(points) != 1 {
		t.Fatalf("refreshPoints = %d points, want 1 (one per device)", len(points))
	}
	if got := points[0].Entry.Platform; got != "button" {
		t.Errorf("platform = %q, want button", got)
	}
	if _, ok := m.get("daikin/" + dev + "/" + emb + "/refresh/state"); ok {
		t.Error("refresh button published a state topic; HA's MQTT button has no state")
	}
}

// waitForGets blocks until the stub cloud has served n GetDevices calls.
func waitForGets(t *testing.T, cloud *stubCloud, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cloud.getCount() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("GetDevices called %d times, want %d", cloud.getCount(), n)
}
