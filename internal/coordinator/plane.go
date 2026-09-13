// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"log/slog"
	"strings"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
)

// ADR 0070 phase 8 step 5: the go-hamqtt publisher runtime this daemon's state,
// availability and command planes go out through.
//
// Everything the composition root has to get right about the library is stated
// here, once, so that main and the tests configure the same daemon. The three
// QoS constants below are the reason this file exists at all.

// StateQoS, CommandQoS and DiscoveryQoS are the delivery guarantees this bridge
// has always had, stated in the library's vocabulary.
//
// All three are publisher.QoSAtMostOnce — MQTT QoS 0 — because that is what all
// fifteen of this daemon's transport calls pass today, measured over 1 083
// recorded publishes by TestPublishQoSAndRetain and unchanged by this step.
//
// They are constants rather than inline literals because of the trap F9 of the
// phase 8 measurement records: publisher.QoS's zero value is QoSUnset and every
// runtime field in that package resolves it to **QoS 1**. An omitted QoS field
// is therefore not "keep what we had", it is a silent upgrade of the whole
// surface — 221 retained topics per poll on a ten-minute cycle — inside a step
// whose entire claim is that nothing on the wire moved. QoSAtMostOnce is 0x80,
// deliberately outside the wire's 0-2 range, precisely so that "unset" and
// "deliberately at most once" cannot be written the same way.
//
// This is an inherited choice being preserved, not an endorsement: a retained
// config lost at QoS 0 is a config that may never be published again, which is
// why the library defaults the other way. Changing it is an operator-visible
// decision about an installed base and belongs in its own step.
const (
	// StateQoS is publisher.StateConfig.QoS — the entity state and attributes plane.
	StateQoS = publisher.QoSAtMostOnce
	// CommandQoS is publisher.CommandConfig.QoS — the `<root>/+/+/+/set` subscription.
	CommandQoS = publisher.QoSAtMostOnce
	// DiscoveryQoS is publisher.Config.QoS — the bridge availability marker, the
	// Last Will it is paired with, and the sweep's snapshot window.
	DiscoveryQoS = publisher.QoSAtMostOnce
)

// RuntimeFactory builds a fresh publisher.Runtime over an already-wired
// transport.
//
// A factory rather than an instance, and that is the whole point (go-mtec2mqtt
// PR #54, F1). A publisher.Runtime remembers three things — which per-entity
// config topics it has superseded, what it has declared, and what it has
// announced — and all three are statements about A BROKER, while the memo is
// per-process. At QoS 0 a "successful" publish is a statement about one
// connection. After a reconnect the old memo says the retractions have already
// been applied, so a republish skips them and publishes the document anyway:
// Home Assistant then sees a bundle while the per-entity configs are still
// retained and refuses it with one WARNING line and no entities.
//
// Rebuilding the whole runtime on every (re)connect fixes the class rather than
// the three fields, so a field the library adds later is covered the day it is
// added. Step 5 publishes no bundle and supersedes nothing — this is the
// plumbing built so that step 6 cannot make that mistake.
type RuntimeFactory func() *publisher.Runtime

// RuntimeConfig is the one publisher.Config literal this daemon has.
//
// One function because a second copy is how a composition root and its tests
// end up configuring different daemons — go-mtec2mqtt found exactly that with a
// mutation: dropping LegacyEntityTopics from its main package was caught by
// nothing, because every fixture carried its own copy.
func RuntimeConfig(cfg *config.Config, logger *slog.Logger) publisher.Config {
	return publisher.Config{
		Prefix: cfg.HASSBaseTopic,
		// Layout, not StatusTopic: with a Layout set the runtime derives the
		// status topic from Layout.Bridge() and PANICS on a StatusTopic that
		// disagrees with it. The availability topic named by all 264 discovery
		// payloads, the retained "online" and the Last Will therefore cannot
		// drift apart — they are one function, internal/layout's BridgeStatus.
		Layout:             hass.Layout(cfg.MQTTTopic),
		QoS:                DiscoveryQoS,
		LegacyEntityTopics: hass.LegacyConfigTopicForms(),
		Logger:             logger,
	}
}

