// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
)

// Invariants of the published surface, asserted against the BUILDERS rather
// than against the goldens (ADR 0070, phase 8 step 0).
//
// Everything in this file rebuilds the surface from the real publish path and
// checks a property of it. Nothing here reads testdata, so every assertion
// still fails immediately after `-update-surface-golden` has been run — which
// is exactly what a golden file cannot do for itself.
//
// The expected values are Go literals, so the diff of a change to them is a
// declaration in the review, not a regenerated blob.

// surfaceOf rebuilds one scenario by name.
func surfaceOf(t *testing.T, name string) []recordedMsg {
	t.Helper()
	for _, sc := range surfaceScenarios() {
		if sc.name == name {
			return buildSurface(t, sc)
		}
	}
	t.Fatalf("no scenario %q", name)
	return nil
}

// configsOf returns the discovery configs of a recorded surface, keyed by
// config topic.
func configsOf(msgs []recordedMsg) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range msgs {
		if strings.HasPrefix(m.Topic, "homeassistant/") && m.JSON != nil {
			out[m.Topic] = m.JSON
		}
	}
	return out
}

// topicsOf returns every topic the surface was published to.
func topicsOf(msgs []recordedMsg) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		out[m.Topic] = true
	}
	return out
}

// str reads a string field, "" when absent.
func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// stateTopicKeys are every key in a discovery config that names a topic this
// daemon is expected to publish to.
var stateTopicKeys = []string{
	"state_topic", "json_attributes_topic", "availability_topic",
	"mode_state_topic", "temperature_state_topic", "current_temperature_topic",
	"fan_mode_state_topic", "swing_mode_state_topic",
	"swing_horizontal_mode_state_topic", "preset_mode_state_topic",
}

// commandTopicKeys are every key naming a topic the daemon must be subscribed to.
var commandTopicKeys = []string{
	"command_topic", "mode_command_topic", "temperature_command_topic",
	"fan_mode_command_topic", "swing_mode_command_topic",
	"swing_horizontal_mode_command_topic", "preset_mode_command_topic",
}

// knownAdvertisedButUnpublished is the exhaustive list of topics a discovery
// config advertises that the publish path never writes to, keyed by scenario.
//
// F6: in local mode the Faikin read path owns fan_mode, and it only publishes
// it when the module's `fan` word is a key of faikinFanToCloud — whose keys are
// the cloud vocabulary (`low`, `medium`, …) while faikin.State documents the
// firmware as sending `auto|1..5|quiet`. The pinned state uses `"3"`, which
// maps to nothing, so the climate entity's fan dropdown stays unknown.
//
// Anything not in this map is a builder divergence and fails the test.
var knownAdvertisedButUnpublished = map[string][]string{
	"multisplit.local.en": {
		"daikin/11112222-3333-4444-5555-666677778888/climateControl/fan_mode/state",
		"daikin/809d41d9-4d42-45fa-af6a-84b512143672/climateControl/fan_mode/state",
	},
}

// TestStateTopicBuildersAgree is the builder-against-builder pin.
//
// The measurement counted nine `<root>/<device>/<embedded>/<topic>/state`
// builders; recounting before converging them found TWELVE, in four packages —
// the three it missed are hass/climate.go's auxBase (the composite climate's
// five synthetic slots), hass/schedule.go's ScheduleStateTopic, and
// coordinator/schedule.go:288 (the per-schedule enable switch). The last two
// are a pair that compose the same topic in two packages from two different
// "scheduler" constants, one of them beside a bare "enabled" literal.
//
// They are now all internal/layout, so this test compares what the retained
// configs advertise against what the publish path writes rather than comparing
// twelve expressions. A drift still leaves entities pointing at a topic nobody
// writes: permanently unknown, nothing in the log, nothing in Home Assistant's
// registry to notice — so the pin stays, and it is what catches a change to the
// layout that only one side of the tree is updated for.
func TestStateTopicBuildersAgree(t *testing.T) {
	t.Parallel()
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			msgs := buildSurface(t, sc)
			published := topicsOf(msgs)
			allowed := map[string]bool{}
			for _, a := range knownAdvertisedButUnpublished[sc.name] {
				allowed[a] = true
			}

			advertised := 0
			for cfgTopic, cfg := range configsOf(msgs) {
				for _, key := range stateTopicKeys {
					st := str(cfg, key)
					if st == "" {
						continue
					}
					advertised++
					if published[st] || allowed[st] {
						continue
					}
					t.Errorf("%s advertises %s=%q, which the publish path never writes to — "+
						"the discovery builder and the coordinator's inline topic formatting have diverged",
						cfgTopic, key, st)
				}
			}
			if advertised == 0 {
				t.Fatal("no state topics advertised")
			}
			// Every allow-listed topic must still be advertised, or the
			// exception has outlived its finding and should be deleted.
			for a := range allowed {
				if !published[a] {
					continue
				}
				t.Errorf("%s is published after all — drop it from knownAdvertisedButUnpublished", a)
			}
			t.Logf("%s: %d advertised state topics, %d allowed dead", sc.name, advertised, len(allowed))
		})
	}
}

