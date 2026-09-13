// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"

	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// F-A — a sibling instance's LIVE schedule switches were tombstoned and their
// per-entity configs retracted.
//
// The mechanism, in three constants none of which is derived from anything an
// operator sets:
//
//   - layout.SchedulerDeviceID is the literal "scheduler", so every instance
//     writes its schedule state under <root>/scheduler/…;
//   - hass.SchedulerNodeID is the literal "daikin_scheduler", so every instance
//     writes ONE device document at homeassistant/device/daikin_scheduler/config;
//   - claimedDeviceSegments used to add "scheduler" to every instance's claim
//     set, which made both of the above resolve as "ours".
//
// So BundleIsOwnConfig accepted the other instance's document (root matches,
// segment claimed, unique_ids in the daikin_ namespace, named > 0), its live
// switches became this instance's tombstone prior state and were marked
// removed, and the sweep then classed its per-entity configs as this instance's
// own unclaimed orphans and retracted them. In Home Assistant the entities are
// REMOVED FROM THE ENTITY REGISTRY: gone from dashboards and automations, area,
// rename and icon lost. The sibling restores them the next time its own
// discovery signature moves or it reconnects — and then does the same back. A
// permanent ping-pong.
//
// It needs no shared schedule id: the ids in these fixtures are disjoint.

const siblingSchedulerDoc = "homeassistant/device/" + hass.SchedulerNodeID + "/config"

// aSiblingsSchedulerFleet seeds a broker with another instance's live schedule
// switches — the document and the per-entity configs — with ids this instance
// does not have.
func aSiblingsSchedulerFleet(br *retainedBroker) []string {
	br.retain(siblingSchedulerDoc, `{
		"device":{"identifiers":["`+hass.SchedulerNodeID+`"],"name":"daikin2mqtt Scheduler"},
		"origin":{"name":"go-daikin2mqtt"},
		"components":{
		  "werktag":{"platform":"switch","unique_id":"daikin_schedule_werktag",
		    "state_topic":"daikin/scheduler/werktag/enabled/state",
		    "command_topic":"daikin/scheduler/werktag/enabled/set",
		    "availability_topic":"daikin/bridge/status"},
		  "nacht_leise":{"platform":"switch","unique_id":"daikin_schedule_nacht_leise",
		    "state_topic":"daikin/scheduler/nacht_leise/enabled/state",
		    "command_topic":"daikin/scheduler/nacht_leise/enabled/set",
		    "availability_topic":"daikin/bridge/status"}}}`)
	legacy := []string{
		"homeassistant/switch/daikin_schedule_werktag/config",
		"homeassistant/switch/daikin_schedule_nacht_leise/config",
	}
	for _, topic := range legacy {
		uid := "daikin_schedule_werktag"
		if topic != legacy[0] {
			uid = "daikin_schedule_nacht_leise"
		}
		br.retain(topic, `{"unique_id":"`+uid+`","platform":"switch",
			"state_topic":"daikin/scheduler/`+uid[len("daikin_schedule_"):]+`/enabled/state",
			"availability_topic":"daikin/bridge/status"}`)
	}
	return legacy
}

// waitForSweep blocks until the asynchronous sweep a poll started has released
// its gate. Taking the gate is what says it has finished.
func waitForSweep(t *testing.T, c *Coordinator) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !c.reconcileGate.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the sweep never released its gate")
		}
		time.Sleep(time.Millisecond)
	}
	c.reconcileGate.Unlock()
}

// TestASiblingsLiveScheduleSwitchesSurviveAMigration is F-A, driven through the
// real coordinator: PublishOnline plus a poll, over a broker pre-seeded with
// the other instance's retained document and per-entity configs.
//
// Both halves are asserted, because either alone passes over a half-fixed
// build: the sibling's components must not be tombstoned into this instance's
// document, and its per-entity configs must still be retained afterwards.
func TestASiblingsLiveScheduleSwitchesSurviveAMigration(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	legacy := aSiblingsSchedulerFleet(br)

	c := migrationCoordinator(t, br)
	// This instance's own scheduler, with entirely DISJOINT ids: the collision
	// is in the node id and the topic segment, not in any schedule id.
	own := schedule.NewDocument()
	own.Schedules = append(own.Schedules,
		schedule.Schedule{ID: "b_morgens", Name: "Morgens", Type: schedule.TypeIndoor})
	c.AttachScheduler(&stubScheduler{doc: own})

	ctx := context.Background()
	c.PublishOnline(ctx)
	c.pollOnce(ctx)
	waitForSweep(t, c)

	comps := componentsOfRetained(t, br, siblingSchedulerDoc)
	if _, ok := comps["b_morgens"]; !ok {
		t.Fatalf("this instance published no schedule switch of its own; the test is vacuous (components %v)",
			sortedComponentKeys(comps))
	}
	for _, key := range []string{"werktag", "nacht_leise"} {
		if _, ok := comps[key]; ok {
			t.Errorf("a sibling instance's live schedule switch %q was tombstoned into this instance's document; "+
				"Home Assistant removes it from the entity registry", key)
		}
	}
	for _, topic := range legacy {
		body, ok := br.retained(topic)
		if !ok || body == "" {
			t.Errorf("a sibling instance's live per-entity config was retracted: %s", topic)
		}
	}
}

