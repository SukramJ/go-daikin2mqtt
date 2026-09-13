// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
)

// ADR 0070 phase 8 step 5: the assertions about the go-hamqtt runtime itself.
//
// Everything here is about a property that MOVES NO PUBLISHED BYTE and is
// therefore invisible to the twelve pinned scenario goldens: the QoS sentinel,
// the legacy topic form the runtime states, the per-connection rebuild, the
// disjointness of what this daemon subscribes from what it publishes, and the
// report-only sweep's verdict. The goldens pin the wire; these pin the parts of
// the migration a wire capture cannot see.

// --- QoS -------------------------------------------------------------------

// TestQoSIsStatedNotDefaulted is F9 in one assertion.
//
// publisher.QoS's zero value is QoSUnset and every runtime field in that
// package resolves it to QoS 1. This bridge publishes at QoS 0 — measured over
// 1 083 recorded publishes — so all three of its QoS fields have to SAY so. The
// sentinel is 0x80, outside the wire's 0-2 range, precisely so that a struct
// literal cannot arrive at "deliberately at most once" by omission.
//
// The wire byte is asserted beside the sentinel because the sentinel alone
// would pass with any value the library happened to resolve; QoS.Wire() is what
// the transport is handed.
func TestQoSIsStatedNotDefaulted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		got  publisher.QoS
	}{
		{"StateQoS", StateQoS},
		{"StatePulseQoS", StatePulseQoS},
		{"CommandQoS", CommandQoS},
		{"DiscoveryQoS", DiscoveryQoS},
	} {
		if tc.got == publisher.QoSUnset {
			t.Errorf("%s is QoSUnset, which the library resolves to QoS 1 — state publisher.QoSAtMostOnce", tc.name)
		}
		wire, ok := tc.got.Wire()
		if !ok || wire != 0 {
			t.Errorf("%s resolves to wire %d (ok=%v), want 0", tc.name, wire, ok)
		}
	}
}

// TestEveryLibraryPublishReachesTheWireAtQoS0 reads the delivery guarantee off
// the TRANSPORT CALL rather than off a constant, for the three planes that now
// go through go-hamqtt: state, the retained clear, and the availability marker.
//
// That distinction is the one that survives a plane moving. A test that asserts
// the constant passes even if the constant is never reached; a test that reads
// the recorded publish cannot.
func TestEveryLibraryPublishReachesTheWireAtQoS0(t *testing.T) {
	t.Parallel()
	rec := &qosRecorder{}
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: rec, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	ctx := context.Background()
	c.PublishOnline(ctx)                                  // the availability marker, via publisher.Runtime
	c.publishState(ctx, "daikin/dev1/mp/x/state", "21.5") // the state plane
	c.publishState(ctx, "daikin/dev1/mp/y/state", "")     // the retained clear, via Evict

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("recorded %d publishes, want 3: %+v", len(got), got)
	}
	for _, p := range got {
		if p.qos != 0 {
			t.Errorf("%s published at QoS %d, want 0", p.topic, p.qos)
		}
		if !p.retain {
			t.Errorf("%s published unretained; every topic this daemon owns is retained", p.topic)
		}
	}
	if got[0].topic != "daikin/bridge/status" || string(got[0].payload) != "online" {
		t.Errorf("availability marker = %q %q, want daikin/bridge/status online", got[0].topic, got[0].payload)
	}
	// The empty payload stays an empty payload: retained + zero bytes is how
	// MQTT clears a retained topic, and four of the twelve pinned scenarios
	// contain one.
	if len(got[2].payload) != 0 {
		t.Errorf("the retained clear carried %q, want no bytes", got[2].payload)
	}
}

// --- the legacy topic form -------------------------------------------------

// TestRuntimeStatesTheLegacyTopicForm pins the one setting step 6 cannot
// discover for itself.
//
// publisher.Config.LegacyEntityTopics REPLACES the library's default rather
// than extending it. Unstated, it is the five-segment LegacyTopicWithNodeID,
// which reproduces 0 of this bridge's 264 config topics (measured at step 4) —
// so a bundle would be published while every per-entity config was still
// retained, Home Assistant would refuse it with one WARNING line, and no
// entity would appear. Nothing on the wire would say why.
//
// It is stated at step 5, one step before it is consumed, because it is free
// here (nothing publishes a bundle) and because a composition root that says
// nothing looks exactly like one that chose the default.
func TestRuntimeStatesTheLegacyTopicForm(t *testing.T) {
	t.Parallel()
	rt := publisher.New(&qosRecorderTransport{}, RuntimeConfig(testConfig(), slog.New(slog.DiscardHandler)))
	defer rt.Close()
	forms := rt.LegacyForms()
	if len(forms) != 1 || forms[0] != hass.LegacyConfigTopicForm {
		t.Errorf("runtime states legacy forms %v, want exactly [%s]", forms, hass.LegacyConfigTopicForm)
	}
	// And the constant the step-4 measurement recorded is the same function, so
	// the documented verdict and the wired-up value cannot drift.
	if !strings.HasSuffix(hass.LegacyConfigTopicForm, "LegacyTopicByUniqueID") {
		t.Errorf("LegacyConfigTopicForm = %q, want the by-unique-id form", hass.LegacyConfigTopicForm)
	}
}

// TestRuntimeDerivesTheStatusTopicFromTheLayout pins the birth/LWT
// convergence: one function feeds the will, the retained "online" and the
// availability_topic of all 264 payloads.
func TestRuntimeDerivesTheStatusTopicFromTheLayout(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"daikin", "haus/klima"} {
		cfg := &config.Config{MQTTTopic: root, HASSBaseTopic: "homeassistant", Language: "en"}
		rt := publisher.New(&qosRecorderTransport{}, RuntimeConfig(cfg, slog.New(slog.DiscardHandler)))
		want := layout.New(root).BridgeStatus()
		if got := rt.BridgeTopic(); got != want {
			t.Errorf("root %q: runtime status topic %q, want %q", root, got, want)
		}
		if adv := hass.New(cfg.HASSBaseTopic, root, "en", nil).BridgeStatusTopic(); adv != want {
			t.Errorf("root %q: discovery advertises %q, the runtime writes %q", root, adv, want)
		}
		rt.Close()
	}
}

// --- the per-connection rebuild --------------------------------------------