// TestCommandTopicsAreSubscribed pins the inbound half: every command topic a
// config advertises must be matched by the one filter the coordinator
// subscribes to, `<root>/+/+/+/set`.
//
// The filter is read from the layout the coordinator actually subscribes with,
// not written out as a literal here, so narrowing the filter fails this test
// instead of quietly leaving every advertised command unroutable. A command
// Home Assistant publishes to a topic nothing is subscribed to is reported as
// sent, and the entity snaps back to its old value a poll later.
func TestCommandTopicsAreSubscribed(t *testing.T) {
	t.Parallel()
	filter := layout.New(config.TopicRoot).CommandFilter()
	if filter != "daikin/+/+/+/set" {
		t.Fatalf("command filter = %q, want %q — the installed base's subscription", filter, "daikin/+/+/+/set")
	}
	total := 0
	for _, sc := range surfaceScenarios() {
		for cfgTopic, cfg := range configsOf(buildSurface(t, sc)) {
			for _, key := range commandTopicKeys {
				ct := str(cfg, key)
				if ct == "" {
					continue
				}
				total++
				if !matchFilter(filter, ct) {
					t.Errorf("%s advertises %s=%q, which %q does not match", cfgTopic, key, ct, filter)
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("no command topics advertised")
	}
	t.Logf("%d advertised command topics, all under %s", total, filter)
}

// matchFilter implements MQTT topic-filter matching for + and #.
func matchFilter(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if seg != "+" && seg != tp[i] {
			return false
		}
	}
	return len(f) == len(tp)
}

// TestConfigTopicForm pins the four-segment, node-id-less discovery topic form.
//
// Step 6 of the phase depends on this: go-hamqtt's SupersededTopics defaults to
// the five-segment LegacyTopicWithNodeID form, which would retract none of
// these. This bridge needs publisher.LegacyTopicByUniqueID.
func TestConfigTopicForm(t *testing.T) {
	t.Parallel()
	platforms := map[string]bool{
		"sensor": true, "binary_sensor": true, "switch": true,
		"select": true, "number": true, "button": true, "climate": true,
	}
	total := 0
	for _, sc := range surfaceScenarios() {
		for topic, cfg := range configsOf(buildSurface(t, sc)) {
			total++
			parts := strings.Split(topic, "/")
			if len(parts) != 4 {
				t.Errorf("%s: %d segments, want 4 (<prefix>/<platform>/<unique_id>/config)", topic, len(parts))
				continue
			}
			if parts[0] != "homeassistant" || parts[3] != "config" {
				t.Errorf("%s: unexpected prefix/suffix", topic)
			}
			if !platforms[parts[1]] {
				t.Errorf("%s: unknown platform %q", topic, parts[1])
			}
			if uid := str(cfg, "unique_id"); parts[2] != uid {
				t.Errorf("%s: third segment %q is not the unique_id %q", topic, parts[2], uid)
			}
		}
	}
	if total != 264 {
		t.Errorf("config topics across all scenarios = %d, want 264", total)
	}
}

// TestNoDuplicateEntityRegistryKeys pins that this bridge publishes no
// duplicate identity. Home Assistant keys the entity registry on
// (domain, platform, unique_id); inside one MQTT integration that reduces to
// (platform, unique_id), which is also what go-hamqtt v0.32.0 validates.
//
// Measured: zero duplicates in every scenario, on either key — unlike mtec's
// nine. The phase therefore inherits no duplicate-unique_id question at all.
func TestNoDuplicateEntityRegistryKeys(t *testing.T) {
	t.Parallel()
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			byPair := map[string]string{}
			byUID := map[string]string{}
			byEntityID := map[string]string{}
			for topic, cfg := range configsOf(buildSurface(t, sc)) {
				platform := strings.Split(topic, "/")[1]
				uid := str(cfg, "unique_id")
				if uid == "" {
					t.Errorf("%s: no unique_id", topic)
					continue
				}
				if prev, dup := byPair[platform+"|"+uid]; dup {
					t.Errorf("%s: (platform=%s, unique_id=%s) already published by %s", topic, platform, uid, prev)
				}
				byPair[platform+"|"+uid] = topic
				if prev, dup := byUID[uid]; dup {
					t.Errorf("%s: unique_id %q also on %s (legal across platforms, but new here)", topic, uid, prev)
				}
				byUID[uid] = topic
				eid := str(cfg, "default_entity_id")
				if prev, dup := byEntityID[eid]; dup {
					t.Errorf("%s: default_entity_id %q also on %s", topic, eid, prev)
				}
				byEntityID[eid] = topic
			}
		})
	}
}