// TestThePollClaimsNoSchedulerSegment pins the first of the two independent
// closures: the scheduler's reserved topic segment is not a claimed device
// segment, so both ownership predicates decline every topic under it.
//
// It replaces the half of TestAPollClaimsTheDevicesItResolved that asserted the
// opposite. Claiming it bought nothing an instance needs and cost a sibling its
// live switches.
func TestThePollClaimsNoSchedulerSegment(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	c.AttachScheduler(&stubScheduler{doc: goldenScheduleDoc()})
	c.pollOnce(context.Background())

	for _, seg := range c.deps.HASS.ClaimedDevices() {
		if seg == layout.SchedulerDeviceID {
			t.Fatalf("the instance claims the scheduler's reserved segment %q; every instance writes under it, "+
				"so a sibling's schedule switches resolve as this instance's own", seg)
		}
	}
	if got := c.deps.HASS.ClaimedDevices(); !equalStrings(got, []string{migrationDeviceID}) {
		t.Errorf("claimed %v, want only the device ids this poll resolved [%s]", got, migrationDeviceID)
	}
}

// TestTheTombstoneReadBackDeclinesTheSchedulerDocument pins the second closure,
// on its own — with the segment claimed on purpose, which is the state the
// first closure removes.
//
// Two closures for one hole is deliberate. The read-back is the step in front
// of the publish that cannot be undone, and the cost of a miss is a sibling's
// entities deleted from Home Assistant, so it does not rest on one predicate.
func TestTheTombstoneReadBackDeclinesTheSchedulerDocument(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	aSiblingsSchedulerFleet(br)
	c := migrationCoordinator(t, br)
	// The state the other closure removes, restored by hand: even with the
	// segment claimed, the document must not become prior state.
	c.deps.HASS.ClaimDevices([]string{migrationDeviceID, layout.SchedulerDeviceID})

	c.loadPriorComponents(context.Background(), c.ha())

	c.mu.Lock()
	prior := c.priorComponents
	c.mu.Unlock()
	if got, ok := prior[hass.SchedulerNodeID]; ok {
		t.Errorf("the scheduler document became prior state (%d components); its node id is a compile-time "+
			"literal, so the document read back may be any instance's", len(got))
	}
}

// TestTheSchedulerNodeIDIsTheOneTheRendererProduces keeps the constant the
// read-back declines and the node id the renderer writes one string.
//
// Two spellings that drift make the guard above decline a topic nothing
// publishes while the document it exists to decline sails past.
func TestTheSchedulerNodeIDIsTheOneTheRendererProduces(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	bundles, err := c.deps.HASS.RenderScheduleBundles(
		config.DefaultHASSBaseTopic, version.Version,
		[]hass.ScheduleInfo{{ID: "werktag", Name: "Werktag"}}, "",
	)
	if err != nil || len(bundles) != 1 {
		t.Fatalf("render: %v (%d bundles)", err, len(bundles))
	}
	if got := bundles[0].Bundle.NodeID; got != hass.SchedulerNodeID {
		t.Errorf("the renderer writes node id %q, the read-back declines %q", got, hass.SchedulerNodeID)
	}
}

// TestASiblingsScheduleConfigIsNeverSwept is the sweep half of F-A, isolated
// from the tombstone half so a regression in either is attributable.
//
// The sweep's verdict is taken on the payload, and the payload of a schedule
// switch names only <root>/scheduler/… topics plus the bridge-level
// availability topic — which F18 deliberately excludes because it is not
// instance-specific. With the scheduler segment claimed, what remained was the
// daikin_ namespace, which every installation shares: exactly F18's shape, for
// the last two configs it did not cover.
func TestASiblingsScheduleConfigIsNeverSwept(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, rt := sweepCoordinator(t, br)
	defer rt.Close()
	c.deps.HASS.ClaimDevices([]string{"dev1"})

	sibling := aSiblingsSchedulerFleet(br)
	fan, res := c.sweepReport(context.Background(), rt, map[string]bool{})

	if len(fan.Orphan) != 0 {
		t.Fatalf("the sweep would retract %d of a sibling's live schedule configs: %v", len(fan.Orphan), fan.Orphan)
	}
	if fan.Sibling != len(sibling) {
		t.Errorf("recognised %d sibling schedule configs, want %d", fan.Sibling, len(sibling))
	}
	// Not vacuous: the window really delivered them and the TOPIC predicate
	// really owned them. Only the payload predicate declined them.
	if res.Inspected != len(sibling) {
		t.Errorf("inspected %d, want %d — the window did not see the sibling's schedule configs",
			res.Inspected, len(sibling))
	}
}

