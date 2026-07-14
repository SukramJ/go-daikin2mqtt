// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"log/slog"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// refreshMinInterval is the shortest gap between a poll and a manual refresh
// that honours it. The ONECTA cloud enforces a hard daily request quota, so a
// button press (which can be automated) must not be able to spend it: a refresh
// arriving within this window of the last poll is dropped — the data it would
// fetch is at most this old anyway.
const refreshMinInterval = 30 * time.Second

// requestRefresh asks the poll loop to run a cycle now instead of waiting for
// the next scheduled one. It never blocks: the request channel holds one
// pending refresh, so presses arriving while a poll runs coalesce into a single
// extra cycle.
func (c *Coordinator) requestRefresh() {
	c.mu.Lock()
	last := c.lastPoll
	c.mu.Unlock()
	if !last.IsZero() {
		if age := c.deps.Clock().Sub(last); age < refreshMinInterval {
			c.deps.Logger.Info("coordinator.refresh_throttled",
				slog.Duration("since_last_poll", age), slog.Duration("min_interval", refreshMinInterval))
			return
		}
	}

	select {
	case c.refresh <- struct{}{}:
		c.deps.Logger.Info("coordinator.refresh_requested")
	default:
		c.deps.Logger.Debug("coordinator.refresh_already_pending")
	}
}

// refreshPoints synthesizes the manual cloud-refresh button, one point per
// device. The button is not a device characteristic — no cloud value backs it —
// so it only ever reaches Home Assistant through here. Its catalog entry is
// scope: outdoor, so the points of the indoor units sharing an outdoor unit
// deduplicate to a single button on that outdoor unit (see hass.entityIdentity);
// a device without a known outdoor serial gets its own button on the main
// device. Every button triggers the same daemon-wide poll, so which device's
// command topic wins the dedup does not matter.
func (c *Coordinator) refreshPoints(devices []model.Device) []process.Point {
	entry, ok := c.deps.Catalog.ByTopic(hass.RefreshTopic)
	if !ok {
		return nil
	}
	out := make([]process.Point, 0, len(devices))
	for _, d := range devices {
		emb, ok := c.climateEmbeddedID(d.ID)
		if !ok {
			continue // no climateControl point yet; nothing to hang the button on
		}
		out = append(out, process.Point{
			DeviceID:   d.ID,
			EmbeddedID: emb,
			MPType:     "climateControl",
			Topic:      entry.Topic,
			Entry:      *entry,
			Settable:   true,
		})
	}
	return out
}