// TestAvailabilityModelIsBridgeOnly pins the availability model: one level,
// the bridge LWT, expressed with the singular availability_topic and top-level
// payloads, no availability_mode, on every entity of every platform.
//
// This is mtec's shape, not homeconnect's: go-hamqtt's default
// {LevelBridge, LevelDevice} with mode "all" would name a per-device
// availability topic this bridge never publishes, leaving every entity
// permanently unavailable. model.BridgeOnly() is the setting the phase needs.
func TestAvailabilityModelIsBridgeOnly(t *testing.T) {
	t.Parallel()
	const want = "daikin/bridge/status"
	for _, sc := range surfaceScenarios() {
		msgs := buildSurface(t, sc)
		published := topicsOf(msgs)
		if !published[want] {
			t.Errorf("%s: the bridge status topic %q is never published", sc.name, want)
		}
		for topic, cfg := range configsOf(msgs) {
			if got := str(cfg, "availability_topic"); got != want {
				t.Errorf("%s: availability_topic %q, want %q", topic, got, want)
			}
			if got := str(cfg, "payload_available"); got != "online" {
				t.Errorf("%s: payload_available %q, want \"online\"", topic, got)
			}
			if got := str(cfg, "payload_not_available"); got != "offline" {
				t.Errorf("%s: payload_not_available %q, want \"offline\"", topic, got)
			}
			for _, absent := range []string{"availability", "availability_mode", "availability_template"} {
				if _, ok := cfg[absent]; ok {
					t.Errorf("%s: unexpected %q key", topic, absent)
				}
			}
		}
	}
}

// TestPublishQoSAndRetain pins the delivery guarantee of every single publish.
//
// go-hamqtt's publisher.QoS zero value means UNSET and resolves to QoS 1. This
// bridge passes mqtt.QoS0 at all fifteen call sites, so the migration must
// spell it publisher.QoSAtMostOnce (0x80) or silently upgrade the whole surface.
func TestPublishQoSAndRetain(t *testing.T) {
	t.Parallel()
	total := 0
	for _, sc := range surfaceScenarios() {
		for _, m := range buildSurface(t, sc) {
			total++
			if m.QoS != 0 {
				t.Errorf("%s: QoS %d, want 0", m.Topic, m.QoS)
			}
			if !m.Retain {
				t.Errorf("%s: retain=false; every topic this daemon owns is retained", m.Topic)
			}
		}
	}
	t.Logf("%d publishes, all QoS 0 retained", total)
}

