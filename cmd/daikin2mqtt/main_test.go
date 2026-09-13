// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/coordinator"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
)

// failingPublisher always reports a broker-side failure so the breaker
// counts every publish against its threshold.
type failingPublisher struct{ calls int }

func (p *failingPublisher) Publish(context.Context, string, []byte, mqtt.QoS, bool, ...mqtt.PublishOption) error {
	p.calls++
	return mqtt.ErrNotConnected
}

// recordingSubscriber captures Subscribe/Unsubscribe filters so the
// test can prove the session delegates them to the raw client.
type recordingSubscriber struct {
	subscribed   []string
	unsubscribed []string
}

func (s *recordingSubscriber) Subscribe(_ context.Context, filter string, _ mqtt.QoS, _ mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	s.subscribed = append(s.subscribed, filter)
	return mqtt.SubscribeResult{}, nil
}

func (s *recordingSubscriber) Unsubscribe(_ context.Context, filter string) error {
	s.unsubscribed = append(s.unsubscribed, filter)
	return nil
}

// TestMQTTSessionPublishIsCircuitGated proves the coordinator-facing
// session routes Publish through the breaker: once the failure
// threshold is reached, publishes fail fast with ErrCircuitOpen and no
// longer hit the underlying client.
func TestMQTTSessionPublishIsCircuitGated(t *testing.T) {
	t.Parallel()

	pub := &failingPublisher{}
	session := mqtt.SplitClient(mqtt.NewBreaker(pub, mqtt.BreakerConfig{
		FailureThreshold: 1,
	}), &recordingSubscriber{})

	err := session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	if !errors.Is(err, mqtt.ErrNotConnected) {
		t.Fatalf("first publish: got %v, want ErrNotConnected", err)
	}
	err = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	if !errors.Is(err, mqtt.ErrCircuitOpen) {
		t.Fatalf("second publish: got %v, want ErrCircuitOpen", err)
	}
	if pub.calls != 1 {
		t.Fatalf("underlying publisher saw %d calls, want 1 (open circuit must fail fast)", pub.calls)
	}
}

// TestMQTTSessionSubscribeBypassesBreaker proves subscriptions are not
// affected by the publish-side circuit state.
func TestMQTTSessionSubscribeBypassesBreaker(t *testing.T) {
	t.Parallel()

	sub := &recordingSubscriber{}
	breaker := mqtt.NewBreaker(&failingPublisher{}, mqtt.BreakerConfig{FailureThreshold: 1})
	session := mqtt.SplitClient(breaker, sub)

	// Trip the circuit open on the publish side.
	_ = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)
	_ = session.Publish(t.Context(), "t", nil, mqtt.QoS0, false)

	if _, err := session.Subscribe(t.Context(), "cmd/#", mqtt.QoS1, func(*mqtt.Message) {}); err != nil {
		t.Fatalf("subscribe with open circuit: %v", err)
	}
	if err := session.Unsubscribe(t.Context(), "cmd/#"); err != nil {
		t.Fatalf("unsubscribe with open circuit: %v", err)
	}
	if len(sub.subscribed) != 1 || sub.subscribed[0] != "cmd/#" {
		t.Fatalf("subscriber saw %v, want [cmd/#]", sub.subscribed)
	}
	if len(sub.unsubscribed) != 1 || sub.unsubscribed[0] != "cmd/#" {
		t.Fatalf("unsubscriber saw %v, want [cmd/#]", sub.unsubscribed)
	}
}

// TestClientIDsComeFromTheConfig pins the wiring half of F1: the two MQTT
// connections this daemon opens must take their client identifier from the
// resolved config, so an operator running a second instance can separate them.
// Reading a constant here is exactly the defect, and it is invisible on the
// wire until the second instance connects.
func TestClientIDsComeFromTheConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{MQTTClientID: "daikin2mqtt-staging"}
	if got := mainClientID(cfg); got != "daikin2mqtt-staging" {
		t.Errorf("mainClientID = %q, want the configured id", got)
	}
	if got := faikinClientID(cfg); got != "daikin2mqtt-staging-faikin" {
		t.Errorf("faikinClientID = %q, want the configured id plus the suffix", got)
	}

	// Two differently-configured instances must collide on neither session.
	other := &config.Config{MQTTClientID: config.DefaultMQTTClientID}
	if mainClientID(cfg) == mainClientID(other) || faikinClientID(cfg) == faikinClientID(other) {
		t.Error("two instances still share a client id — F1 is not fixed")
	}

	// And the unconfigured instance must still present the pre-fix strings.
	if got := mainClientID(other); got != "daikin2mqtt" {
		t.Errorf("default mainClientID = %q, want %q", got, "daikin2mqtt")
	}
	if got := faikinClientID(other); got != "daikin2mqtt-faikin" {
		t.Errorf("default faikinClientID = %q, want %q", got, "daikin2mqtt-faikin")
	}
}