// TestPublishOnlineRebuildsTheRuntimeOnEveryConnect is the plumbing step 6
// depends on, asserted at step 5 where it still moves no byte.
//
// A publisher.Runtime remembers what it has superseded, declared and announced.
// All three are statements about A BROKER while the memo is per PROCESS, and at
// QoS 0 a successful publish is a statement about one connection. go-mtec2mqtt
// shipped the per-process version: after a reconnect its runtime reported the
// retractions as already applied, skipped them, published the document anyway,
// and Home Assistant refused it — retractions re-sent 0, document published
// true, configs still retained 1.
//
// The fix is structural rather than three fields being cleared, so a field the
// library adds later is covered the day it is added. This test therefore
// asserts the OBJECT changed, not that some field was reset.
func TestPublishOnlineRebuildsTheRuntimeOnEveryConnect(t *testing.T) {
	t.Parallel()
	built := 0
	rec := &qosRecorder{}
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: rec, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
		NewHARuntime: func() *publisher.Runtime {
			built++
			return publisher.New(hagomqtt.Split(rec, rec), RuntimeConfig(testConfig(), slog.New(slog.DiscardHandler)))
		},
	})
	if built != 1 {
		t.Fatalf("constructing the coordinator built %d runtimes, want 1", built)
	}
	first := c.ha()
	c.PublishOnline(context.Background()) // the first connect
	second := c.ha()
	c.PublishOnline(context.Background()) // a reconnect
	third := c.ha()

	if built != 3 {
		t.Errorf("the factory was called %d times, want 3 (construction + two connects)", built)
	}
	if first == second || second == third {
		t.Error("PublishOnline kept the previous connection's runtime; its memo of what it superseded would outlive the connection it was written on")
	}
}

// TestTheDedupGateReopensOnReconnect is the other half of a reconnect.
//
// A broker that came back without its retained store holds nothing, while the
// state plane's cache still believes every value is published. Without the
// reset every entity sits blank until its value happens to change, which on a
// sensor that reports on change alone is forever.
func TestTheDedupGateReopensOnReconnect(t *testing.T) {
	t.Parallel()
	rec := &qosRecorder{}
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: rec, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	ctx := context.Background()
	const topic = "daikin/dev1/climateControl/room_temperature/state"

	if w, _ := c.publishState(ctx, topic, "21.5"); !w {
		t.Fatal("the first publish did not reach the broker")
	}
	if w, _ := c.publishState(ctx, topic, "21.5"); w {
		t.Error("an unchanged value was written again; the dedup gate is not doing its job")
	}
	c.PublishOnline(ctx) // a reconnect
	if w, _ := c.publishState(ctx, topic, "21.5"); !w {
		t.Error("after a reconnect the gate still suppressed a value the broker may no longer hold")
	}
}

// --- subscription overlap ---------------------------------------------------

// TestSubscriptionFiltersCannotOverlap answers the question with evidence
// rather than with an argument.
//
// A broker sends one copy of a message PER MATCHING SUBSCRIPTION, so two
// filters that both match some topic run their handlers twice per message.
// openccu-loom measured exactly that against Mosquitto and needed a separate
// broker connection to fix it.
//
// This bridge has three writers onto one topic tree and four filters. They fall
// into two groups, and the groups are the answer:
//
//   - The PERMANENT ones — "<root>/+/+/+/set" on the main connection and
//     "state/<faikin host>" on the Faikin connection — are structurally
//     disjoint, tested here by registering them on one router: Handle refuses
//     an overlapping pair outright.
//   - The two DISCOVERY ones — "<prefix>/+/+/config" (the hand-rolled
//     reconcile) and "<prefix>/#" (the library's sweep window) — DO overlap,
//     and that is fine only because they are never installed at the same time:
//     both run inside reconcileOrphans' goroutine, behind the same try-locked
//     gate, and the reconcile unsubscribes before the sweep subscribes. This is
//     go-homeconnect2mqtt's case, not openccu-loom's, and the overlap is
//     asserted rather than assumed so that a future caller that hoists one of
//     them out of the gate has to come past this test.
func TestSubscriptionFiltersCannotOverlap(t *testing.T) {
	t.Parallel()
	root := layout.New(config.TopicRoot)

	r := publisher.NewCommandRouter(&qosRecorderTransport{}, publisher.CommandConfig{QoS: CommandQoS})
	nop := func(context.Context, publisher.Command) {}
	for _, f := range []string{root.CommandFilter(), "state/Klima WZ", "state/Klima KU"} {
		if err := r.Handle(f, nop); err != nil {
			t.Fatalf("the permanent filters overlap: %v", err)
		}
	}
	// Sanity: the router really does refuse an overlap, so the pass above is
	// not vacuous.
	if err := r.Handle(config.TopicRoot+"/+/+/+/#", nop); err == nil {
		t.Error("the router accepted a filter overlapping the command route; this test proves nothing")
	}

	// The two discovery filters: each one disjoint from the permanent plane,
	// and — the fact worth stating — NOT from each other.
	const reconcileFilter = "homeassistant/+/+/config"
	const sweepFilter = "homeassistant/#"
	for _, f := range []string{reconcileFilter, sweepFilter} {
		fresh := publisher.NewCommandRouter(&qosRecorderTransport{}, publisher.CommandConfig{QoS: CommandQoS})
		if err := fresh.Handle(root.CommandFilter(), nop); err != nil {
			t.Fatalf("registering the command route: %v", err)
		}
		if err := fresh.Handle(f, nop); err != nil {
			t.Errorf("discovery filter %q overlaps the command plane: %v", f, err)
		}
	}
	both := publisher.NewCommandRouter(&qosRecorderTransport{}, publisher.CommandConfig{QoS: CommandQoS})
	if err := both.Handle(reconcileFilter, nop); err != nil {
		t.Fatalf("registering the reconcile filter: %v", err)
	}
	if err := both.Handle(sweepFilter, nop); err == nil {
		t.Error("the two discovery filters no longer overlap; the serialisation below is no longer load-bearing " +
			"and reconcileOrphans can be simplified")
	}
	// They overlap, so a broker would deliver every retained config twice while
	// both were installed. What makes that safe is that they never are: the
	// reconcile unsubscribes before the sweep subscribes, inside one goroutine
	// holding the reconcile gate.
	if !publisher.MatchFilter(reconcileFilter, "homeassistant/sensor/daikin_x/config") ||
		!publisher.MatchFilter(sweepFilter, "homeassistant/sensor/daikin_x/config") {
		t.Error("the two discovery filters no longer both match a config topic; the claim above is stale")
	}
}