// TestDeviceBlocksArePinned holds the device-registry keys as Go literals.
// Home Assistant keys the device registry on `identifiers` and has no migration
// path, so these are the strings the phase may not change.
func TestDeviceBlocksArePinned(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"air-to-air-dx4.en": `["daikin_809d41d9-4d42-45fa-af6a-84b512143672"]`,
		"multisplit.en": `["daikin_11112222-3333-4444-5555-666677778888"],` +
			`["daikin_809d41d9-4d42-45fa-af6a-84b512143672"],` +
			`["daikin_gateway_GW0000000001"],["daikin_gateway_GW0000000002"],` +
			`["daikin_outdoor_ODU0000000001"]`,
		"multisplit.scheduler.en": `["daikin_11112222-3333-4444-5555-666677778888"],` +
			`["daikin_809d41d9-4d42-45fa-af6a-84b512143672"],` +
			`["daikin_gateway_GW0000000001"],["daikin_gateway_GW0000000002"],` +
			`["daikin_outdoor_ODU0000000001"],["daikin_scheduler"]`,
		"altherma-air-to-water-wlan.en": `["daikin_63731cd9-3240-476c-9d2b-7c41dc94d82c"],` +
			`["daikin_gateway_ABCDEF0123456789"]`,
	}
	for scenario, wantIDs := range want {
		var seen []string
		for _, cfg := range configsOf(surfaceOf(t, scenario)) {
			dev, ok := cfg["device"].(map[string]any)
			if !ok {
				t.Fatalf("%s: a config has no device block", scenario)
			}
			b, err := json.Marshal(dev["identifiers"])
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, string(b))
		}
		sort.Strings(seen)
		seen = slicesCompact(seen)
		if got := strings.Join(seen, ","); got != wantIDs {
			t.Errorf("%s device identifiers:\n  got  %s\n  want %s", scenario, got, wantIDs)
		}
	}
}

func slicesCompact(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// TestIdentityIsLanguageIndependent pins the entity-id invariant the repo's own
// CLAUDE.md calls out: unique_id and default_entity_id are English and must not
// move with LANGUAGE. Only the display `name` and a select's `options` may.
//
// Compared en against de directly, so no golden participates.
func TestIdentityIsLanguageIndependent(t *testing.T) {
	t.Parallel()
	localizedSeeds := 0
	for _, pair := range [][2]string{
		{"air-to-air-dx4.en", "air-to-air-dx4.de"},
		{"altherma-air-to-water-wlan.en", "altherma-air-to-water-wlan.de"},
		{"d2cnd-gas-boiler.en", "d2cnd-gas-boiler.de"},
		{"airpurifier.en", "airpurifier.de"},
		{"multisplit.en", "multisplit.de"},
	} {
		en, de := configsOf(surfaceOf(t, pair[0])), configsOf(surfaceOf(t, pair[1]))
		if len(en) != len(de) {
			t.Errorf("%s/%s: %d vs %d configs", pair[0], pair[1], len(en), len(de))
		}
		for topic, e := range en {
			d, ok := de[topic]
			if !ok {
				t.Errorf("%s: published in %s but not in %s", topic, pair[0], pair[1])
				continue
			}
			for _, key := range []string{"unique_id", "state_topic", "command_topic"} {
				if str(e, key) != str(d, key) {
					t.Errorf("%s: %s moves with LANGUAGE: %q vs %q", topic, key, str(e, key), str(d, key))
				}
			}
			// F2: default_entity_id is seeded from the DEVICE BLOCK's name,
			// and the shared gateway / outdoor sub-devices are named with a
			// localized label ("Outdoor unit" / "Außengerät"), so their
			// entity-id seed does move with LANGUAGE — in direct contradiction
			// of the invariant CLAUDE.md states. Every entity on a main device
			// is unaffected, because the main device's name is the operator's.
			if str(e, "default_entity_id") != str(d, "default_entity_id") {
				if !knownLocalizedEntityIDSeed[topic] {
					t.Errorf("%s: default_entity_id moves with LANGUAGE: %q vs %q (F2)",
						topic, str(e, "default_entity_id"), str(d, "default_entity_id"))
				}
				localizedSeeds++
			}
			eb, _ := json.Marshal(e["device"].(map[string]any)["identifiers"])
			db, _ := json.Marshal(d["device"].(map[string]any)["identifiers"])
			if !bytes.Equal(eb, db) {
				t.Errorf("%s: device.identifiers moves with LANGUAGE: %s vs %s", topic, eb, db)
			}
		}
	}
	if localizedSeeds != 2 {
		t.Errorf("configs whose default_entity_id moves with LANGUAGE = %d, want 2 (F2)", localizedSeeds)
	}
}

// knownLocalizedEntityIDSeed lists every config whose default_entity_id is
// built from a localized device name (F2). All of them hang off a shared
// gateway / outdoor sub-device, whose HA name this bridge composes from a
// translated label rather than from operator text.
var knownLocalizedEntityIDSeed = map[string]bool{
	"homeassistant/sensor/daikin_outdoor_ODU0000000001_outdoor_temperature/config": true,
	"homeassistant/button/daikin_outdoor_ODU0000000001_refresh/config":             true,
}

// --- slug agreement --------------------------------------------------------

// librarySlug is a verbatim transcription of go-hamqtt v0.32.0's topic.Slug
// (topic/topic.go), kept here so the divergence can be counted without taking
// the dependency one phase early. It is unreachable from any production path
// and is deleted when the bridge actually adopts the library.
var libraryTransliterations = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue",
	"Ä", "ae", "Ö", "oe", "Ü", "ue",
	"ß", "ss",
	"å", "a", "æ", "ae", "ø", "oe",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"á", "a", "à", "a", "â", "a",
	"í", "i", "ì", "i", "î", "i",
	"ó", "o", "ò", "o", "ô", "o",
	"ú", "u", "ù", "u", "û", "u",
	"ñ", "n", "ç", "c",
)

