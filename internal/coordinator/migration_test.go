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

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// ADR 0070 phase 8 step 6 — the migration itself, driven end to end.
//
// Everything here is about the ONE ordering the migration cannot get wrong: a
// per-entity config retained for a unique_id and a device document carrying the
// same id cannot coexist, so every retraction must be complete before the
// document lands. Home Assistant's refusal is one WARNING line in its own log
// and no entities; nothing on the wire says why.

const (
	migrationDeviceID  = "dev1"
	migrationEmbedded  = "climateControl"
	migrationBundleTop = "homeassistant/device/daikin_dev1/config"
)

// migrationCoordinator builds a coordinator whose discovery plane really
// publishes, over a broker that replays its retained store the way a real one
// does.
func migrationCoordinator(t *testing.T, br *retainedBroker) *Coordinator {
	t.Helper()
	return migrationCoordinatorWith(t, br, loadTestCatalog(t))
}

// migrationCoordinatorWith is migrationCoordinator over a stated catalogue, so
// a test can boot the same daemon twice with an entry removed in between —
// which is what a characteristics.yaml edit shipped with a release does.
func migrationCoordinatorWith(t *testing.T, br *retainedBroker, cat *catalog.Catalog) *Coordinator {
	t.Helper()
	cfg := testConfig()
	cfg.HASSBaseTopic = config.DefaultHASSBaseTopic
	c := New(Deps{
		Cfg:     cfg,
		Client:  &stubCloud{devices: devicesJSON(migrationDeviceID, migrationEmbedded)},
		MQTT:    br,
		Catalog: cat,
		HASS:    hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, br),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   fixedClock(),
	})
	c.collectWindow = 20 * time.Millisecond
	return c
}

// retractionsBefore splits a publish order at the device document and returns
// the per-entity config topics that preceded it, plus whether the document was
// published at all.
func retractionsBefore(order []string, document string) (before []string, published bool) {
	for _, topic := range order {
		if topic == document {
			return before, true
		}
		if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/") && strings.HasSuffix(topic, "/config") {
			before = append(before, topic)
		}
	}
	return before, false
}

// TestAReconnectReSendsTheRetractionsBeforeTheDocument is the defect
// go-mtec2mqtt shipped, proved by its reviewer, and the reason step 5 built a
// runtime FACTORY rather than holding an instance.
//
// publisher.Runtime remembers what it has superseded, and that memo is a
// statement about A BROKER made PER PROCESS. At QoS 0 a successful publish is a
// statement about ONE CONNECTION: Publish returns when go-mqtt's writeFrame
// returned, which says the bytes went to a socket, not that a broker applied
// them. So a retraction written to a dying socket "succeeds", the memo records
// it done, and after the reconnect the retry sends ZERO retractions and
// publishes the document anyway — into a live conflict. Measured there:
// retractions re-sent 0, document published true, configs still retained 1.
//
// This test drives a RECONNECT, not a process restart, and that distinction is
// the whole point: mtec's own test drove a restart, which is why the gap
// survived a release. A restart rebuilds everything and cannot fail; only a
// reconnect exercises the memo actually outliving the connection it describes.
//
// Both halves are asserted, because either alone is a passing test of a broken
// migration: every retraction goes out AGAIN, and every one of them precedes
// the document.
func TestAReconnectReSendsTheRetractionsBeforeTheDocument(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()

	c.PublishOnline(ctx)
	c.pollOnce(ctx)
	first, ok := retractionsBefore(br.publishOrder(), migrationBundleTop)
	if !ok {
		t.Fatalf("no device document was published; order was %v", br.publishOrder())
	}
	if len(first) == 0 {
		t.Fatal("the first publish retracted nothing — the legacy topic form is not stated")
	}

	// The reconnect. PublishOnline is what the MQTT lifecycle's OnConnect
	// calls, and it is the only thing that happens between the two batches:
	// the daemon does not know the broker lost its retained store, and must
	// not need to.
	before := len(br.publishOrder())
	c.PublishOnline(ctx)
	c.pollOnce(ctx)

	second, ok := retractionsBefore(br.publishOrder()[before:], migrationBundleTop)
	if !ok {
		t.Fatalf("the reconnect published no device document; order was %v", br.publishOrder()[before:])
	}
	if !equalStrings(sortedCopy(first), sortedCopy(second)) {
		t.Errorf("the reconnect re-sent %d of %d retractions before the document\n  first  %v\n  second %v",
			len(second), len(first), sortedCopy(first), sortedCopy(second))
	}
}