// TestNothingThisDaemonPublishesIsAlsoSubscribed drives the library's own
// guard over the real catalogue.
//
// A state topic inside this process's own command filter is delivered straight
// back as a command it issued to itself — the sharpest defect the library
// records, which in the measured case ran every Home Assistant "program" on the
// device on every boot. It is a boot failure here, not a warning.
func TestNothingThisDaemonPublishesIsAlsoSubscribed(t *testing.T) {
	t.Parallel()
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: newStubMQTT(), Catalog: loadRealCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	if err := c.subscribeWrites(context.Background()); err != nil {
		t.Fatalf("subscribeWrites: %v", err)
	}
	t.Cleanup(func() { _ = c.commands.Stop(context.Background()) })
	if err := c.checkCommandDisjoint(); err != nil {
		t.Fatalf("checkCommandDisjoint: %v", err)
	}
	// Not vacuous: a topic that really is inside the command filter is caught.
	if err := c.commands.CheckDisjoint(c.topicRoot.Slot("dev", "mp", "power").Command()); err == nil {
		t.Error("CheckDisjoint accepted a command topic as something to publish; this test proves nothing")
	}
	// And the probe set is not empty, which is the other way this could pass
	// while checking nothing.
	slots := c.knownSlots()
	if len(slots) < 50 {
		t.Errorf("the disjointness probe covered %d slots, want the whole catalogue", len(slots))
	}

	// The property behind the guard, asserted directly rather than only through
	// it. Every topic this layout can produce ends in /state or /attributes
	// while the command filter's last level is the literal `set`, so NO input
	// the catalogue can supply makes checkCommandDisjoint fail today — it is an
	// upgrade tripwire for a future layout change, and a mutation that stops it
	// failing is therefore equivalent. What is NOT equivalent is the layout
	// growing a topic that collides, and this is what catches that.
	filter := c.topicRoot.CommandFilter()
	for _, s := range slots {
		for _, topic := range []string{s.State(), s.Attributes()} {
			if publisher.MatchFilter(filter, topic) {
				t.Errorf("%s falls inside the command filter %q", topic, filter)
			}
		}
		// The command topic must match, or the filter subscribes to nothing the
		// discovery payloads advertise.
		if !publisher.MatchFilter(filter, s.Command()) {
			t.Errorf("%s is advertised as a command topic but %q does not match it", s.Command(), filter)
		}
	}
}

// --- the report-only sweep --------------------------------------------------

// TestReportOnlySweepOverTheRealFleet is the measurement this step owes.
//
// It opens a real publisher.Runtime.Sweep window over a broker seeded with one
// instance's live fleet plus every class of retained config a shared discovery
// tree actually carries, and asserts both the verdict AND the reason each
// survivor survived. The fan-out is the point: a count alone cannot tell a
// predicate that judged correctly from one that saw nothing.
func TestReportOnlySweepOverTheRealFleet(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, rt := sweepCoordinator(t, br)
	defer rt.Close()
	// Exactly what a poll claims: device ids, and not the scheduler's shared
	// reserved segment (F-A).
	c.deps.HASS.ClaimDevices([]string{"dev1"})

	// This instance's live fleet: three configs it is publishing right now.
	live := map[string]bool{}
	for _, x := range []struct{ topic, body string }{
		{
			"homeassistant/sensor/daikin_dev1_room_temperature/config",
			`{"unique_id":"daikin_dev1_room_temperature","state_topic":"daikin/dev1/climateControl/room_temperature/state"}`,
		},
		{
			"homeassistant/climate/daikin_dev1_climate/config",
			`{"unique_id":"daikin_dev1_climate","mode_state_topic":"daikin/dev1/climateControl/hvac_mode/state"}`,
		},
		{
			"homeassistant/switch/daikin_schedule_werktag/config",
			`{"unique_id":"daikin_schedule_werktag","state_topic":"daikin/scheduler/werktag/enabled/state"}`,
		},
	} {
		br.retain(x.topic, x.body)
		live[x.topic] = true
	}

	// One real orphan: this instance's own, from a catalogue that no longer has
	// the entry. Its state topic is still under a device it polls.
	const orphan = "homeassistant/sensor/daikin_dev1_retired_sensor/config"
	br.retain(orphan, `{"unique_id":"daikin_dev1_retired_sensor","state_topic":"daikin/dev1/climateControl/retired_sensor/state"}`)

	// Everything a shared discovery tree also carries.
	for _, x := range []struct{ topic, body string }{
		// Another integration, another namespace.
		{"homeassistant/sensor/zigbee2mqtt_0x00124b/config", `{"unique_id":"zigbee2mqtt_0x00124b","state_topic":"zigbee2mqtt/x"}`},
		{"homeassistant/binary_sensor/tasmota_ABC123_status/config", `{"unique_id":"tasmota_ABC123","state_topic":"tasmota/x"}`},
		// A device bundle — not the four-segment form.
		{"homeassistant/device/daikin_dev9/config", `{"dev":{"ids":["daikin_dev9"]},"cmps":{}}`},
		// The five-segment node-id form this bridge has never published.
		{"homeassistant/sensor/daikinnode/daikin_via_node/config", `{"unique_id":"daikin_via_node","state_topic":"daikin/dev1/climateControl/x/state"}`},
		// A platform this daemon never emits, in its own namespace.
		{"homeassistant/vacuum/daikin_dev1_hoover/config", `{"unique_id":"daikin_dev1_hoover","state_topic":"daikin/dev1/climateControl/hoover/state"}`},
		// Our namespace on the topic, somebody else's payload.
		{"homeassistant/sensor/daikin_handwritten/config", `{"state_topic":"daikin/dev1/climateControl/x/state"}`},
		// Not a config topic at all.
		{"homeassistant/sensor/daikin_dev1_x/config.bak", `{"unique_id":"daikin_dev1_x"}`},
		// THE case that matters: a second go-daikin2mqtt instance, on the SAME
		// MQTT root and the same discovery prefix, publishing a device this
		// instance does not poll. Every string in the topic is a string this
		// instance could have produced.
		{
			"homeassistant/sensor/daikin_dev2_room_temperature/config",
			`{"unique_id":"daikin_dev2_room_temperature","state_topic":"daikin/dev2/climateControl/room_temperature/state"}`,
		},
		{
			"homeassistant/climate/daikin_dev2_climate/config",
			`{"unique_id":"daikin_dev2_climate","mode_state_topic":"daikin/dev2/climateControl/hvac_mode/state"}`,
		},
		// A sibling with a different MQTT root: the easy edge, kept beside the
		// hard one so a predicate that only ever handled this one is visible.
		{
			"homeassistant/sensor/daikin_dev3_room_temperature/config",
			`{"unique_id":"daikin_dev3_room_temperature","state_topic":"klima/dev3/climateControl/room_temperature/state"}`,
		},
	} {
		br.retain(x.topic, x.body)
	}

	// One config this process published EARLIER and is not publishing in this
	// batch. It is still not an orphan of anybody's making, and the runtime's
	// own declarations are what say so — the `published` argument alone would
	// have retracted it.
	const declared = "homeassistant/sensor/daikin_dev1_declared_earlier/config"
	declaredBody := `{"unique_id":"daikin_dev1_declared_earlier","state_topic":"daikin/dev1/climateControl/declared_earlier/state"}`
	if _, err := rt.Publish(context.Background(), declared, []byte(declaredBody)); err != nil {
		t.Fatalf("seeding a declared config: %v", err)
	}
	br.retain(declared, declaredBody)

	fan, res := c.sweepReport(context.Background(), rt, live)

	if got := []string{orphan}; !equalStrings(fan.Orphan, got) {
		t.Errorf("would retract %v, want %v", fan.Orphan, got)
	}
	if fan.Claimed != len(live)+1 {
		t.Errorf("claimed %d, want %d (this batch plus what the runtime still declares)", fan.Claimed, len(live)+1)
	}
	if fan.Sibling != 3 {
		t.Errorf("sibling configs %d, want 3 (two on this root, one on another)", fan.Sibling)
	}
	if fan.Foreign != 1 {
		t.Errorf("foreign-payload configs %d, want 1", fan.Foreign)
	}
	// Inspected is the half that tells a correct verdict from an empty window:
	// "0 retracted" reads the same either way.
	if res.Inspected != fan.Claimed+fan.Sibling+fan.Foreign+len(fan.Orphan) {
		t.Errorf("inspected %d does not account for the fan-out %+v", res.Inspected, fan)
	}
	// Nine of fifteen: the other six retained configs were declined by the
	// TOPIC predicate before their payload was ever read — another namespace
	// (2), a bundle, the five-segment node-id form, a platform this daemon
	// never emits, and a topic that is not a config topic at all.
	if res.Inspected != 9 {
		t.Errorf("inspected %d owned configs, want 9 — the window saw the wrong set", res.Inspected)
	}
	// Nothing went out beyond the one seeded declaration. A report-only pass
	// that published anything would be the whole hazard.
	if n := br.publishes(); n != 1 {
		t.Errorf("the report-only pass published %d messages beyond the seeded one, want 0", n-1)
	}
	t.Logf("report-only sweep: %d retained configs offered, %d inspected, %d claimed, "+
		"%d another instance's, %d another integration's, %d would be retracted: %v",
		br.retainedCount(), res.Inspected, fan.Claimed, fan.Sibling, fan.Foreign, len(fan.Orphan), fan.Orphan)
}