func librarySlug(s string) string {
	s = libraryTransliterations.Replace(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(s))
	lastSep := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
			lastSep = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
			lastSep = false
		default:
			if !lastSep {
				b.WriteByte('_')
				lastSep = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if out == "" {
		return "x"
	}
	return out
}

// bridgeSlug is a transcription of internal/hass.slugify, which is unexported.
// Kept beside librarySlug so the two are read together.
var bridgeTransliterations = strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss")

func bridgeSlug(s string) string {
	s = bridgeTransliterations.Replace(strings.ToLower(s))
	var b strings.Builder
	pendingSep := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)
		} else {
			pendingSep = true
		}
	}
	return b.String()
}

// TestSlugAgreementOverTheRealCatalogue counts, rather than assumes, how far
// this bridge's slugify and go-hamqtt's topic.Slug disagree.
//
// Two populations: the 55 catalog topics (pure ASCII snake_case) and the device
// names, which are operator- and cloud-supplied and are where every divergence
// lives. The expected divergence rows are Go literals — a change to either
// function moves them and fails here.
func TestSlugAgreementOverTheRealCatalogue(t *testing.T) {
	t.Parallel()

	// (a) the catalog topics.
	cat := loadRealCatalog(t)
	diverged := 0
	for _, e := range cat.Entries() {
		if bridgeSlug(e.Topic) != librarySlug(e.Topic) {
			diverged++
			t.Logf("catalog topic diverges: %q -> %q vs %q", e.Topic, bridgeSlug(e.Topic), librarySlug(e.Topic))
		}
	}
	if diverged != 0 {
		t.Errorf("catalog topics diverging = %d, want 0", diverged)
	}
	if n := len(cat.Entries()); n != 55 {
		t.Errorf("catalog entries = %d, want 55", n)
	}

	// (b) device-name probes. The device name is the operator's own room label
	// and reaches unique_id (for gateway/outdoor entities it does not, but it
	// reaches default_entity_id for every entity) — so a divergence here
	// re-keys entity ids across the whole device.
	probes := []struct{ name, bridge, library string }{
		{"Wohnzimmer", "wohnzimmer", "wohnzimmer"},
		{"Küche", "kuche", "kueche"},
		{"Büro", "buro", "buero"},
		{"Außengerät", "aussengerat", "aussengeraet"},
		{"Gäste-WC", "gaste_wc", "gaeste-wc"},
		{"Café", "caf", "cafe"},
		{"EG-Wohnzimmer", "eg_wohnzimmer", "eg-wohnzimmer"},
		{"ÜÄÖ", "uao", "ueaeoe"},
		{"", "", "x"},
	}
	divergent := 0
	for _, p := range probes {
		if got := bridgeSlug(p.name); got != p.bridge {
			t.Errorf("bridgeSlug(%q) = %q, want %q", p.name, got, p.bridge)
		}
		if got := librarySlug(p.name); got != p.library {
			t.Errorf("librarySlug(%q) = %q, want %q", p.name, got, p.library)
		}
		if p.bridge != p.library {
			divergent++
		}
	}
	if divergent != 8 {
		t.Errorf("diverging device-name probes = %d, want 8 of %d", divergent, len(probes))
	}
}

