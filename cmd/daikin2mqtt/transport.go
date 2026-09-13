// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"errors"
	"sync"

	"github.com/SukramJ/go-hamqtt/publisher"
)

// errTransportNotWired is returned by every method of a deferredTransport that
// has not been given a client yet.
var errTransportNotWired = errors.New("daikin2mqtt: mqtt transport not wired yet")

// deferredTransport is a publisher.Transport whose client is supplied after
// construction.
//
// It exists because of one ordering that cannot be escaped: the Last Will is
// part of CONNECT, so the MQTT client needs it before the client exists — and
// the will is publisher.Runtime.Will(), which needs a runtime, which needs a
// transport. Handing the runtime a transport that is wired a few lines later is
// what lets the will and the retained "online" come from ONE object instead of
// from two literals that have to be kept equal by hand.
//
// It refuses rather than panics before wiring. The callers are publish paths,
// and a daemon that loses one availability marker during a start-up race is a
// better outcome than a daemon that dies; the error is returned, logged by the
// caller, and the next (re)connect announces again.
type deferredTransport struct {
	mu    sync.RWMutex
	inner publisher.Transport
}

var _ publisher.Transport = (*deferredTransport)(nil)

// wire supplies the real transport. Calling it twice is a programming error
// this does not try to detect: the composition root calls it exactly once.
func (d *deferredTransport) wire(tr publisher.Transport) {
	d.mu.Lock()
	d.inner = tr
	d.mu.Unlock()
}

func (d *deferredTransport) get() (publisher.Transport, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.inner == nil {
		return nil, errTransportNotWired
	}
	return d.inner, nil
}

func (d *deferredTransport) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	tr, err := d.get()
	if err != nil {
		return err
	}
	return tr.Publish(ctx, topic, payload, qos, retain)
}

func (d *deferredTransport) Subscribe(ctx context.Context, filter string, qos byte, h publisher.Handler) error {
	tr, err := d.get()
	if err != nil {
		return err
	}
	return tr.Subscribe(ctx, filter, qos, h)
}

func (d *deferredTransport) Unsubscribe(ctx context.Context, filter string) error {
	tr, err := d.get()
	if err != nil {
		return err
	}
	return tr.Unsubscribe(ctx, filter)
}
