// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/SukramJ/go-hamqtt/discovery"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// `<name>/connected` and the per-device `online` items (mqtt-smarthome 2.0
// §3.1, openccu-loom ADR 0083).
//
// The level is 0 by the Last Will and on a graceful stop, 1 while the broker is
// up but the upstream this daemon reads and writes through is not usable, and 2
// while it is. Every entity's availability reads it at ≥ 2, beside its device's
// own `online` item.

// upstreamWatchInterval is how often local mode re-checks the Faikin link
// between polls. The cloud poll can be half an hour apart at night, and a
// Faikin broker that drops in between must not leave the level at 2 that long.
const upstreamWatchInterval = 15 * time.Second

// noteCloud records the outcome of one cloud poll and re-derives the level.
func (c *Coordinator) noteCloud(ctx context.Context, ok bool) {
	c.mu.Lock()
	c.cloudOK = ok
	if ok {
		c.devicesKnown = true
	}
	c.mu.Unlock()
	c.updateConnected(ctx)
}

// upstreamLevel is the level the daemon's upstream justifies right now.
//
//   - Cloud mode: 2 while the last ONECTA poll succeeded. A failed poll — the
//     cloud unreachable, rate-limited, a dead refresh token, missing
//     credentials — is 1. A scan-ignore skip after a write is no answer and
//     changes nothing (see pollOnce).
//   - Local mode: 2 while the connection the Faikin modules are reached over
//     is up and the device topology is known, i.e. at least one cloud poll has
//     succeeded since start. The topology is what routes a Faikin state onto a
//     device's items, so before it the local path cannot work; after it a
//     cloud outage no longer matters, because reads and writes of the mapped
//     units go to Faikin.
func (c *Coordinator) upstreamLevel() int {
	c.mu.Lock()
	cloudOK, known := c.cloudOK, c.devicesKnown
	c.mu.Unlock()
	usable := cloudOK
	if c.deps.Cfg.LocalEnabled() {
		usable = known && c.faikinUp()
	}
	if usable {
		return discovery.ConnectedOperational
	}
	return discovery.ConnectedBroker
}

// faikinUp reports whether the Faikin connection is up; unknown counts as up.
func (c *Coordinator) faikinUp() bool {
	return c.deps.FaikinConnected == nil || c.deps.FaikinConnected()
}

// updateConnected publishes `<name>/connected` when the level the upstream
// justifies differs from the one last decided. It holds connMu across the
// publish so a reconnect's runtime swap cannot interleave with it — see
// [Coordinator.PublishOnline], which re-announces the decided level on the
// fresh runtime.
func (c *Coordinator) updateConnected(ctx context.Context) {
	level := c.upstreamLevel()
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.connLevel == level {
		return
	}
	c.connLevel = level
	rt := c.ha()
	if rt == nil {
		return
	}
	if _, err := rt.SetConnected(ctx, level); err != nil {
		c.deps.Logger.Warn("coordinator.connected_publish_failed",
			slog.Int("level", level), slog.String("err", err.Error()))
	}
}

// watchUpstream re-derives the level on a timer, for the local-mode half that
// changes without a poll.
func (c *Coordinator) watchUpstream(ctx context.Context) error {
	t := time.NewTicker(upstreamWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.updateConnected(ctx)
		}
	}
}

// publishCloudOnline publishes each cloud-served device's `online` item from
// the ONECTA device document's own isCloudConnectionUp. A locally mapped
// device is skipped: in local mode its reachability is the Faikin module's
// report (see publishLocalState), and the cloud's view of it lags.
func (c *Coordinator) publishCloudOnline(ctx context.Context, devices []model.Device) {
	for i := range devices {
		d := &devices[i]
		if c.localActiveFor(d.ID) {
			continue
		}
		c.publishState(ctx, c.topicRoot.Online(d.ID), cloudConnectionUp(d.Raw))
	}
}

// cloudConnectionUp reads a device document's isCloudConnectionUp. A document
// that does not carry it — every fixture before the field existed — counts as
// up: the device was just listed by the cloud, which is evidence of the link.
func cloudConnectionUp(raw json.RawMessage) bool {
	var doc struct {
		Up *struct {
			Value *bool `json:"value"`
		} `json:"isCloudConnectionUp"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Up == nil || doc.Up.Value == nil {
		return true
	}
	return *doc.Up.Value
}