// TestTwoDefaultInstancesCollideOnEveryString is the two-instance measurement.
//
// Every identity string this bridge publishes is derived from the ONECTA device
// id (or a component serial) and the catalog topic. Neither MQTT_TOPIC nor
// HASS_BASE_TOPIC nor any instance identifier appears in unique_id,
// device.identifiers or the config topic — so two daemons on one broker that
// see any device in common publish byte-identical configs to byte-identical
// topics, last writer wins. And because config.MQTTClientID is a compile-time
// constant with no override, they cannot even stay connected at the same time.
func TestTwoDefaultInstancesCollideOnEveryString(t *testing.T) {
	t.Parallel()
	a := configsOf(surfaceOf(t, "multisplit.en"))
	// A second instance differs only in the operator's chosen language; nothing
	// about its identity plane can differ, because nothing instance-specific
	// enters it.
	b := configsOf(surfaceOf(t, "multisplit.de"))
	shared := 0
	for topic, ca := range a {
		cb, ok := b[topic]
		if !ok {
			continue
		}
		shared++
		if str(ca, "unique_id") != str(cb, "unique_id") {
			t.Errorf("%s: unique_id differs between instances", topic)
		}
	}
	if shared != len(a) {
		t.Errorf("shared config topics = %d, want all %d", shared, len(a))
	}
	t.Logf("two default instances share all %d config topics and all %d unique_ids", shared, shared)
}

// TestSurfaceCensus holds the measured entity counts as Go literals, so a
// silent change in what the bridge publishes is visible without reading a
// 2 500-line golden.
func TestSurfaceCensus(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		entities, messages int
		platforms          string
	}{
		"airpurifier.en":                {8, 26, "binary_sensor=2 select=1 sensor=4 switch=1"},
		"airpurifier.de":                {8, 26, "binary_sensor=2 select=1 sensor=4 switch=1"},
		"air-to-air-dx4.en":             {16, 58, "binary_sensor=2 button=1 climate=1 sensor=11 switch=1"},
		"air-to-air-dx4.de":             {16, 58, "binary_sensor=2 button=1 climate=1 sensor=11 switch=1"},
		"altherma-air-to-water-wlan.en": {20, 68, "binary_sensor=2 button=1 climate=1 number=2 sensor=11 switch=3"},
		"altherma-air-to-water-wlan.de": {20, 68, "binary_sensor=2 button=1 climate=1 number=2 sensor=11 switch=3"},
		"d2cnd-gas-boiler.en":           {13, 46, "binary_sensor=5 button=1 climate=1 sensor=5 switch=1"},
		"d2cnd-gas-boiler.de":           {13, 46, "binary_sensor=5 button=1 climate=1 sensor=5 switch=1"},
		"multisplit.en":                 {30, 113, "binary_sensor=4 button=1 climate=2 sensor=21 switch=2"},
		"multisplit.de":                 {30, 113, "binary_sensor=4 button=1 climate=2 sensor=21 switch=2"},
		"multisplit.local.en":           {52, 219, "binary_sensor=4 button=1 climate=2 number=1 sensor=38 switch=6"},
		"multisplit.scheduler.en":       {38, 142, "binary_sensor=4 button=1 climate=2 sensor=27 switch=4"},
	}
	for _, sc := range surfaceScenarios() {
		msgs := buildSurface(t, sc)
		cfgs := configsOf(msgs)
		w, ok := want[sc.name]
		if !ok {
			t.Errorf("no census pinned for scenario %q", sc.name)
			continue
		}
		if len(cfgs) != w.entities {
			t.Errorf("%s: entities = %d, want %d", sc.name, len(cfgs), w.entities)
		}
		if len(msgs) != w.messages {
			t.Errorf("%s: distinct messages = %d, want %d", sc.name, len(msgs), w.messages)
		}
		byPlatform := map[string]int{}
		for topic := range cfgs {
			byPlatform[strings.Split(topic, "/")[1]]++
		}
		keys := make([]string, 0, len(byPlatform))
		for k := range byPlatform {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%s=%d", k, byPlatform[k])
		}
		if b.String() != w.platforms {
			t.Errorf("%s: platform census %q, want %q", sc.name, b.String(), w.platforms)
		}
	}
}