// TestTheDiscoveryGateReopensOnEveryConnect is F16, which step 5 named and
// explicitly left for step 6.
//
// lastDiscSig is a statement about what a BROKER holds, and a broker restarted
// without a persistent retained store holds nothing. A gate that outlived the
// connection it was computed on meant the whole fleet never came back until the
// entity set happened to change — which, on a stable installation, is never.
//
// The state plane's equivalent (StatePublisher.Reset) was fixed at step 5; this
// is the discovery plane's, and it could not be done there because clearing it
// puts traffic on the wire on every reconnect, which is the one thing step 5
// claimed did not happen.
func TestTheDiscoveryGateReopensOnEveryConnect(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()

	c.PublishOnline(ctx)
	c.pollOnce(ctx)
	c.mu.Lock()
	sig := c.lastDiscSig
	c.mu.Unlock()
	if sig == "" {
		t.Fatal("the first poll committed no signature")
	}

	// A second poll on the same connection publishes nothing: the gate is the
	// gate, and this step does not make the daemon chattier.
	before := len(br.publishOrder())
	c.pollOnce(ctx)
	if n := len(br.publishOrder()) - before; documentsIn(br.publishOrder()[before:]) != 0 {
		t.Errorf("an unchanged poll published %d device documents (%d publishes)",
			documentsIn(br.publishOrder()[before:]), n)
	}

	// The reconnect re-opens it.
	c.PublishOnline(ctx)
	c.mu.Lock()
	sig = c.lastDiscSig
	c.mu.Unlock()
	if sig != "" {
		t.Errorf("lastDiscSig survived the reconnect: %q", sig)
	}
	before = len(br.publishOrder())
	c.pollOnce(ctx)
	if documentsIn(br.publishOrder()[before:]) == 0 {
		t.Error("the reconnect republished no device document; a broker without its retained store stays empty")
	}
}

// documentsIn counts device-document publishes in a publish order.
func documentsIn(order []string) int {
	n := 0
	for _, topic := range order {
		if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/"+publisher.BundleSegment+"/") {
			n++
		}
	}
	return n
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestTheCrashWindowHealsOnTheNextBoot drives a process into the one state the
// migration can leave behind: the retractions applied, the document refused.
//
// The broker then holds NO discovery config for the device at all — the
// entities are absent, not unavailable — and nothing the dead process knew
// survives to say so.
//
// It heals because the daemon remembers nothing across boots: publisher.Runtime
// keeps `superseded` and `declared` in memory with no session store, so a fresh
// process re-sends every retraction (a no-op against topics already cleared)
// AND publishes the document, because its dedup gate has no record of it. Both
// halves are asserted: a boot that trusted a previous process's retraction is a
// boot that publishes into a conflict.
func TestTheCrashWindowHealsOnTheNextBoot(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	ctx := context.Background()

	// Boot one: the document is refused, the retractions are not.
	first := migrationCoordinator(t, br)
	br.refuse(config.DefaultHASSBaseTopic + "/" + publisher.BundleSegment + "/")
	first.PublishOnline(ctx)
	first.pollOnce(ctx)
	retracted, published := retractionsBefore(br.publishOrder(), migrationBundleTop)
	if published {
		t.Fatal("the document was published; the crash window was not entered")
	}
	if len(retracted) == 0 {
		t.Fatal("nothing was retracted; the crash window was not entered")
	}
	if _, ok := br.retained(migrationBundleTop); ok {
		t.Fatal("the broker holds a document it refused")
	}

	// Boot two: a NEW coordinator over a NEW runtime, which is what makes the
	// fresh memo real rather than simulated. The broker is back.
	br.refuse("")
	mark := len(br.publishOrder())
	second := migrationCoordinator(t, br)
	second.PublishOnline(ctx)
	second.pollOnce(ctx)

	again, ok := retractionsBefore(br.publishOrder()[mark:], migrationBundleTop)
	if !ok {
		t.Fatal("the next boot published no document")
	}
	if !equalStrings(sortedCopy(retracted), sortedCopy(again)) {
		t.Errorf("the next boot re-sent %d of %d retractions\n  first %v\n  again %v",
			len(again), len(retracted), sortedCopy(retracted), sortedCopy(again))
	}
}

