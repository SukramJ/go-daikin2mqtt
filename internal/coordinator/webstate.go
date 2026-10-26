// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"errors"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
	"github.com/SukramJ/go-daikin2mqtt/internal/state"
)

// ErrWriteQueueFull is returned by [Coordinator.Submit] when the write queue
// cannot take the request.
var ErrWriteQueueFull = errors.New("write queue full")

// recordPoints describes the devices of a poll to the web UI's state store.
func (c *Coordinator) recordPoints(devices []model.Device, points []process.Point) {
	if c.deps.State == nil {
		return
	}
	models := make(map[string]string, len(devices))
	for _, d := range devices {
		models[d.ID] = d.Model
	}
	byDevice := make(map[string][]state.Point, len(devices))
	for i := range points {
		p := &points[i]
		e := &p.Entry
		sp := state.Point{
			EmbeddedID: p.EmbeddedID, MPType: p.MPType, Topic: p.Topic,
			Characteristic: e.Match.Characteristic,
			Name:           e.Name, NameDE: e.NameDE,
			Platform: e.Platform, Category: e.Category,
			Unit: p.Unit, Min: p.Min, Max: p.Max, Step: p.Step,
			Settable: e.Settable,
		}
		for _, v := range e.Values {
			sp.Options = append(sp.Options, state.Option{Value: v.Value, Label: v.Label, LabelDE: v.LabelDE})
		}
		byDevice[p.DeviceID] = append(byDevice[p.DeviceID], sp)
	}
	c.deps.State.SetPoints(models, byDevice)
}

// observe hands a published status item to the web UI's state store.
func (c *Coordinator) observe(topic string, value any) {
	if c.deps.State == nil {
		return
	}
	if dev, emb, leaf, ok := c.topicRoot.ParseStatus(topic); ok {
		c.deps.State.Observe(dev, emb, leaf, value, c.deps.Clock())
	}
}

// Submit queues a write exactly as an MQTT set on the item
// `<name>/set/<deviceID>/<embeddedID>/<topic>` would: the same
// validation, coalescing and PATCH path. It returns before the write runs.
func (c *Coordinator) Submit(deviceID, embeddedID, topic, payload string) error {
	select {
	case c.writes <- writeReq{deviceID: deviceID, embeddedID: embeddedID, topic: topic, payload: payload}:
		return nil
	default:
		return ErrWriteQueueFull
	}
}

// Upstream is the health the web UI reports: whether the last cloud poll
// succeeded, whether devices are known, and when the last poll started.
func (c *Coordinator) Upstream() (cloudOK, devicesKnown bool, lastPoll time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cloudOK, c.devicesKnown, c.lastPoll
}
