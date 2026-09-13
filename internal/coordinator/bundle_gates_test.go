// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// The two gates in front of the one publish that cannot be undone, and the
// margin they are measured against.
//
// publisher.Runtime.PublishBundle retracts every per-entity config a document
// carries BEFORE it writes the document, so anything that can refuse a document
// has to refuse it before that call — and has to judge the document that will
// actually go on the wire.

// measureDocuments runs one coordinator against a throwaway broker and returns
// the size each device document reached — payload plus topic, the two halves
// bundleFits adds its margin to.
//
// A measured limit rather than a literal, because the fixture's rendered size
// is not a number this test should own: a catalogue edit would otherwise turn a
// gate assertion into a stale constant that passes for the wrong reason.
func measureDocuments(t *testing.T, withScheduler bool, victim ...string) map[string]int {
	t.Helper()
	br := newRetainedBroker()
	cat := loadTestCatalog(t)
	if len(victim) > 0 {
		cat = catalogWithout(t, victim...)
	}
	c := migrationCoordinatorWith(t, br, cat)
	if withScheduler {
		c.AttachScheduler(&stubScheduler{doc: goldenScheduleDoc()})
	}
	c.PublishOnline(context.Background())
	c.pollOnce(context.Background())

	out := map[string]int{}
	for _, topic := range br.publishOrder() {
		if !strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/"+publisher.BundleSegment+"/") {
			continue
		}
		body, ok := br.retained(topic)
		if !ok || body == "" {
			continue
		}
		out[topic] = len(body) + len(topic)
	}
	if len(out) == 0 {
		t.Fatal("the measuring run published no device document")
	}
	return out
}

// TestATombstonedDocumentIsGatedInTheShapeItIsPublished is F-C.
//
// The order used to be buildBundles (validate, fit) -> loadPriorComponents ->
// tombstone -> publishBundles, so both gates judged a document the daemon never
// published: [hass.ApplyTombstones] ADDS a platform-only entry per removed
// component, measured at +42 bytes each. A document that passed the preflight
// by less than that had its per-entity configs retracted and then failed the
// PUBLISH — the exact irreversible failure bundleFits exists to prevent, with
// the gate in place.
//
// This drives the real removal (a characteristics.yaml edit across a restart)
// with the broker's advertised maximum set to EXACTLY what the untombstoned
// document needs, and requires the batch to be withheld. The positive control
// beside it — the same run with room for the tombstone — is what stops this
// passing because the fixture never fits at all.
func TestATombstonedDocumentIsGatedInTheShapeItIsPublished(t *testing.T) {
	t.Parallel()
	const victim = "room_temperature"
	// What the second boot's document measures with NO prior state, i.e.
	// without the tombstone the real run adds.
	var untombstoned int
	for _, size := range measureDocuments(t, false, victim) {
		if size > untombstoned {
			untombstoned = size
		}
	}

	for name, limit := range map[string]int{
		"exactly the untombstoned document": untombstoned + bundlePublishOverhead,
		"room for the tombstone":            untombstoned + bundlePublishOverhead + 1024,
	} {
		wantPublished := strings.HasPrefix(name, "room")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			br := newRetainedBroker()
			ctx := context.Background()

			first := migrationCoordinatorWith(t, br, loadTestCatalog(t))
			first.PublishOnline(ctx)
			first.pollOnce(ctx)
			if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; !ok {
				t.Fatalf("the first boot published no %q; the removal cannot be driven", victim)
			}
			mark := len(br.publishOrder())

			second := migrationCoordinatorWith(t, br, catalogWithout(t, victim))
			second.deps.BrokerMaxPacketSize = func() (uint32, bool) { return uint32(limit), true } //nolint:gosec // a test-sized limit
			second.PublishOnline(ctx)
			second.pollOnce(ctx)

			got := documentsIn(br.publishOrder()[mark:]) > 0
			if got != wantPublished {
				t.Errorf("with the broker maximum at %d the second boot published=%v, want %v — "+
					"the gates run on a document the daemon does not publish", limit, got, wantPublished)
			}
			if wantPublished {
				return
			}
			// Withheld means withheld completely: the previous document is
			// still there and the device's entities still work.
			if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; !ok {
				t.Error("the withheld batch still changed the retained document")
			}
			for _, topic := range br.publishOrder()[mark:] {
				if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/") {
					t.Errorf("the withheld batch retracted %s", topic)
				}
			}
		})
	}
}