// TestAStaggeredUpgradeDoesNotReachTheSiblingsFleet is F14, driven rather than
// argued.
//
// The failure it excludes was proved by go-mtec2mqtt's reviewer: instance A
// upgrades, publishes a device bundle and retracts "its" per-entity configs
// under publisher.LegacyTopicByUniqueID; instance B is still on the old build.
// Because the legacy form is keyed on a unique_id both instances produce
// identically, and the bundle node id is sanitize(dev.UID()) — likewise
// identical — neither instance can tell its own retained configs from the
// other's by TOPIC. A's retraction then deletes B's entire fleet, permanently,
// because B has no reason to republish.
//
// This drives the actual sweep against a sibling's retained configs and asserts
// the retraction list is EMPTY, rather than asserting the predicate and
// assuming the sweep uses it — the exact weakness mtec's reviewer faulted in the
// earlier version of its own test. The sibling here differs only in the device:
// same broker, same discovery prefix, same MQTT root, same namespace, same
// topic form.
func TestAStaggeredUpgradeDoesNotReachTheSiblingsFleet(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, rt := sweepCoordinator(t, br)
	defer rt.Close()
	c.deps.HASS.ClaimDevices([]string{"dev1"})

	// The sibling's whole fleet, published by an instance that polls dev2.
	sibling := []string{
		"homeassistant/sensor/daikin_dev2_room_temperature/config",
		"homeassistant/sensor/daikin_dev2_outdoor_temperature/config",
		"homeassistant/switch/daikin_dev2_powerful/config",
		"homeassistant/climate/daikin_dev2_climate/config",
		"homeassistant/button/daikin_outdoor_ODU2_refresh/config",
	}
	for _, topic := range sibling {
		uid := strings.Split(topic, "/")[2]
		br.retain(topic, `{"unique_id":"`+uid+`","state_topic":"daikin/dev2/climateControl/x/state"}`)
	}

	fan, res := c.sweepReport(context.Background(), rt, map[string]bool{})

	if len(fan.Orphan) != 0 {
		t.Fatalf("the sweep would have retracted %d of a sibling instance's configs: %v", len(fan.Orphan), fan.Orphan)
	}
	if fan.Sibling != len(sibling) {
		t.Errorf("recognised %d sibling configs, want %d", fan.Sibling, len(sibling))
	}
	// Not vacuous: the window really did deliver them, and the TOPIC predicate
	// really did own every one. Only the payload predicate declined them, which
	// is the whole of the fix.
	if res.Inspected != len(sibling) {
		t.Errorf("inspected %d, want %d — the window did not see the sibling's fleet", res.Inspected, len(sibling))
	}
	for _, topic := range sibling {
		ct, ok := publisher.ParseConfigTopic("homeassistant", topic)
		if !ok || !OwnsConfigTopic(ct) {
			t.Errorf("%s: the topic predicate did not own it, so this test is not exercising the payload one", topic)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// sweepCoordinator builds a coordinator whose runtime and state plane speak to
// br, with discovery enabled and nothing claimed yet.
func sweepCoordinator(t *testing.T, br *retainedBroker) (*Coordinator, *publisher.Runtime) {
	t.Helper()
	cfg := testConfig()
	// Stated rather than left at the zero value: config.Normalize defaults this
	// for the daemon, but publisher.New substitutes discovery.DefaultPrefix for
	// an empty Prefix while hass.Discovery does not — so an unnormalised config
	// would put the two planes on different discovery trees. See the notes' F15.
	cfg.HASSBaseTopic = config.DefaultHASSBaseTopic
	logger := slog.New(slog.DiscardHandler)
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: br, Catalog: loadTestCatalog(t),
		HASS:   hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, br),
		Logger: logger, Clock: fixedClock(),
	})
	// Shrunk on this coordinator rather than on the package, so a parallel
	// test's reconcile goroutine is not reading a var this one writes.
	c.collectWindow = 20 * time.Millisecond
	rt := c.ha()
	return c, rt
}