// TestAnOversizedDocumentIsWithheldBeforeAnythingIsRetracted is the preflight.
//
// go-mqtt refuses an oversized packet from its own write path — by which time
// publisher.Runtime.PublishBundle has already superseded every per-entity
// config the document carries, leaving the device with no discovery config at
// all. So the check has to happen before the call, and its failure has to be
// total for that device: no document AND no retraction.
func TestAnOversizedDocumentIsWithheldBeforeAnythingIsRetracted(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	// Smaller than any document this bridge renders, and stated as "known".
	c.deps.BrokerMaxPacketSize = func() (uint32, bool) { return 512, true }

	c.PublishOnline(ctx0())
	c.pollOnce(ctx0())

	for _, topic := range br.publishOrder() {
		if strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/") {
			t.Errorf("the discovery plane published %s despite the preflight", topic)
		}
	}
	c.mu.Lock()
	sig := c.lastDiscSig
	c.mu.Unlock()
	if sig != "" {
		t.Error("a withheld batch committed its signature; the next poll will not retry")
	}
}

// TestAnUnknownPacketLimitIsNotTreatedAsASmallOne pins the other direction,
// which go-mtec2mqtt's notes got wrong by conflating go-mqtt's INBOUND 1 MiB
// cap with the broker's advertised OUTBOUND one.
//
// MQTT 5.0's meaning of an absent Maximum Packet Size property IS "no limit",
// and an MQTT 3.1.1 broker advertises nothing at all. Refusing on unknown would
// withhold the migration from every such installation for nothing, so unknown
// is unknown: the guard can only ever prevent a publish that would genuinely
// have failed.
func TestAnUnknownPacketLimitIsNotTreatedAsASmallOne(t *testing.T) {
	t.Parallel()
	for name, hook := range map[string]func() (uint32, bool){
		"no connect yet":      func() (uint32, bool) { return 0, false },
		"broker set no limit": func() (uint32, bool) { return 0, true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			br := newRetainedBroker()
			c := migrationCoordinator(t, br)
			c.deps.BrokerMaxPacketSize = hook
			c.PublishOnline(ctx0())
			c.pollOnce(ctx0())
			if documentsIn(br.publishOrder()) == 0 {
				t.Error("an unknown limit withheld the migration")
			}
		})
	}
}

// TestARemovedComponentIsTombstonedRatherThanOmitted is the capability
// go-mtec2mqtt lost when it deferred this, and the reason this bridge does not.
//
// An omitted component is NOT removed from Home Assistant. The entity stays in
// the registry and — because this bridge's availability is bridge-level and the
// bridge is online — reads AVAILABLE, so nothing on screen says it is dead.
// Under the per-entity form the orphan sweep removed it; on a document the
// sweep cannot, because its ownership predicate declines device documents on
// purpose.
//
// Three things remove a component from a device that is otherwise unchanged
// here, and all three are ordinary operator actions: deleting a weekly
// schedule, turning LOCAL_MODE off, and a characteristics.yaml edit shipped
// with a release. This drives the third, across a RESTART — the case an
// in-process memo cannot see and the reason the previous document is read back
// from the broker before anything is published.
func TestARemovedComponentIsTombstonedRatherThanOmitted(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	ctx := context.Background()

	first := migrationCoordinatorWith(t, br, loadTestCatalog(t))
	first.PublishOnline(ctx)
	first.pollOnce(ctx)
	before := componentsOfRetained(t, br, migrationBundleTop)
	const victim = "room_temperature"
	if _, ok := before[victim]; !ok {
		t.Fatalf("the first document does not carry %q; it carries %v", victim, sortedComponentKeys(before))
	}

	// The catalogue entry is gone, and the daemon restarts. The new document
	// simply never renders that component — which is exactly why
	// Bundle.Remove cannot help and only the PREVIOUS document can say what
	// was there.
	second := migrationCoordinatorWith(t, br, catalogWithout(t, victim))
	second.PublishOnline(ctx)
	second.pollOnce(ctx)

	after := componentsOfRetained(t, br, migrationBundleTop)
	entry, present := after[victim]
	if !present {
		t.Fatalf("%q was omitted rather than tombstoned; Home Assistant keeps the entity and it reads available", victim)
	}
	body, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", victim, entry)
	}
	if _, hasPlatform := body["platform"]; !hasPlatform {
		t.Errorf("the tombstone for %q carries no platform; an empty object is ignored by Home Assistant", victim)
	}
	if len(body) != 1 {
		t.Errorf("the tombstone for %q carries %v; a platform-only entry is what removes an entity", victim, body)
	}
	// The unique_id must NOT be in the payload — putting it back un-removes the
	// entity the tombstone exists to finish removing. It is remembered outside
	// the payload instead, which is what lets the removed entity's retained
	// per-entity config be retracted too.
	if _, hasUID := body["unique_id"]; hasUID {
		t.Errorf("the tombstone for %q carries a unique_id, which un-removes the entity", victim)
	}
	if got, ok := br.retained("homeassistant/sensor/daikin_dev1_" + victim + "/config"); ok && got != "" {
		t.Errorf("the removed entity's per-entity config was left retained: %q", got)
	}
}