// TestTwoInstancesOnOneAccountStillOverwriteEachOther is F14 proper, pinned
// rather than fixed — and the qualified half of the read-back's safety claim.
//
// Two instances on the SAME ONECTA account with different LOCAL_MODE or
// characteristics.yaml resolve the same device ids and claim them both
// legitimately. Every ownership predicate this bridge has says "ours" about
// both, correctly, so the instance with the smaller component set reads the
// larger one's document back, diffs it, and tombstones components the other
// instance is really publishing.
//
// This is NOT F-A. F-A was a false claim over a compile-time literal and is
// closed. This is a true claim over a genuinely shared device, and it is
// indistinguishable from the case the tombstone exists for: a component that
// disappears because LOCAL_MODE was turned off looks exactly the same whether
// the operator turned it off here or never turned it on in a sibling. Only an
// instance identifier separates them — ADR 0070 step 3(c), still open.
//
// So it is asserted as it behaves, loudly, so that a future instance identifier
// has a test to flip rather than a surprise to discover. changelog.md and
// README.md say the same thing to operators: one instance per ONECTA account
// per MQTT root.
func TestTwoInstancesOnOneAccountStillOverwriteEachOther(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	ctx := context.Background()
	const victim = "room_temperature"

	// Instance A: the full catalogue.
	a := migrationCoordinatorWith(t, br, loadTestCatalog(t))
	a.PublishOnline(ctx)
	a.pollOnce(ctx)
	if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; !ok {
		t.Fatalf("instance A published no %q; the test is vacuous", victim)
	}

	// Instance B: the same account, the same devices, a smaller component set.
	b := migrationCoordinatorWith(t, br, catalogWithout(t, victim))
	b.PublishOnline(ctx)
	b.pollOnce(ctx)

	if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; !ok {
		t.Skip("two instances on one account no longer collide — an instance identifier has landed; " +
			"update this test and the operator note in changelog.md")
	}
	t.Logf("KNOWN AND DOCUMENTED (F14): a second instance on the same ONECTA account with a smaller "+
		"component set tombstones %q, which the first instance really publishes. No predicate can "+
		"separate the two; an instance identifier (step 3(c)) can.", victim)
}

// TestADeletedScheduleIsStillRemovedWithinOneProcess is the half of F-A's fix
// that must NOT have been given up.
//
// Not claiming the scheduler segment costs the broker read-back and the sweep
// their view of schedule switches. The in-process memo keeps its view, and that
// is the one that matters in practice: a schedule is deleted in the daemon's
// own web UI, with the daemon running. What is genuinely lost — and is written
// down in changelog.md, README.md and addon/DOCS.md — is a schedule deleted
// while the daemon is STOPPED, which leaves a phantom switch.
func TestADeletedScheduleIsStillRemovedWithinOneProcess(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()

	both := schedule.NewDocument()
	both.Schedules = append(both.Schedules,
		schedule.Schedule{ID: "werktag", Name: "Werktag", Type: schedule.TypeIndoor},
		schedule.Schedule{ID: "urlaub", Name: "Urlaub", Type: schedule.TypeIndoor})
	sched := &stubScheduler{doc: both}
	c.AttachScheduler(sched)
	c.PublishOnline(ctx)
	c.pollOnce(ctx)
	if _, ok := componentsOfRetained(t, br, siblingSchedulerDoc)["urlaub"]; !ok {
		t.Fatal("the first poll published no switch for the schedule about to be deleted")
	}

	// The operator deletes it in the web UI, on the same connection.
	one := schedule.NewDocument()
	one.Schedules = append(one.Schedules,
		schedule.Schedule{ID: "werktag", Name: "Werktag", Type: schedule.TypeIndoor})
	sched.doc = one
	c.pollOnce(ctx)

	entry, present := componentsOfRetained(t, br, siblingSchedulerDoc)["urlaub"]
	if !present {
		t.Fatal("the deleted schedule's switch was omitted rather than tombstoned; " +
			"Home Assistant keeps the entity and it reads available")
	}
	body, ok := entry.(map[string]any)
	if !ok || len(body) != 1 || body["platform"] == nil {
		t.Errorf("the tombstone for the deleted schedule is %v; a platform-only entry is what removes an entity", entry)
	}
	if got, ok := br.retained("homeassistant/switch/daikin_schedule_urlaub/config"); ok && got != "" {
		t.Errorf("the deleted schedule's per-entity config was left retained: %q", got)
	}
}