// NewStatePlane builds the state publisher: retained entity state and the
// attributes siblings, at QoS 0, guarded against this daemon's own command
// subscription.
//
// Encoding is stated even though this daemon renders its own payload bytes
// (formatValue's output is the installed base's, and the library's raw renderer
// would print a Go float differently), because the zero Encoding is
// EnvelopeEncoding — the shape a `value_template` would have to read.
func NewStatePlane(tr publisher.Transport, root layout.Root, logger *slog.Logger) *publisher.StatePublisher {
	return publisher.NewStatePublisher(tr, publisher.StateConfig{
		QoS:      StateQoS,
		Encoding: discovery.RawEncoding,
		// The library's own echo guard: a state publish that would land inside
		// this process's own command subscription is refused with
		// ErrStateCommandCollision instead of being delivered back to the
		// daemon as a command it issued to itself. One formula, three readers
		// (the subscription, the router route and this guard).
		CommandFilters: []string{root.CommandFilter()},
		Logger:         logger,
	})
}

// NewCommandRouter builds the router for the one command filter this bridge
// subscribes: "<root>/+/+/+/set".
//
// Workers is 1 because the handler's only job is to parse the topic and hand
// the request to the daemon's own `writes` channel, which the single drain
// goroutine serialises behind the cloud's single-in-flight lock anyway; a pool
// would buy concurrency the cloud API cannot use.
//
// DeliverRetained is false, which is the behaviour the hand-written handler had
// with an explicit check: a retained /set is a stale command the broker replays
// on every (re)subscribe, and applying it re-writes hardware state on each
// reconnect.
func NewCommandRouter(tr publisher.Transport, logger *slog.Logger) *publisher.CommandRouter {
	return publisher.NewCommandRouter(tr, publisher.CommandConfig{
		QoS:             CommandQoS,
		Workers:         1,
		DeliverRetained: false,
		Logger:          logger,
	})
}

// OwnsConfigTopic is the sweep's ownership predicate: the TOPIC half of
// "is this retained discovery config mine".
//
// It scopes on what publisher.ConfigTopic offers, which is all the broker
// offers before the payload is read:
//
//   - the four-segment per-entity form only — not a bundle, and not the
//     five-segment node-id form this bridge has never published (F5, measured
//     264 of 264 at step 4);
//   - a platform this daemon actually emits, of Home Assistant's 32;
//   - an object id — which in this form IS the unique_id — inside the compile-
//     time `daikin_` namespace.
//
// It is deliberately NOT the whole answer. Every string it can see is
// byte-identical between two go-daikin2mqtt instances that see one ONECTA
// device (F14), so a predicate over the topic alone cannot tell this instance's
// retained configs from a sibling's. hass.Discovery.IsOwnConfig is the second,
// decisive predicate and it reads the payload; the sweep applies it in Inspect.
//
// The Bundle and empty-field guards are redundant given publisher.ParseConfigTopic's
// current contract and are kept as an upgrade tripwire: a future release that
// let a bundle topic carry a Platform would otherwise make this sweep retract a
// sibling's entire device document.
func OwnsConfigTopic(t publisher.ConfigTopic) bool {
	if t.Bundle || t.Platform == "" || t.NodeID != "" || t.ObjectID == "" {
		return false
	}
	if !publishedPlatforms[t.Platform] {
		return false
	}
	return strings.HasPrefix(t.ObjectID, hass.UniqueIDPrefix)
}

// publishedPlatforms are the seven Home Assistant platforms this bridge emits.
// A `climate` config in the daikin_ namespace is ours; a `vacuum` one is not.
var publishedPlatforms = map[string]bool{
	"binary_sensor": true,
	"button":        true,
	"climate":       true,
	"number":        true,
	"select":        true,
	"sensor":        true,
	"switch":        true,
}