// TestTheOversizeMarginCoversTheMQTTPacketHeader is F-D.
//
// bundlePublishOverhead was READ by a test and never CHECKED: mutating it to 0
// survived. At 0 a document whose payload plus topic exactly equals the
// broker's advertised maximum passes the preflight, has its per-entity configs
// retracted, and then fails the PUBLISH on the fixed header, the
// remaining-length varint and the topic-length prefix — five to eight bytes
// that are not in the document and are on the wire.
//
// So the margin is asserted where it acts: a document offered a limit equal to
// its own size must be REFUSED, because the packet it becomes is bigger than
// the document it carries.
func TestTheOversizeMarginCoversTheMQTTPacketHeader(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)

	in := buildHamqttInputs(t, surfaceScenarios()[0])
	bundles, err := in.disc.RenderBundles(
		config.DefaultHASSBaseTopic, version.Version, in.points, in.infos, in.climateInfos,
	)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("render: %v", err)
	}
	b := bundles[0]
	payload, err := json.Marshal(b.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	exact := len(payload) + len(b.Topic)

	c.deps.BrokerMaxPacketSize = func() (uint32, bool) { return uint32(exact), true } //nolint:gosec // a test-sized limit
	if c.bundleFits(b) {
		t.Errorf("a %d-byte document was accepted against a %d-byte maximum; the PUBLISH packet adds a fixed "+
			"header, a remaining-length varint and a topic-length prefix on top of it, and fails AFTER "+
			"the retraction", exact, exact)
	}
	// The margin must still be a margin and not a wall: one byte of room below
	// the limit has to publish, or the guard withholds working migrations.
	c.deps.BrokerMaxPacketSize = func() (uint32, bool) { return uint32(exact + bundlePublishOverhead), true } //nolint:gosec // a test-sized limit
	if !c.bundleFits(b) {
		t.Error("a document that fits with the full margin was refused")
	}
	// The derivation, stated independently of the constant: fixed header (1) +
	// remaining-length varint (up to 4) + topic-length prefix (2) + an empty v5
	// property block (1). Anything below this cannot cover the packet.
	const minimumPacketOverhead = 1 + 4 + 2 + 1
	if bundlePublishOverhead < minimumPacketOverhead {
		t.Errorf("bundlePublishOverhead = %d, below the %d bytes an MQTT PUBLISH adds to its payload",
			bundlePublishOverhead, minimumPacketOverhead)
	}
}

// TestAnOversizedDocumentWithholdsTheWholeBatch is F-E.
//
// The doc comment used to say "one withheld device withholds the SWEEP for the
// whole batch, not just itself", which describes a design where the other
// documents still publish. The code does something else and safer:
// buildBundles reports the batch as not-OK and maybePublishDiscovery returns
// before publishing anything at all. The old test could not tell the two apart,
// because its fixture had one device.
//
// This one has two — the ONECTA device and the daemon's own scheduler — and
// sets the broker's maximum so that the scheduler's document fits comfortably
// and the device's does not. A "withhold that device" design publishes the
// scheduler; this one publishes nothing.
func TestAnOversizedDocumentWithholdsTheWholeBatch(t *testing.T) {
	t.Parallel()
	sizes := measureDocuments(t, true)
	if len(sizes) < 2 {
		t.Fatalf("the fixture rendered %d documents; this test needs a big one and a small one", len(sizes))
	}
	schedulerTopic := config.DefaultHASSBaseTopic + "/" + publisher.BundleSegment + "/" + hass.SchedulerNodeID + "/config"
	small, ok := sizes[schedulerTopic]
	if !ok {
		t.Fatalf("no scheduler document among %v", sizes)
	}
	big := sizes[migrationBundleTop]
	if big <= small {
		t.Fatalf("the device document (%d) is not bigger than the scheduler's (%d)", big, small)
	}
	// Comfortably above the small document, comfortably below the big one.
	limit := small + bundlePublishOverhead + (big-small)/2

	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	c.AttachScheduler(&stubScheduler{doc: goldenScheduleDoc()})
	c.deps.BrokerMaxPacketSize = func() (uint32, bool) { return uint32(limit), true } //nolint:gosec // a test-sized limit
	c.PublishOnline(context.Background())
	c.pollOnce(context.Background())

	for _, topic := range br.publishOrder() {
		if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/") {
			t.Errorf("the batch published %s although one of its documents was refused; "+
				"a half-migrated fleet is the one state with no owner", topic)
		}
	}
	c.mu.Lock()
	sig := c.lastDiscSig
	c.mu.Unlock()
	if sig != "" {
		t.Error("a withheld batch committed its signature; the next poll will not retry")
	}
}

