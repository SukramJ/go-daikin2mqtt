// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
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
	c.deps.HASS.ClaimDevices([]string{"dev1", layout.SchedulerDeviceID})

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

// retainedBroker is an mqtt.Client that replays its retained messages to a new
// subscriber, the way a broker does — which is the only thing a sweep window
// reads.
type retainedBroker struct {
	mu       sync.Mutex
	messages map[string]string
	sent     int
	events   []string
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
	b.sent++
	if retain {
		b.messages[topic] = string(payload)
	}
	return nil
}

func (b *retainedBroker) publishes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent
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
	// The scheduler's reserved segment is claimed only when a scheduler is
	// attached — it is the one segment two instances can still collide on, and
	// claiming it unconditionally would widen that for nothing.
	c.AttachScheduler(&stubScheduler{doc: goldenScheduleDoc()})
	c.pollOnce(context.Background())
	if got := c.deps.HASS.ClaimedDevices(); !equalStrings(got, []string{dev, layout.SchedulerDeviceID}) {
		t.Errorf("with a scheduler attached the instance claims %v, want [%s %s]", got, dev, layout.SchedulerDeviceID)
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

// TestTheReconcileRunsTheReportOnlySweepAfterItsOwnWindow drives the real
// reconcile path end to end.
//
// Two things are asserted and both are about ORDER. The library's sweep really
// does run — a mutation that drops the call is otherwise invisible, because a
// report-only pass changes nothing an assertion on the broker could see. And it
// runs strictly AFTER the hand-rolled reconcile has unsubscribed: the two
// filters overlap (see TestSubscriptionFiltersCannotOverlap), so a broker with
// both installed would deliver every retained config to both handlers, and what
// keeps that from happening is the sequencing inside one gated goroutine.
func TestTheReconcileRunsTheReportOnlySweepAfterItsOwnWindow(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, _ := sweepCoordinator(t, br)
	c.deps.HASS.ClaimDevices([]string{"dev1"})
	br.retain("homeassistant/sensor/daikin_dev1_retired/config",
		`{"unique_id":"daikin_dev1_retired","state_topic":"daikin/dev1/climateControl/retired/state"}`)

	c.reconcileOrphans(context.Background(), map[string]bool{})
	// The reconcile runs asynchronously behind a try-locked gate; taking the
	// gate is what says it has finished.
	deadline := time.Now().Add(5 * time.Second)
	for !c.reconcileGate.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the reconcile never released its gate")
		}
		time.Sleep(time.Millisecond)
	}
	c.reconcileGate.Unlock()

	want := []string{
		"sub homeassistant/+/+/config",
		"unsub homeassistant/+/+/config",
		"sub homeassistant/#",
		"unsub homeassistant/#",
	}
	if got := br.log(); !equalStrings(got, want) {
		t.Errorf("subscription sequence %v, want %v — the two overlapping discovery windows must not be open together", got, want)
	}
}