// TestATombstoneIsNotRepeatedForever pins the other half: a component marked
// removed must not be re-marked on every publish after that, or a document
// accumulates dead keys for the life of the installation.
func TestATombstoneIsNotRepeatedForever(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	ctx := context.Background()
	const victim = "room_temperature"

	first := migrationCoordinatorWith(t, br, loadTestCatalog(t))
	first.PublishOnline(ctx)
	first.pollOnce(ctx)

	second := migrationCoordinatorWith(t, br, catalogWithout(t, victim))
	second.PublishOnline(ctx)
	second.pollOnce(ctx)
	if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; !ok {
		t.Fatal("the removal was not tombstoned; this test would pass vacuously")
	}

	third := migrationCoordinatorWith(t, br, catalogWithout(t, victim))
	third.PublishOnline(ctx)
	third.pollOnce(ctx)
	if _, ok := componentsOfRetained(t, br, migrationBundleTop)[victim]; ok {
		t.Errorf("%q is still tombstoned a boot later; the mark is being re-derived from a tombstone", victim)
	}
}

// catalogWithout is the shipped test catalogue with one topic's entry removed.
func catalogWithout(t *testing.T, topics ...string) *catalog.Catalog {
	t.Helper()
	var kept []string
	for _, line := range strings.Split(testCatalogYAML, "\n- ") {
		drop := false
		for _, topic := range topics {
			if strings.Contains(line, "topic: "+topic+"\n") || strings.Contains(line, "topic: "+topic+",") {
				drop = true
			}
		}
		if drop {
			continue
		}
		kept = append(kept, line)
	}
	cat, err := catalog.Load(strings.NewReader(strings.Join(kept, "\n- ")))
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	return cat
}