// TestTheClaimedBranchIsReachedByARealMigration is F-B.
//
// sweepReport looks every inspected config up by its four-segment PER-ENTITY
// topic (publisher.LegacyTopicByUniqueID). The claim set used to be keyed by
// DEVICE-DOCUMENT topics — from publishBundles and from Runtime.Declared(),
// both of which hold document topics after step 6 — so `case claimed[topic]`
// could not match any key the set contained. `Claimed` was structurally 0, and
// the sentence that arms the sweep ("the claim set is exactly the batch just
// published plus what the runtime still declares") was false of the batch half.
//
// The set is now seeded from publisher.SupersededTopics, which renders exactly
// the per-entity topics a document replaces — the tombstoned components'
// included, whose unique_id it takes from Bundle.Tombstones.
//
// The state driven here is one a successful batch normally leaves behind in
// nobody's broker: discovery is published at QoS 0, so a retraction that "went
// out" is a statement about one socket write, and a broker that never applied
// it still holds the config while the document landed. That is the case the
// claim set is the backstop for, and it is the reason it has to name the topics
// the sweep asks about.
func TestTheClaimedBranchIsReachedByARealMigration(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c, rt := sweepCoordinator(t, br)
	defer rt.Close()
	c.deps.HASS.ClaimDevices([]string{migrationDeviceID})

	in := buildHamqttInputs(t, surfaceScenarios()[0])
	bundles, err := in.disc.RenderBundles(
		config.DefaultHASSBaseTopic, version.Version, in.points, in.infos, in.climateInfos,
	)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("render: %v", err)
	}
	claimed, allSent := c.publishBundles(context.Background(), rt, bundles)
	if !allSent {
		t.Fatal("the batch did not reach the socket")
	}

	// Every key must be a topic the sweep can ask about, and at least one must
	// be the per-entity form — a set of document topics alone is the defect.
	var perEntity []string
	for topic := range claimed {
		if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/"+publisher.BundleSegment+"/") {
			continue
		}
		perEntity = append(perEntity, topic)
	}
	if len(perEntity) == 0 {
		t.Fatal("the claim set holds only device-document topics, which the sweep never looks up")
	}

	// The QoS 0 case: the retraction did not reach the broker, so the config is
	// still retained when the window opens.
	victim := perEntity[0]
	for _, topic := range perEntity {
		if topic < victim {
			victim = topic
		}
	}
	uid := strings.Split(victim, "/")[2]
	br.retain(victim, `{"unique_id":"`+uid+`","state_topic":"daikin/`+migrationDeviceID+`/climateControl/x/state"}`)

	fan, res := c.sweepReport(context.Background(), rt, claimed)
	if res.Inspected == 0 {
		t.Fatal("the window saw nothing; the verdict is vacuous")
	}
	if fan.Claimed == 0 {
		t.Errorf("the claimed branch was never reached over a real batch's claim set (%+v); "+
			"the set does not name the topics the sweep looks up", fan)
	}
	for _, orphan := range fan.Orphan {
		if orphan == victim {
			t.Errorf("%s was judged an orphan although this batch claims it; the sweep would delete a "+
				"config this daemon is publishing", victim)
		}
	}
}