// runArmedSweep drives the real armed sweep to completion.
//
// published is the claim set — the device documents this batch wrote. It is
// passed in rather than derived so a test can state the worst case (an empty
// set, which the sweep must refuse outright) as easily as the ordinary one.
//
// The sweep runs asynchronously behind a try-locked gate; taking the gate is
// what says it has finished.
func runArmedSweep(t *testing.T, c *Coordinator, published map[string]bool) {
	t.Helper()
	c.sweepOrphans(context.Background(), published)
	deadline := time.Now().Add(10 * time.Second)
	for !c.reconcileGate.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the sweep never released its gate")
		}
		time.Sleep(time.Millisecond)
	}
	c.reconcileGate.Unlock()
}

// aClaimedDocument is a non-empty claim set for a sweep that is not itself
// under test. Its content is irrelevant — what matters is that it is not empty,
// because an empty one makes the sweep refuse to run at all.
func aClaimedDocument() map[string]bool {
	return map[string]bool{"homeassistant/device/daikin_claimed/config": true}
}

// retainedBroker is an mqtt.Client that replays its retained messages to a new
// subscriber, the way a broker does — which is the only thing a sweep window
// reads.
type retainedBroker struct {
	mu       sync.Mutex
	messages map[string]string
	sent     int
	events   []string
	// order is every publish in the order it was made — the only thing that can
	// answer the question the migration turns on, which is whether the
	// retractions preceded the document.
	order []string
	// failPrefix refuses every publish under it, so a test can put the daemon
	// in the state the migration cannot survive on its own: retractions
	// applied, document refused.
	failPrefix string
	// onPublish runs before each publish is recorded, so a test can make
	// something happen mid-batch — a reconnect landing while the documents are
	// still going out, for instance.
	onPublish func(topic string)
}

func newRetainedBroker() *retainedBroker {
	return &retainedBroker{messages: map[string]string{}}
}

// retain seeds one retained message without counting it as a publish.
func (b *retainedBroker) retain(topic, payload string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages[topic] = payload
}

func (b *retainedBroker) Publish(
	_ context.Context, topic string, payload []byte, _ mqtt.QoS, retain bool, _ ...mqtt.PublishOption,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failPrefix != "" && strings.HasPrefix(topic, b.failPrefix) {
		return errors.New("broker down")
	}
	if b.onPublish != nil {
		hook := b.onPublish
		b.mu.Unlock()
		hook(topic)
		b.mu.Lock()
	}
	b.sent++
	b.order = append(b.order, topic)
	if retain {
		b.messages[topic] = string(payload)
	}
	return nil
}

// publishOrder returns every publish in order.
func (b *retainedBroker) publishOrder() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...)
}

// refuse makes every publish under prefix fail; "" lifts it.
func (b *retainedBroker) refuse(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failPrefix = prefix
}

func (b *retainedBroker) publishes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent
}

// retained returns one retained payload and whether the topic is present at
// all. A retraction leaves the topic present with an EMPTY payload here, which
// is what lets a test tell "cleared" from "never written".
func (b *retainedBroker) retained(topic string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.messages[topic]
	return v, ok
}

func (b *retainedBroker) retainedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

func (b *retainedBroker) Subscribe(
	_ context.Context, filter string, _ mqtt.QoS, h mqtt.MessageHandler, _ ...mqtt.SubscribeOption,
) (mqtt.SubscribeResult, error) {
	b.mu.Lock()
	b.events = append(b.events, "sub "+filter)
	msgs := make(map[string]string, len(b.messages))
	for k, v := range b.messages {
		msgs[k] = v
	}
	b.mu.Unlock()
	topics := make([]string, 0, len(msgs))
	for topic := range msgs {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	for _, topic := range topics {
		if publisher.MatchFilter(filter, topic) {
			h(&mqtt.Message{Topic: topic, Payload: []byte(msgs[topic]), Retain: true})
		}
	}
	return mqtt.SubscribeResult{}, nil
}

func (b *retainedBroker) Unsubscribe(_ context.Context, filter string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, "unsub "+filter)
	return nil
}

// log returns the subscribe/unsubscribe sequence, in order.
func (b *retainedBroker) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

// recordedPublish is one transport call, as the wire saw it.
type recordedPublish struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

// qosRecorder is an mqtt.Client recording the QoS and retain flag of every
// publish, in order.
type qosRecorder struct {
	mu   sync.Mutex
	msgs []recordedPublish
}

func (r *qosRecorder) Publish(
	_ context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, _ ...mqtt.PublishOption,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, recordedPublish{
		topic: topic, payload: append([]byte(nil), payload...), qos: byte(qos), retain: retain,
	})
	return nil
}

func (r *qosRecorder) Subscribe(
	context.Context, string, mqtt.QoS, mqtt.MessageHandler, ...mqtt.SubscribeOption,
) (mqtt.SubscribeResult, error) {
	return mqtt.SubscribeResult{}, nil
}

func (r *qosRecorder) Unsubscribe(context.Context, string) error { return nil }

func (r *qosRecorder) snapshot() []recordedPublish {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPublish(nil), r.msgs...)
}