// TestTheWillWritesTheTopicEveryEntityReads is the cross-package half of F3.
//
// The bridge's availability topic is composed by three different packages: the
// Last Will here, Coordinator.PublishOnline's "online", and the
// availability_topic of all 264 discovery payloads. Nothing compared them. If
// the will names a topic the payloads do not, entities never grey out when the
// daemon dies; if the payloads name a topic nobody writes, they never come up
// at all. Both failures are silent — there is no log line and no registry
// entry that reports either one.
func TestTheWillWritesTheTopicEveryEntityReads(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{MQTTTopic: "daikin", HASSBaseTopic: "homeassistant", Language: "en"}
	rt := publisher.New(&deferredTransport{}, coordinator.RuntimeConfig(cfg, slog.New(slog.DiscardHandler)))
	defer rt.Close()
	will, err := rt.Will()
	if err != nil {
		t.Fatalf("Will: %v", err)
	}

	// The literal an installed base already has retained on its broker.
	if will.Topic != "daikin/bridge/status" {
		t.Errorf("will topic = %q, want %q", will.Topic, "daikin/bridge/status")
	}
	// What every discovery payload names as its availability_topic. Since step
	// 5 these cannot drift: publisher.Config takes the Layout rather than a
	// status-topic literal, derives the topic from Layout.Bridge() and PANICS
	// on a StatusTopic that disagrees with it — so this assertion is now a
	// statement about the layout rather than about two hand-kept copies.
	if adv := hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, nil).BridgeStatusTopic(); adv != will.Topic {
		t.Errorf("discovery advertises availability_topic %q, the will writes %q", adv, will.Topic)
	}
	// And a non-default root must move both together.
	other := &config.Config{MQTTTopic: "haus/klima", HASSBaseTopic: "homeassistant", Language: "en"}
	otherRT := publisher.New(&deferredTransport{}, coordinator.RuntimeConfig(other, slog.New(slog.DiscardHandler)))
	defer otherRT.Close()
	otherWill, err := otherRT.Will()
	if err != nil {
		t.Fatalf("Will: %v", err)
	}
	if adv := hass.New(other.HASSBaseTopic, other.MQTTTopic, other.Language, nil).BridgeStatusTopic(); adv != otherWill.Topic {
		t.Errorf("with a custom root, discovery advertises %q but the will writes %q", adv, otherWill.Topic)
	}

	// What the CONNECT actually carries, built by the same function run() uses.
	connect := bridgeWill(will)
	if connect.Topic != will.Topic || !bytes.Equal(connect.Payload, will.Payload) ||
		connect.QoS != mqtt.QoS(will.QoS) || connect.Retain != will.Retain {
		t.Errorf("the CONNECT will %+v does not carry the runtime's will %+v", connect, will)
	}

	// The three wire values of the will itself, unchanged by the migration:
	// "offline", retained, QoS 0. Retained because a marker that is not
	// retained tells nothing to a Home Assistant that subscribes after the
	// crash — which is exactly when it needs to be told. QoS 0 because that is
	// what this bridge has always connected with, and publisher.QoS's zero
	// value would have made it 1 (F9).
	if string(will.Payload) != "offline" {
		t.Errorf("will payload = %q, want %q", will.Payload, "offline")
	}
	if !will.Retain {
		t.Error("the will must be retained")
	}
	if will.QoS != 0 {
		t.Errorf("will QoS = %d, want 0", will.QoS)
	}
}

// TestDeferredTransportRefusesUseBeforeWiring pins the one type the composition
// root owns.
//
// The runtime is built before the MQTT client exists, because the client needs
// the will the runtime produces. Whatever fills that gap must refuse rather
// than panic: the callers are publish paths, and losing one availability marker
// to a start-up race is survivable while a panicking daemon is not.
func TestDeferredTransportRefusesUseBeforeWiring(t *testing.T) {
	t.Parallel()
	d := &deferredTransport{}
	if err := d.Publish(context.Background(), "t", []byte("p"), 0, true); !errors.Is(err, errTransportNotWired) {
		t.Errorf("Publish before wiring = %v, want errTransportNotWired", err)
	}
	if err := d.Subscribe(context.Background(), "f", 0, func(string, []byte, bool) {}); !errors.Is(err, errTransportNotWired) {
		t.Errorf("Subscribe before wiring = %v, want errTransportNotWired", err)
	}
	if err := d.Unsubscribe(context.Background(), "f"); !errors.Is(err, errTransportNotWired) {
		t.Errorf("Unsubscribe before wiring = %v, want errTransportNotWired", err)
	}
	rec := &recordTransport{}
	d.wire(rec)
	if err := d.Publish(context.Background(), "t", []byte("p"), 0, true); err != nil {
		t.Errorf("Publish after wiring = %v", err)
	}
	if rec.topic != "t" {
		t.Errorf("wired transport saw %q, want %q", rec.topic, "t")
	}
}

// recordTransport is the minimal publisher.Transport a wiring test needs.
type recordTransport struct{ topic string }

func (r *recordTransport) Publish(_ context.Context, topic string, _ []byte, _ byte, _ bool) error {
	r.topic = topic
	return nil
}

func (r *recordTransport) Subscribe(context.Context, string, byte, publisher.Handler) error {
	return nil
}
func (r *recordTransport) Unsubscribe(context.Context, string) error { return nil }