// sortedComponentKeys is for a failure message.
func sortedComponentKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestASiblingsDeviceDocumentIsNeverMistakenForOurs pins the bundle half of
// F14's payload predicate, in both directions.
//
// A device document carries no top-level unique_id and no top-level `*_topic`
// keys — they sit one level down, inside each component — so the per-entity
// predicate declines EVERY document. That is safe but it is not an answer, and
// the tombstone read-back needs one: a document whose component set came from a
// sibling instance would tombstone entities this instance never published.
//
// The rule is the per-entity rule applied component by component: every topic
// key of every component, bar the bridge-level availability_topic, under this
// instance's own root AND a device it polls.
func TestASiblingsDeviceDocumentIsNeverMistakenForOurs(t *testing.T) {
	t.Parallel()
	d := hass.New(config.DefaultHASSBaseTopic, config.TopicRoot, "en", nil)

	ours := `{"device":{"identifiers":["daikin_dev1"]},"origin":{"name":"go-daikin2mqtt"},"components":{
		"power":{"platform":"switch","unique_id":"daikin_dev1_power",
		 "state_topic":"daikin/dev1/climateControl/power/state",
		 "availability_topic":"daikin/bridge/status"},
		"refresh":{"platform":"button","unique_id":"daikin_dev1_refresh",
		 "command_topic":"daikin/dev1/climateControl/refresh/set",
		 "availability_topic":"daikin/bridge/status"}}}`
	// A sibling instance: same namespace, same discovery prefix, same MQTT
	// root, a device this instance does not poll.
	sibling := strings.ReplaceAll(ours, "dev1", "dev2")
	// A sibling on another root, and the stateless-component case that F18 was
	// about: a document whose only topic keys are the bridge-level ones.
	onlyBridge := `{"device":{"identifiers":["daikin_dev1"]},"origin":{"name":"go-daikin2mqtt"},"components":{
		"climate":{"platform":"climate","unique_id":"daikin_dev1_climate",
		 "availability_topic":"daikin/bridge/status"}}}`

	if d.BundleIsOwnConfig([]byte(ours)) {
		t.Error("claimed before the first poll; ownership that cannot be proven must not be claimed")
	}
	d.ClaimDevices([]string{"dev1"})

	for name, tc := range map[string]struct {
		body []byte
		want bool
	}{
		"ours":                     {[]byte(ours), true},
		"a sibling's device":       {[]byte(sibling), false},
		"only bridge-level topics": {[]byte(onlyBridge), false},
		"a per-entity config":      {[]byte(`{"unique_id":"daikin_dev1_power","state_topic":"daikin/dev1/x/y/state"}`), false},
		"not json":                 {[]byte("nonsense"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := d.BundleIsOwnConfig(tc.body); got != tc.want {
				t.Errorf("BundleIsOwnConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheDowngradeTopicIsWhatTheDaemonPublishes verifies the one command the
// documentation gives a rolling-back user, against the code rather than against
// a description of it.
//
// Rolling back is not "stop publishing documents": Home Assistant's refusal is
// symmetric, so an old build's per-entity configs are refused while a document
// for the same ids is retained. The retained document must be cleared by hand,
// and the topic must be right — go-mtec2mqtt got it wrong in three of five
// places by composing it from a raw identifier instead of the sanitised node id.
func TestTheDowngradeTopicIsWhatTheDaemonPublishes(t *testing.T) {
	t.Parallel()
	for _, sc := range surfaceScenarios() {
		for _, d := range buildBundleSurface(t, sc).Documents {
			parsed, ok := publisher.ParseConfigTopic(config.DefaultHASSBaseTopic, d.Topic)
			if !ok || !parsed.Bundle {
				t.Errorf("%s does not parse as a device document topic", d.Topic)
				continue
			}
			want := config.DefaultHASSBaseTopic + "/" + publisher.BundleSegment + "/" + parsed.NodeID + "/config"
			if d.Topic != want {
				t.Errorf("document topic %s, want %s", d.Topic, want)
			}
			if strings.ContainsAny(parsed.NodeID, "/+# ") || parsed.NodeID == "" {
				t.Errorf("node id %q is not a usable topic segment", parsed.NodeID)
			}
			if !strings.HasPrefix(parsed.NodeID, hass.UniqueIDPrefix) {
				t.Errorf("node id %q is outside this bridge's namespace", parsed.NodeID)
			}
		}
	}
}

func ctx0() context.Context { return context.Background() }

// componentsOfRetained decodes the components map of a retained device document.
func componentsOfRetained(t *testing.T, br *retainedBroker, topic string) map[string]any {
	t.Helper()
	raw, ok := br.retained(topic)
	if !ok || raw == "" {
		t.Fatalf("no document retained at %s", topic)
	}
	var doc struct {
		Components map[string]any `json:"components"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode %s: %v", topic, err)
	}
	return doc.Components
}

// TestTheClaimSetIsPopulatedByTheDocumentsThatWentOut establishes, rather than
// assumes, the precondition the armed sweep rests on.
//
// Step 5 kept the sweep report-only for an arithmetic reason: the runtime
// published no config at all, so publisher.Runtime.Declared() was empty and an
// acting pass would have judged this bridge's whole retained fleet an orphan.
// What makes arming possible is that the documents now go out through that same
// runtime — so this asserts exactly that, off the runtime rather than off the
// publish path that filled it.
func TestTheClaimSetIsPopulatedByTheDocumentsThatWentOut(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()

	if n := len(c.ha().Declared()); n != 0 {
		t.Fatalf("the runtime declares %d topics before anything was published", n)
	}
	c.PublishOnline(ctx)
	c.pollOnce(ctx)

	declared := map[string]bool{}
	for _, topic := range c.ha().Declared() {
		declared[topic] = true
	}
	if !declared[migrationBundleTop] {
		t.Errorf("the runtime does not claim the document it published; Declared() = %v", c.ha().Declared())
	}
	// And nothing of the per-entity form is claimed, which is why the sweep's
	// verdict has to come from the payload: every four-segment config the
	// window sees is unclaimed, a sibling's included.
	for topic := range declared {
		if !strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/"+publisher.BundleSegment+"/") {
			t.Errorf("the runtime claims a non-document discovery topic: %s", topic)
		}
	}
}

// TestARuntimeWithoutTheLegacyFormIsABootPanic pins the composition root.
//
// publisher.Config.LegacyEntityTopics REPLACES the library's default rather
// than extending it, and the default reproduces 0 of this bridge's 264 retained
// config topics. Omitting it therefore publishes every document into a live
// conflict and shows no entities, with nothing in any log to say why.
//
// This is go-mtec2mqtt's worst mutation finding: dropping the field at the
// composition root was caught by NOTHING, because the daemon and every fixture
// each spelled the config out and the fixtures still carried it. The want side
// here is derived from RuntimeConfig, so there is no second literal to drift.
func TestARuntimeWithoutTheLegacyFormIsABootPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a runtime built without the legacy topic form was accepted")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "LegacyTopicByUniqueID") {
			t.Errorf("panic message does not name the expected form: %v", r)
		}
	}()
	rt := publisher.New(nopTransport{}, publisher.Config{
		Prefix: config.DefaultHASSBaseTopic,
		Layout: hass.Layout(config.TopicRoot),
		QoS:    DiscoveryQoS,
		Logger: slog.New(slog.DiscardHandler),
	})
	defer rt.Close()
	checkLegacyForms(rt)
}

// TestTheTwoPlanesAgreeOnTheDiscoveryPrefix is F15, checked in both directions
// rather than in the one that was found.
//
// publisher.New substitutes discovery.DefaultPrefix for an empty Config.Prefix
// and internal/hass uses the empty string verbatim; the library's topicPrefix
// trims a trailing slash and internal/hass does not. An empty MQTT level is
// legal and DISTINCT, so the two would never meet — which is how a sibling
// bridge ended up subscribing a birth topic Home Assistant never writes.
//
// The RAW value is fed to config.Normalize, so removing the normalisation fails
// this rather than an assertion that passes its own answer back to itself.
func TestTheTwoPlanesAgreeOnTheDiscoveryPrefix(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "/", "homeassistant", "homeassistant/", "/homeassistant/", "ha"} {
		t.Run("prefix="+raw, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{HASSBaseTopic: raw, MQTTTopic: raw}
			cfg.Normalize()

			rt := publisher.New(nopTransport{}, RuntimeConfig(cfg, slog.New(slog.DiscardHandler)))
			defer rt.Close()
			d := hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, "en", nil)

			// The library's own idea of the prefix and this package's must be
			// the same string, and the filter this daemon subscribes must match
			// the topic the runtime publishes to.
			if rt.Prefix() != cfg.HASSBaseTopic {
				t.Errorf("runtime prefix %q, config %q", rt.Prefix(), cfg.HASSBaseTopic)
			}
			document := publisher.BundleConfigTopic(rt.Prefix(), "daikin_dev1")
			if !publisher.MatchFilter(d.ConfigFilter(), document) {
				t.Errorf("this daemon's filter %q does not match the document topic %q",
					d.ConfigFilter(), document)
			}
			// And the state plane's bridge topic is one function, so the
			// availability topic every component names is the one the will
			// writes. publisher.New panics on a disagreement; reaching here is
			// the assertion.
			if rt.BridgeTopic() != d.BridgeStatusTopic() {
				t.Errorf("runtime bridge topic %q, discovery %q", rt.BridgeTopic(), d.BridgeStatusTopic())
			}
		})
	}
}

// TestAnInvalidDocumentIsRefusedBeforeItIsPublished pins the schema gate.
//
// discovery.Validate is Home Assistant's own discovery schemas, and a blocking
// finding means HA discards the whole document without a line in its log — so
// publishing one trades a working per-entity fleet for silence, since
// publisher.Runtime.PublishBundle retracts that fleet on its way in.
//
// It is checked at BUILD time, never on the publish path, and here it is
// checked against a genuinely invalid document rather than against the
// measurement that says none of this bridge's are. Every document this bridge
// renders validates clean today (TestEveryPublishedBundleValidates), which is
// exactly why this gate would otherwise be untested code.
func TestAnInvalidDocumentIsRefusedBeforeItIsPublished(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)

	in := buildHamqttInputs(t, surfaceScenarios()[0])
	prefix := config.DefaultHASSBaseTopic
	bundles, err := in.disc.RenderBundles(prefix, version.Version, in.points, in.infos, in.climateInfos)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("render: %v (%d bundles)", err, len(bundles))
	}
	good := bundles[0]
	if !c.bundleValidates(good) {
		t.Fatal("a document this bridge actually publishes was refused")
	}

	// origin.name is blocking on a device document and merely advisory on a
	// per-entity config — which is why F12 lands at this step and not at the
	// byte-equality proof.
	bad := hass.Bundle{Topic: good.Topic, Bundle: cloneBundle(good.Bundle)}
	bad.Bundle.Origin.Name = ""
	if c.bundleValidates(bad) {
		t.Error("a document Home Assistant's schemas refuse was accepted")
	}
}

// cloneBundle copies a document shallowly enough to invalidate one field
// without disturbing the original.
func cloneBundle(b *discovery.Bundle) *discovery.Bundle {
	out := *b
	return &out
}

// TestTheTombstoneReadBackIgnoresASiblingsDocument is the safety the read-back
// itself needs.
//
// The previous document is read back from the broker, and a device document's
// topic is byte-identical between two go-daikin2mqtt instances seeing one
// ONECTA device (F14) — so the topic cannot say whose it is. If a sibling's
// component set became this instance's prior state, the diff would tombstone
// entities this instance never published, which is strictly worse than the
// phantom the read-back exists to prevent.
func TestTheTombstoneReadBackIgnoresASiblingsDocument(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)

	// A document on this instance's own node id whose components all name
	// another device's topics: same namespace, same prefix, same root.
	br.retain(migrationBundleTop, `{"device":{"identifiers":["daikin_dev1"]},
		"origin":{"name":"go-daikin2mqtt"},"components":{
		"a_component_we_never_published":{"platform":"sensor","unique_id":"daikin_dev9_ghost",
		 "state_topic":"daikin/dev9/climateControl/ghost/state"}}}`)

	c.PublishOnline(context.Background())
	c.pollOnce(context.Background())

	if _, ok := componentsOfRetained(t, br, migrationBundleTop)["a_component_we_never_published"]; ok {
		t.Error("a sibling's component was tombstoned into this instance's own document")
	}
}

// TestATombstoneIsNotRepeatedWithinOneProcess is TestATombstoneIsNotRepeatedForever
// for the in-process memo rather than the broker read-back.
//
// The read-back happens once per connection; every publish after it diffs
// against what this process last wrote. If that memo carried tombstones forward
// as prior state, a dead component key would be re-marked in every document for
// the life of the connection.
func TestATombstoneIsNotRepeatedWithinOneProcess(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()

	c.PublishOnline(ctx)
	c.pollOnce(ctx)

	// A component leaves, on the SAME connection: no second read-back happens,
	// so the diff is against the memo recordPublished left.
	const first, second = "room_temperature", "refresh"
	c.deps.Catalog = catalogWithout(t, first)
	c.pollOnce(ctx)
	if _, ok := componentsOfRetained(t, br, migrationBundleTop)[first]; !ok {
		t.Fatalf("%q was not tombstoned; this test would pass vacuously", first)
	}

	// A second component leaves. The first is done with and must not reappear.
	c.deps.Catalog = catalogWithout(t, first, second)
	c.pollOnce(ctx)
	after := componentsOfRetained(t, br, migrationBundleTop)
	if _, ok := after[second]; !ok {
		t.Errorf("%q was omitted rather than tombstoned", second)
	}
	if _, ok := after[first]; ok {
		t.Errorf("%q is tombstoned again a publish later; the memo carries tombstones forward as prior state", first)
	}
}

// TestNewRefusesARuntimeBuiltWithoutRuntimeConfig drives the composition-root
// check through Coordinator.New, which is where it has to fire.
//
// TestARuntimeWithoutTheLegacyFormIsABootPanic pins the check itself; this pins
// that anything is calling it. go-mtec2mqtt's equivalent hole was exactly this
// shape — the guard existed, the composition root did not reach it, and no test
// noticed.
func TestNewRefusesARuntimeBuiltWithoutRuntimeConfig(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Coordinator.New accepted a runtime built without the legacy topic form")
		}
	}()
	cfg := testConfig()
	cfg.HASSBaseTopic = config.DefaultHASSBaseTopic
	New(Deps{
		Cfg: cfg, Client: &stubCloud{}, MQTT: newRetainedBroker(), Catalog: loadTestCatalog(t),
		Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
		NewHARuntime: func() *publisher.Runtime {
			return publisher.New(nopTransport{}, publisher.Config{
				Prefix: cfg.HASSBaseTopic,
				Layout: hass.Layout(cfg.MQTTTopic),
				QoS:    DiscoveryQoS,
				Logger: slog.New(slog.DiscardHandler),
			})
		},
	})
}

// TestABatchFromAReplacedConnectionDoesNotCommitItsSignature pins the
// generation guard.
//
// A reconnect can land while a batch of documents is still going out. The fresh
// runtime has published nothing and has retracted nothing, so committing the
// old batch's signature would suppress the republish that connection is owed —
// and the fleet would stay dark until the entity set happened to change, which
// is F16 by another route.
func TestABatchFromAReplacedConnectionDoesNotCommitItsSignature(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	ctx := context.Background()
	c.PublishOnline(ctx)

	// The reconnect lands the moment the first document reaches the socket.
	var once sync.Once
	br.onPublish = func(topic string) {
		if !strings.HasPrefix(topic, config.DefaultHASSBaseTopic+"/"+publisher.BundleSegment+"/") {
			return
		}
		once.Do(func() { c.discoveryGen.Add(1) })
	}
	c.pollOnce(ctx)

	c.mu.Lock()
	sig := c.lastDiscSig
	c.mu.Unlock()
	if sig != "" {
		t.Errorf("a batch published on a replaced connection committed its signature: %q", sig)
	}
}

// TestThePublishedDocumentCarriesThisBuildsVersion asserts the origin block
// where it is SENT, not where it is built.
//
// The pinned artefact deliberately carries a placeholder (goldenBundleSW) so a
// release tag does not stale twelve digests — which means the goldens cannot
// see this at all, and an origin wired to the wrong string would be invisible.
// That is go-mtec2mqtt's finding: its origin was asserted only at construction,
// so the coordinator could have handed it the inverter's firmware version.
func TestThePublishedDocumentCarriesThisBuildsVersion(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	c.PublishOnline(context.Background())
	c.pollOnce(context.Background())

	raw, ok := br.retained(migrationBundleTop)
	if !ok || raw == "" {
		t.Fatal("no document was published")
	}
	var doc struct {
		Origin struct {
			Name string `json:"name"`
			SW   string `json:"sw_version"`
			URL  string `json:"support_url"`
		} `json:"origin"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Origin.Name != hass.OriginName || doc.Origin.URL != hass.OriginURL {
		t.Errorf("published origin = %+v", doc.Origin)
	}
	if doc.Origin.SW != version.Version {
		t.Errorf("published origin.sw_version = %q, want the build version %q", doc.Origin.SW, version.Version)
	}
}

// TestTheRenderedTopicIsTheOneTheRuntimeWrites pins the one place a prefix
// spelled with a trailing slash would separate the two planes again.
//
// publisher.Runtime.PublishBundle addresses the document with
// publisher.BundleConfigTopic, which normalises the prefix;
// discovery.Bundle.Topic concatenates. config.Normalize means the daemon cannot
// reach the difference today (F15), but a renderer that named a topic the
// runtime never writes would publish a perfectly valid document where nobody
// looks, and the goldens could not see it because they only ever pass a
// normalised prefix.
func TestTheRenderedTopicIsTheOneTheRuntimeWrites(t *testing.T) {
	t.Parallel()
	in := buildHamqttInputs(t, surfaceScenarios()[0])
	for _, prefix := range []string{"homeassistant", "homeassistant/", "ha"} {
		bundles, err := in.disc.RenderBundles(prefix, version.Version, in.points, in.infos, in.climateInfos)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bundles {
			if want := publisher.BundleConfigTopic(prefix, b.Bundle.NodeID); b.Topic != want {
				t.Errorf("prefix %q: rendered topic %q, the runtime writes %q", prefix, b.Topic, want)
			}
		}
	}
}

// TestAFailedDocumentStaysInTheClaimSet pins the tripwire under the sweep guard.
//
// The claim set the sweep subtracts names every document of the batch,
// INCLUDING one whose publish failed, so a transient broker error can never
// make the sweep clear a config this daemon still intends to publish. Today
// that can only matter if the allSent guard is ever relaxed — the sweep does
// not run at all on a failed batch — which is exactly why it is asserted here
// rather than left to be rediscovered.
func TestAFailedDocumentStaysInTheClaimSet(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)
	br.refuse(config.DefaultHASSBaseTopic + "/" + publisher.BundleSegment + "/")

	in := buildHamqttInputs(t, surfaceScenarios()[0])
	bundles, err := in.disc.RenderBundles(
		config.DefaultHASSBaseTopic, version.Version, in.points, in.infos, in.climateInfos)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("render: %v", err)
	}
	published, allSent := c.publishBundles(context.Background(), c.ha(), bundles)
	if allSent {
		t.Fatal("the refused publish was reported as sent")
	}
	for _, b := range bundles {
		if !published[b.Topic] {
			t.Errorf("%s was dropped from the claim set because its publish failed", b.Topic)
		}
	}
}
