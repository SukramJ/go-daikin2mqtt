// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package state is the web UI's copy of what the bridge publishes: every
// device's points as the last cloud poll resolved them, and the value last
// published on each point's status item — from the cloud, the Faikin read
// path or the scheduler alike. It reads nothing itself and costs no cloud
// request; the coordinator feeds it. It is only allocated when WEB_ENABLE
// is on.
package state

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Point describes one published item of a device: where it sits, what it
// is called and, when it is settable, how it is written.
type Point struct {
	EmbeddedID string
	MPType     string
	// Topic is the item's leaf: a catalogue topic or a synthetic one.
	Topic string
	// Characteristic is the ONECTA characteristic behind the point.
	Characteristic string
	Name, NameDE   string
	Platform       string
	Category       string
	Unit           string
	Min, Max, Step *float64
	Options        []Option
	Settable       bool
}

// Option is one enum value with its labels.
type Option struct {
	Value, Label, LabelDE string
}

// Device is one device as the UI shows it.
type Device struct {
	ID     string
	Model  string
	Online *bool
	// UpdatedAt is the last value published for the device.
	UpdatedAt time.Time
	Points    []PointValue
}

// PointValue is a point with its current value; Value is nil until one was
// published.
type PointValue struct {
	Point
	Value any
}

type device struct {
	model   string
	online  *bool
	updated time.Time
	points  map[string]Point // "<embeddedID>/<topic>"
	values  map[string]any   // "<embeddedID>/<topic>", also for points not (yet) described
}

// Store is safe for concurrent use.
type Store struct {
	mu        sync.RWMutex
	devices   map[string]*device
	startedAt time.Time
	onChange  func()
}

// New returns an empty store.
func New(now time.Time) *Store {
	return &Store{devices: map[string]*device{}, startedAt: now}
}

// StartedAt is when the store, and with it the daemon, started.
func (s *Store) StartedAt() time.Time { return s.startedAt }

// OnChange registers f to be called after every change; f must not block.
func (s *Store) OnChange(f func()) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

func (s *Store) changed() {
	s.mu.RLock()
	f := s.onChange
	s.mu.RUnlock()
	if f != nil {
		f()
	}
}

func (s *Store) deviceLocked(id string) *device {
	d := s.devices[id]
	if d == nil {
		d = &device{points: map[string]Point{}, values: map[string]any{}}
		s.devices[id] = d
	}
	return d
}

// SetPoints replaces the points of the devices the last poll resolved, keyed
// by device ID, with each device's model. Values already published stay;
// devices the poll no longer returns are dropped.
func (s *Store) SetPoints(models map[string]string, points map[string][]Point) {
	s.mu.Lock()
	for id := range s.devices {
		if _, ok := models[id]; !ok {
			delete(s.devices, id)
		}
	}
	for id, m := range models {
		d := s.deviceLocked(id)
		d.model = m
		d.points = make(map[string]Point, len(points[id]))
		for i := range points[id] {
			p := &points[id][i]
			d.points[p.EmbeddedID+"/"+p.Topic] = *p
		}
	}
	s.mu.Unlock()
	s.changed()
}

// Observe records a value published on a device's item. An embeddedID of ""
// with the leaf "online" is the device's reachability. A nil value clears
// the item.
func (s *Store) Observe(deviceID, embeddedID, leaf string, value any, at time.Time) {
	s.mu.Lock()
	d := s.deviceLocked(deviceID)
	if embeddedID == "" {
		if b, ok := value.(bool); ok {
			d.online = &b
		} else {
			d.online = nil
		}
	} else {
		key := embeddedID + "/" + leaf
		if value == nil {
			delete(d.values, key)
		} else {
			d.values[key] = value
		}
		d.updated = at
	}
	s.mu.Unlock()
	s.changed()
}

// Snapshot returns every device, ordered by ID, with its described points
// ordered by management point and topic.
func (s *Store) Snapshot() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0, len(s.devices))
	for _, id := range slices.Sorted(maps.Keys(s.devices)) {
		d := s.devices[id]
		if len(d.points) == 0 {
			continue // published to before the first poll described it
		}
		dev := Device{ID: id, Model: d.model, UpdatedAt: d.updated, Points: make([]PointValue, 0, len(d.points))}
		if d.online != nil {
			on := *d.online
			dev.Online = &on
		}
		for _, k := range slices.SortedFunc(maps.Keys(d.points), strings.Compare) {
			dev.Points = append(dev.Points, PointValue{Point: d.points[k], Value: d.values[k]})
		}
		out = append(out, dev)
	}
	return out
}

// Point returns one described point of a device.
func (s *Store) Point(deviceID, embeddedID, topic string) (Point, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[deviceID]
	if d == nil {
		return Point{}, false
	}
	p, ok := d.points[embeddedID+"/"+topic]
	return p, ok
}

// HasDevice reports whether the last poll returned the device.
func (s *Store) HasDevice(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[id]
	return d != nil && len(d.points) > 0
}