// TestStatePlaneStatesBothStateLevels is the assertion that PulseQoS is
// STATED rather than inherited, and it is deliberately written so that it
// cannot pass by the coincidence that makes the omission invisible today.
//
// publisher.StateConfig.PulseQoS is the one field in that package whose unset
// default is QoS 0 rather than QoS 1. This bridge publishes at QoS 0, so a
// StateConfig that omits the field reaches the same wire byte as one that
// states it, and any test that simply asserts "pulses go out at QoS 0" passes
// either way. That is the trap: the omission is correct by arithmetic
// accident, not by choice, and StateQoS is explicitly contemplated as
// changeable in its own step.
//
// So the assertion is RELATIVE: the level a pulse actually reaches the
// transport at must equal the level this package states for it. Mutate
// StateQoS and StatePulseQoS to QoSAtLeastOnce and the test still passes;
// mutate them and delete `PulseQoS: StatePulseQoS` from NewStatePlane and the
// pulse stays at 0 while the stated level is 1, and this fails. A test that
// pinned the literal 0 would not.
func TestStatePlaneStatesBothStateLevels(t *testing.T) {
	t.Parallel()
	rec := &pulseRecorderTransport{}
	plane := NewStatePlane(rec, layout.New("daikin"), slog.New(slog.DiscardHandler))

	wantState, ok := StateQoS.Wire()
	if !ok {
		t.Fatalf("StateQoS does not resolve to a wire level")
	}
	wantPulse, ok := StatePulseQoS.Wire()
	if !ok {
		t.Fatalf("StatePulseQoS does not resolve to a wire level")
	}

	if _, err := plane.Publish(context.Background(), "daikin/dev1/mp/x/state", []byte("21.5")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := plane.Pulse(context.Background(), "daikin/dev1/mp/x/state", []byte("21.5")); err != nil {
		t.Fatalf("Pulse: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("recorded %d publishes, want 2 (one Publish, one Pulse): %+v", len(got), got)
	}
	if got[0].qos != wantState {
		t.Errorf("Publish reached the transport at QoS %d, want StateQoS's %d", got[0].qos, wantState)
	}
	if got[1].qos != wantPulse {
		t.Errorf(
			"Pulse reached the transport at QoS %d, want StatePulseQoS's %d — "+
				"publisher.StateConfig.PulseQoS does not inherit QoS and defaults to 0, "+
				"so NewStatePlane has to state it",
			got[1].qos, wantPulse,
		)
	}
}

// TestStatePlaneDoesNotWarnAboutAnUnstatedPulseQoS is the assertion that
// actually fails today, and it is the reason the go-hamqtt bump rides in this
// PR rather than waiting.
//
// TestStatePlaneStatesBothStateLevels above is relative and therefore only
// bites once StateQoS moves. At today's QoS 0 no behavioural test can see the
// omission at all, because publisher.StateConfig.PulseQoS's unset default and
// this bridge's chosen level are the same number. go-hamqtt v0.34.0 closes
// exactly that blind spot: NewStatePublisher warns
// `publisher.state.pulse_qos_unstated` when one state level is stated and the
// other is not, whatever the levels are. So the checkable property is the
// absence of that warning from this daemon's own construction path — and on
// v0.32.0 it was not available at all, which is the whole cost of the version
// gap this repository was carrying.
func TestStatePlaneDoesNotWarnAboutAnUnstatedPulseQoS(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	NewStatePlane(&pulseRecorderTransport{}, layout.New("daikin"), logger)

	if strings.Contains(buf.String(), "publisher.state.pulse_qos_unstated") {
		t.Errorf(
			"NewStatePlane logged publisher.state.pulse_qos_unstated at construction:\n%s\n"+
				"state publisher.StateConfig.PulseQoS explicitly — it does not inherit "+
				"StateConfig.QoS and is the one field in the package defaulting to QoS 0",
			buf.String(),
		)
	}
}

// pulseRecorderTransport is a publisher.Transport that records what it was
// handed, for the assertions that have to read a level off the wire rather
// than off a constant.
type pulseRecorderTransport struct {
	mu   sync.Mutex
	msgs []recordedPublish
}

func (r *pulseRecorderTransport) Publish(
	_ context.Context, topic string, payload []byte, qos byte, retain bool,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, recordedPublish{
		topic: topic, payload: append([]byte(nil), payload...), qos: qos, retain: retain,
	})
	return nil
}

func (r *pulseRecorderTransport) Subscribe(context.Context, string, byte, publisher.Handler) error {
	return nil
}

func (r *pulseRecorderTransport) Unsubscribe(context.Context, string) error { return nil }

func (r *pulseRecorderTransport) snapshot() []recordedPublish {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPublish(nil), r.msgs...)
}

// qosRecorderTransport is a publisher.Transport that accepts everything, for
// the constructor-level assertions that never publish.
type qosRecorderTransport struct{}

func (qosRecorderTransport) Publish(context.Context, string, []byte, byte, bool) error { return nil }

func (qosRecorderTransport) Subscribe(context.Context, string, byte, publisher.Handler) error {
	return nil
}
func (qosRecorderTransport) Unsubscribe(context.Context, string) error { return nil }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEveryStateTopicGoesThroughTheDedupGate is the structural proof that the
// state plane really did move — every site of it, not just the one a unit test
// happens to call.
//
// The pinned harness runs two poll cycles, so before this step every
// steady-state topic was written to the broker TWICE with identical bytes. The
// golden cannot see that: it collapses byte-identical repeats, which is exactly
// what makes it a pin on the published surface rather than on the traffic. Here
// the raw recording is read instead, and a topic written twice is a publish
// site that bypassed the library.
//
// The exemption is stated rather than filtered: a retained CLEAR goes through
// StatePublisher.Evict, which has no dedup gate (and must not — the topic is
// dropped from the index so the next real value is not compared against a
// retraction), so those are written once per call by design.
func TestEveryStateTopicGoesThroughTheDedupGate(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"multisplit.en", "multisplit.local.en", "multisplit.scheduler.en"} {
		raw := buildSurfaceRecorder(t, scenarioNamed(t, name)).raw()
		// Keyed on the WHOLE message, not on the topic: a topic legitimately
		// carries two different values in one run — the cloud poll publishes a
		// unit's hvac_mode and the Faikin read path then publishes the live one
		// — and that is a second value, not a repeat. Only a byte-identical
		// re-write is evidence of a site that bypassed the gate.
		counts := map[string]int{}
		empties := map[string]bool{}
		for _, m := range raw {
			if !strings.HasPrefix(m.Topic, config.TopicRoot+"/") {
				continue // the discovery plane is step 6's
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("%s: encode recorded message: %v", name, err)
			}
			counts[string(b)]++
			if m.Empty {
				empties[string(b)] = true
			}
		}
		if len(counts) == 0 {
			t.Fatalf("%s: no state topic recorded at all", name)
		}
		repeated := 0
		for msg, n := range counts {
			if n > 1 && !empties[msg] {
				repeated++
				if repeated < 4 {
					t.Errorf("%s: %s written %d times with the same bytes — that publish site does not go "+
						"through the state plane", name, msg, n)
				}
			}
		}
		if repeated >= 4 {
			t.Errorf("%s: %d topics were re-written with identical bytes", name, repeated)
		}
	}
}

// scenarioNamed looks one pinned scenario up by name.
func scenarioNamed(t *testing.T, name string) surfaceScenario {
	t.Helper()
	for _, sc := range surfaceScenarios() {
		if sc.name == name {
			return sc
		}
	}
	t.Fatalf("no scenario %q", name)
	return surfaceScenario{}
}

// TestAPollClaimsTheDevicesItResolved is the other half of F14's fix: the
// predicate is only as good as the set it is told.
//
// Without this the claim set would stay empty, IsOwnConfig would refuse
// everything and the orphan reconcile would silently stop clearing this
// instance's own retired entities — a failure that looks exactly like "there
// were no orphans".
func TestAPollClaimsTheDevicesItResolved(t *testing.T) {
	t.Parallel()
	const dev, emb = "dev1", "climateControl"
	m := newStubMQTT()
	cfg := testConfig()
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{devices: devicesJSON(dev, emb)}, MQTT: m,
		Catalog: loadTestCatalog(t),
		HASS:    hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, m),
		Logger:  slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	if got := c.deps.HASS.ClaimedDevices(); len(got) != 0 {
		t.Errorf("before any poll the instance claimed %v", got)
	}
	c.pollOnce(context.Background())
	if got := c.deps.HASS.ClaimedDevices(); !equalStrings(got, []string{dev}) {
		t.Errorf("after a poll the instance claims %v, want [%s]", got, dev)
	}
	// Attaching a scheduler changes nothing: the scheduler's reserved segment
	// is a compile-time literal every instance writes under, so claiming it
	// made a sibling's live schedule switches resolve as this instance's own
	// (F-A). See TestThePollClaimsNoSchedulerSegment.
	c.AttachScheduler(&stubScheduler{doc: goldenScheduleDoc()})
	c.pollOnce(context.Background())
	if got := c.deps.HASS.ClaimedDevices(); !equalStrings(got, []string{dev}) {
		t.Errorf("with a scheduler attached the instance claims %v, want [%s] — "+
			"the scheduler's shared segment must not be claimed", got, dev)
	}
}

// TestTheStatePlaneRefusesToPublishIntoItsOwnCommandTree asserts the library
// guard StateConfig.CommandFilters buys, which no scenario can reach on its own.
//
// Every topic this bridge's layout produces ends in /state or /attributes while
// the command filter's last level is the literal `set`, so nothing the
// catalogue can supply lands inside the subscription — the guard is inert
// today. Inert is not the same as absent: a state publish that DID land there
// would be delivered straight back to this process as a command it issued to
// itself, and the library refuses it rather than echoing it. The only way to
// assert that is to hand it the topic on purpose.
func TestTheStatePlaneRefusesToPublishIntoItsOwnCommandTree(t *testing.T) {
	t.Parallel()
	rec := &qosRecorder{}
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{}, MQTT: rec, Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	cmd := c.topicRoot.Slot("dev1", "climateControl", "power").Command()
	if _, ok := c.publishState(context.Background(), cmd, "on"); ok {
		t.Errorf("the state plane published to %s, which this daemon subscribes as a command", cmd)
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("%d messages reached the broker; the collision guard is not configured", n)
	}
}

// TestTheSweepOpensOneWindowAndRetractsFromIt drives the real armed sweep end
// to end.
//
// Three things are asserted. The sweep runs at all — a mutation that drops the
// call is otherwise invisible, because the orphan it clears is one topic among
// a tree nothing else touches. It opens exactly ONE window, `homeassistant/#`,
// and closes it: step 5 had two overlapping windows (the hand-rolled
// `homeassistant/+/+/config` reconcile and the library's report-only pass) kept
// apart only by their sequencing inside one gated goroutine, and this step
// removes the hand-rolled one rather than continuing to sequence it. And what
// the window found IS retracted, which is the arming.
func TestTheSweepOpensOneWindowAndRetractsFromIt(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, _ := sweepCoordinator(t, br)
	c.deps.HASS.ClaimDevices([]string{"dev1"})
	const orphan = "homeassistant/sensor/daikin_dev1_retired/config"
	br.retain(orphan,
		`{"unique_id":"daikin_dev1_retired","state_topic":"daikin/dev1/climateControl/retired/state"}`)

	runArmedSweep(t, c, aClaimedDocument())

	want := []string{"sub homeassistant/#", "unsub homeassistant/#"}
	if got := br.log(); !equalStrings(got, want) {
		t.Errorf("subscription sequence %v, want %v", got, want)
	}
	if got, ok := br.retained(orphan); !ok || got != "" {
		t.Errorf("the orphan was not retracted: retained %q (present=%v)", got, ok)
	}
}

// TestTheSweepIsArmedOnlyOverAPopulatedClaimSet is the guard step 5 said had to
// exist before the sweep could ever act, and the one go-mtec2mqtt's reviewer
// found testing the wrong thing.
//
// The library's acting pass compares what a window saw against what the runtime
// has CLAIMED. Until this step the runtime published no discovery at all, so
// that set was empty and an armed pass would have judged this bridge's entire
// retained fleet an orphan — once per boot. What makes arming possible is that
// the device documents now go out through that same runtime.
//
// So the question the guard must ask is "was a document PUBLISHED", not "was
// one BUILT". The two come apart exactly where it matters: a valid document
// whose publish failed (an open circuit breaker, a broker brownout) leaves the
// per-entity configs that are still carrying the fleet — and a sweep would then
// delete every one of them and put nothing back. go-mtec2mqtt logged "no device
// document was published" while testing that one had been built.
func TestTheSweepIsArmedOnlyOverAPopulatedClaimSet(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, _ := sweepCoordinator(t, br)
	c.deps.HASS.ClaimDevices([]string{"dev1"})
	br.retain("homeassistant/sensor/daikin_dev1_retired/config",
		`{"unique_id":"daikin_dev1_retired","state_topic":"daikin/dev1/climateControl/retired/state"}`)

	// The build-succeeded-publish-failed state, spelled as the claim set the
	// publish path hands over: empty.
	runArmedSweep(t, c, map[string]bool{})

	if got := br.log(); len(got) != 0 {
		t.Errorf("the sweep opened a window with nothing published: %v", got)
	}
	if n := br.publishes(); n != 0 {
		t.Errorf("%d retractions went out over an empty claim set", n)
	}
}

// TestASiblingsStatelessEntitiesAreNeverSwept is the regression test for a
// defect that was LIVE before this PR, not a hazard step 6 would have created.
//
// The predicate this bridge shipped was
//
//	strings.HasPrefix(uid, "daikin_") &&
//		(stateTopic == "" || strings.HasPrefix(stateTopic, root+"/"))
//
// and the escape hatch is the whole defect: when a config carries no
// `state_topic` the rule collapses to the `daikin_` namespace, which EVERY
// instance of this bridge shares. 24 of the 264 pinned configs carry no
// `state_topic` — the 14 composite climate entities, which name
// mode_state_topic / temperature_state_topic / current_temperature_topic and
// never a plain one, and the 10 refresh buttons, which are stateless by
// definition. Those 24 are exactly this bridge's flagship entity and its one
// daemon action.
//
// So a second instance — a second ONECTA account, a holiday home, a staging
// daemon — had its climate cards and refresh buttons retracted from Home
// Assistant's entity registry by the first instance's orphan reconcile, on
// every discovery-signature change, silently. The reconcile is live today: it
// subscribes the SHARED `homeassistant/+/+/config` and gates on this payload
// predicate alone.
//
// The fixtures here are RENDERED by the real builders, not hand-written: the
// hand-written ones are what let the hole be encoded as intended behaviour in
// the first place (discovery_test.go asserted `want: true` for a climate
// payload with no state_topic). The sibling is another scenario of this same
// bridge on the SAME MQTT root and the SAME discovery prefix — not another
// integration, which is the case the one pre-existing sweep test covered.
func TestASiblingsStatelessEntitiesAreNeverSwept(t *testing.T) {
	t.Parallel()

	// Across all twelve pinned scenarios, which configs the old rule could not
	// key on. Held as literals, because the number IS the exposed surface.
	stateless := map[string]int{}
	total := 0
	for _, sc := range surfaceScenarios() {
		for topic, cfg := range configsOf(buildSurface(t, sc)) {
			total++
			if str(cfg, "state_topic") == "" {
				stateless[strings.Split(topic, "/")[1]]++
			}
		}
	}
	if total != 264 {
		t.Errorf("rendered %d configs, want the pinned 264", total)
	}
	if want := (map[string]int{"climate": 14, "button": 10}); !equalCounts(stateless, want) {
		t.Errorf("configs with no state_topic = %v, want %v — the class the old predicate could not key on", stateless, want)
	}

	// One instance polls the multi-split; a sibling polls a different device on
	// the same broker, same root, same prefix.
	own := surfaceOf(t, "multisplit.en")
	sibling := configsOf(surfaceOf(t, "altherma-air-to-water-wlan.en"))

	br := newRetainedBroker()
	c, _ := sweepCoordinator(t, br)
	c.deps.HASS.ClaimDevices([]string{
		"809d41d9-4d42-45fa-af6a-84b512143672",
		"11112222-3333-4444-5555-666677778888",
	})

	retained := map[string][]byte{}
	statelessSibling := 0
	for topic, cfg := range sibling {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("re-encode %s: %v", topic, err)
		}
		retained[topic] = b
		if str(cfg, "state_topic") == "" {
			statelessSibling++
		}
	}
	if statelessSibling < 2 {
		t.Fatalf("the sibling fixture carries %d stateless configs; it must carry the climate and the button "+
			"or this test exercises nothing", statelessSibling)
	}
	// Not vacuous the other way either: this instance's own configs must still
	// be claimable, or "nothing was swept" would be true for the wrong reason.
	ownClaimed := 0
	for _, cfg := range configsOf(own) {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if c.deps.HASS.IsOwnConfig(b) {
			ownClaimed++
		}
	}
	if ownClaimed != len(configsOf(own)) {
		t.Errorf("this instance claims %d of its own %d configs", ownClaimed, len(configsOf(own)))
	}

	// The real acting path, over a real window, with the claim set a batch of
	// device documents leaves behind — which after this step claims NONE of
	// the four-segment per-entity topics the window sees, so every one of the
	// sibling's twenty configs is unclaimed and the payload predicate is the
	// only thing standing between them and a retraction.
	for topic, body := range retained {
		br.retain(topic, string(body))
	}
	runArmedSweep(t, c, aClaimedDocument())
	if n := br.publishes(); n != 0 {
		t.Errorf("the sweep cleared %d of a sibling instance's %d configs", n, len(retained))
	}

	// A sibling on a DIFFERENT MQTT root: the same fleet with every topic
	// re-rooted, which is the two-ONECTA-accounts case the notes contemplate.
	otherRoot := map[string][]byte{}
	for topic, cfg := range sibling {
		moved := map[string]any{}
		for k, v := range cfg {
			if sv, ok := v.(string); ok && strings.HasPrefix(sv, config.TopicRoot+"/") {
				v = "klima/" + strings.TrimPrefix(sv, config.TopicRoot+"/")
			}
			moved[k] = v
		}
		b, err := json.Marshal(moved)
		if err != nil {
			t.Fatalf("re-encode %s: %v", topic, err)
		}
		otherRoot[topic] = b
	}
	br2 := newRetainedBroker()
	c2, _ := sweepCoordinator(t, br2)
	c2.deps.HASS.ClaimDevices([]string{
		"809d41d9-4d42-45fa-af6a-84b512143672",
		"11112222-3333-4444-5555-666677778888",
	})
	for topic, body := range otherRoot {
		br2.retain(topic, string(body))
	}
	runArmedSweep(t, c2, aClaimedDocument())
	if n := br2.publishes(); n != 0 {
		t.Errorf("the sweep cleared %d configs of a sibling on another root", n)
	}

	// And what the shipped predicate would have done with both inputs, so the
	// defect's size is recorded rather than described.
	sameRootCleared, otherRootCleared := 0, 0
	for _, body := range retained {
		if shippedIsOwnConfig(body, config.TopicRoot) {
			sameRootCleared++
		}
	}
	for _, body := range otherRoot {
		if shippedIsOwnConfig(body, config.TopicRoot) {
			otherRootCleared++
		}
	}
	// On a shared root the shipped rule claimed the sibling's WHOLE fleet.
	if sameRootCleared != len(retained) {
		t.Errorf("the shipped predicate would have cleared %d of the sibling's %d configs on a shared root, want all",
			sameRootCleared, len(retained))
	}
	// On a different root it still claimed the stateless ones, because the rule
	// collapses to the `daikin_` namespace when there is no state_topic to key
	// on. That is the half no configuration could avoid.
	if otherRootCleared != statelessSibling {
		t.Errorf("the shipped predicate would have cleared %d of the sibling's configs across roots, want its %d stateless ones",
			otherRootCleared, statelessSibling)
	}
	t.Logf("the shipped predicate would have retracted %d of a sibling's %d configs on a shared root and %d "+
		"(the climate and the button) across roots; the current one retracts 0 either way",
		sameRootCleared, len(retained), otherRootCleared)
}

// shippedIsOwnConfig is the predicate go-daikin2mqtt shipped up to and
// including 0.11.0, transcribed so the regression above can state what it would
// have done. It is unreachable from any production path.
func shippedIsOwnConfig(payload []byte, root string) bool {
	var cfg struct {
		UniqueID   string `json:"unique_id"`
		StateTopic string `json:"state_topic"`
	}
	if json.Unmarshal(payload, &cfg) != nil {
		return false
	}
	return strings.HasPrefix(cfg.UniqueID, hass.UniqueIDPrefix) &&
		(cfg.StateTopic == "" || strings.HasPrefix(cfg.StateTopic, root+"/"))
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
