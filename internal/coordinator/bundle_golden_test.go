// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// The device-bundle pin (ADR 0070 phase 8 step 6).
//
// A NEW artefact, alongside the twelve per-entity goldens and never instead of
// them: the claim this step makes is a statement about the RELATIONSHIP between
// the two forms, and it is only checkable while both exist. The per-entity
// goldens are now the retraction contract (see buildSurface); these are what
// goes on the wire.
//
// Topic and payload are pinned together, because they fail differently and both
// in silence: a wrong topic publishes a perfectly valid document where Home
// Assistant never looks, and a wrong payload publishes to the right place a
// document Home Assistant discards without a line in its log.
//
// It has its OWN update flag, precisely so that -update-surface-golden is
// unreachable from a bundle test. That flag is never passed by anything here,
// and it could not rewrite goldenDigests even if it were.
var updateBundleGolden = flag.Bool("update-bundle-golden", false,
	"rewrite the device-bundle goldens under testdata/bundle (digests must then be updated by hand)")

// goldenBundleSW is the origin block's sw_version inside the pinned artefact.
//
// A fixed string rather than version.Version, so cutting a release tag does not
// stale twelve goldens and twelve digests. That the REAL origin is wired to the
// build version is asserted separately, by
// TestBundleOriginIsWiredFromTheBuildVersion — which is the split that keeps
// both facts checkable instead of trading one for the other.
const goldenBundleSW = "0.0.0-golden"

// bundleDigests is the SHA-256 of each scenario's canonical bundle encoding,
// held outside testdata so a regenerated golden cannot silently re-bless a
// changed document set. -update-bundle-golden never writes this map.
var bundleDigests = map[string]string{
	"air-to-air-dx4.en":             "ad3e520e2d267e3819f42177a5495fda280f155a3bd8eda114ff868a321fb611",
	"air-to-air-dx4.de":             "1576ded7fd3daaa4cf6aa917a4f4a00fdcf1442afbd9bb38ab6197cccaeece20",
	"airpurifier.en":                "7a92f7ff9656ebfad500bf19b41125b6c543b5094fbc99f7eb9e9f434a3eb57a",
	"airpurifier.de":                "c7d4981260c59f9f24c04533e5b75df4097683d628ba7410ec9b50fde379bf09",
	"altherma-air-to-water-wlan.en": "e4b7a7e65648ecaf793134e90d22b8b43d796932d5a5eb93ebaf2f77e7cbd7b3",
	"altherma-air-to-water-wlan.de": "132bd811e75e20e515118bd2580d04e9fcf0355a9c8ff320a562ff32eb596bf4",
	"d2cnd-gas-boiler.en":           "c5b48189086f166e521774405133b87ef1dbe7e1208f85968845af71a006e300",
	"d2cnd-gas-boiler.de":           "d0f8a8f8ae1cd65300650bdab7e3edd43e1d9b506367fe79a99a3fcfdb7c16f0",
	"multisplit.en":                 "caa509655eac2457ea9a326913c1810cb8ab0c52e310ffa32a3ad4d25b5ccb83",
	"multisplit.de":                 "b2d7d015375df3a7b85678bfc19ee21cbb4ed842338eb7a70c60a3811f07a379",
	"multisplit.local.en":           "f3152bb51b9c188f99181a4542e6b37ee4874b85a4cfdb73ad14332020c83517",
	"multisplit.scheduler.en":       "fcca47cf7e91d1ae42e209ef4cc846d0e60f35718d4a4df8cd779cb11e823094",
}

// bundleDoc is one scenario's device documents.
type bundleDoc struct {
	Scenario  string       `json:"scenario"`
	Documents []bundleFile `json:"documents"`
}

// bundleFile is one retained device document plus the per-entity config topics
// publishing it retracts.
type bundleFile struct {
	Topic string `json:"topic"`
	// Superseded is the retraction list publisher.Runtime.PublishBundle applies
	// BEFORE this document. It is pinned beside the document because it is the
	// half that cannot be seen in Home Assistant when it is wrong: an
	// under-retraction is refused entities and one WARNING line.
	Superseded []string       `json:"superseded"`
	Document   map[string]any `json:"document"`
}

// buildBundleSurface renders one scenario's device documents through the
// PRODUCTION path — Discovery.RenderBundles / RenderScheduleBundles, the very
// calls Coordinator.buildBundles makes — and never by reading anything back.
func buildBundleSurface(t *testing.T, sc surfaceScenario) bundleDoc {
	t.Helper()
	in := buildHamqttInputs(t, sc)
	prefix := config.DefaultHASSBaseTopic

	bundles, err := in.disc.RenderBundles(prefix, goldenBundleSW, in.points, in.infos, in.climateInfos)
	if err != nil {
		t.Fatalf("RenderBundles: %v", err)
	}
	scheds, err := in.disc.RenderScheduleBundles(prefix, goldenBundleSW, in.schedules, in.configURL)
	if err != nil {
		t.Fatalf("RenderScheduleBundles: %v", err)
	}
	bundles = append(bundles, scheds...)

	doc := bundleDoc{Scenario: sc.name}
	for _, b := range bundles {
		raw, err := json.Marshal(b.Bundle)
		if err != nil {
			t.Fatalf("marshal bundle: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode bundle: %v", err)
		}
		doc.Documents = append(doc.Documents, bundleFile{
			Topic:      b.Topic,
			Superseded: publisher.SupersededTopics(prefix, b.Bundle, hass.LegacyConfigTopicForms()...),
			Document:   body,
		})
	}
	sort.Slice(doc.Documents, func(i, j int) bool { return doc.Documents[i].Topic < doc.Documents[j].Topic })
	return doc
}

// TestDeviceBundleGolden pins every device document this bridge publishes.
//
//nolint:tparallel // the subtests append to `printed` and the parent reports it
func TestDeviceBundleGolden(t *testing.T) {
	var printed []string
	for name := range bundleDigests {
		if !slices.ContainsFunc(surfaceScenarios(), func(sc surfaceScenario) bool { return sc.name == name }) {
			t.Errorf("bundleDigests has %q, which is no scenario — a digest that pins nothing", name)
		}
	}
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			got := buildBundleSurface(t, sc)
			path := filepath.Join("testdata", "bundle", sc.name+".json")

			if *updateBundleGolden {
				writeBundleGolden(t, path, got)
				printed = append(printed, fmt.Sprintf("\t%q: %q,", sc.name, canonicalBundleDigest(t, got)))
				return
			}
			want := readBundleGolden(t, path)
			if diff := diffBundles(want, got); diff != "" {
				t.Errorf("device documents changed for %s:\n%s", sc.name, diff)
			}
			wantDigest, ok := bundleDigests[sc.name]
			if !ok {
				t.Fatalf("no digest pinned for scenario %q — add it to bundleDigests", sc.name)
			}
			if d := canonicalBundleDigest(t, got); d != wantDigest {
				t.Errorf("scenario %q digest %s, pinned %s", sc.name, d, wantDigest)
			}
		})
	}
	if *updateBundleGolden && len(printed) > 0 {
		t.Errorf("bundle goldens rewritten; paste these into bundleDigests:\n%s", strings.Join(printed, "\n"))
	}
}

// TestTheRetractionCoversTheWholePinnedFleet is the assertion the whole
// migration rests on, and the one that keeps the twelve per-entity goldens
// load-bearing now that the live path no longer produces them.
//
// A per-entity config retained for a unique_id and a device document carrying
// the same id cannot coexist. So the set publisher.SupersededTopics derives
// from the documents must be exactly the set of config topics this bridge has
// published since 0.1 — no fewer (one missed topic costs that device its whole
// entity set, with one WARNING line in Home Assistant's log as the only
// evidence) and no more (a topic this bridge never published is somebody
// else's).
//
// The two sides come from genuinely different places: the want side is the
// pinned golden FILE, the got side is the library rendering the model. Nothing
// compares them but this test, which is exactly the shape — a value spelled
// twice with nothing comparing the spellings — that let go-mtec2mqtt's
// composition root drop its legacy topic form unnoticed.
func TestTheRetractionCoversTheWholePinnedFleet(t *testing.T) {
	t.Parallel()
	// atomic, not a plain int: the subtests are parallel, and a package-level
	// or closure counter written from several of them is the CI-only failure
	// step 5 shipped and -race caught on all three runners.
	var total atomic.Int64
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			want := map[string]bool{}
			for topic := range configsOf(surfaceOf(t, sc.name)) {
				want[topic] = true
			}
			if len(want) == 0 {
				t.Fatal("the per-entity golden pins no config topics")
			}
			got := map[string]bool{}
			for _, d := range buildBundleSurface(t, sc).Documents {
				for _, topic := range d.Superseded {
					if got[topic] {
						t.Errorf("%s is retracted by two documents", topic)
					}
					got[topic] = true
				}
			}
			for topic := range want {
				if !got[topic] {
					t.Errorf("no document retracts %s — Home Assistant will refuse its device's whole document", topic)
				}
			}
			for topic := range got {
				if !want[topic] {
					t.Errorf("%s is retracted but was never published by this bridge", topic)
				}
			}
			total.Add(int64(len(want)))
		})
	}
	t.Cleanup(func() {
		t.Logf("retraction covers %d pinned per-entity config topics", total.Load())
	})
}

// TestTheRuledOutLegacyFormsRetractNothing is the other half of the same proof,
// and it is what makes the one above non-vacuous.
//
// publisher.Config.LegacyEntityTopics REPLACES the library's default. Saying
// nothing yields the five-segment publisher.LegacyTopicWithNodeID, and a
// consumer who says nothing gets a migration that retracts nothing, publishes
// every document into a live conflict, and shows no entities. Measured here
// rather than argued: both ruled-out forms reproduce ZERO of the pinned topics,
// so the verdict cannot be an accident of a form that happens to overlap.
func TestTheRuledOutLegacyFormsRetractNothing(t *testing.T) {
	t.Parallel()
	for _, form := range []struct {
		name string
		fn   publisher.LegacyTopicFunc
	}{
		{"LegacyTopicWithNodeID (the library default)", publisher.LegacyTopicWithNodeID},
		{"LegacyTopicByObjectID", publisher.LegacyTopicByObjectID},
	} {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			hits, seen := 0, 0
			for _, sc := range surfaceScenarios() {
				want := configsOf(surfaceOf(t, sc.name))
				in := buildHamqttInputs(t, sc)
				prefix := config.DefaultHASSBaseTopic
				bundles, err := in.disc.RenderBundles(prefix, goldenBundleSW, in.points, in.infos, in.climateInfos)
				if err != nil {
					t.Fatal(err)
				}
				for _, b := range bundles {
					for _, topic := range publisher.SupersededTopics(prefix, b.Bundle, form.fn) {
						seen++
						if want[topic] != nil {
							hits++
						}
					}
				}
			}
			if seen == 0 {
				t.Fatal("the form produced no topics at all; this test would pass vacuously")
			}
			if hits != 0 {
				t.Errorf("%s reproduces %d of the pinned config topics, want 0", form.name, hits)
			}
		})
	}
}

// TestBundleOriginIsWiredFromTheBuildVersion pins the one thing the frozen
// artefact cannot: that the origin block the daemon actually publishes carries
// this build's version rather than the golden's placeholder.
//
// discovery.Validate makes origin.name a BLOCKING issue on a device document
// (and only advisory on a per-entity config), so this is also where F12 of the
// phase 8 measurement lands — deliberately here and not inside step 4's
// byte-equality proof, where it would have been a 264-row payload addition.
func TestBundleOriginIsWiredFromTheBuildVersion(t *testing.T) {
	t.Parallel()
	got := hass.BundleOrigin(version.Version)
	if got.Name != hass.OriginName || got.URL != hass.OriginURL {
		t.Errorf("origin = %+v, want name %q and url %q", got, hass.OriginName, hass.OriginURL)
	}
	if got.SW != version.Version || got.SW == "" {
		t.Errorf("origin.sw_version = %q, want the build version %q", got.SW, version.Version)
	}
	if got.SW == goldenBundleSW {
		t.Error("the daemon publishes the golden's placeholder version")
	}
}

// TestEveryPublishedBundleValidates runs Home Assistant's own discovery schemas
// over every document, through the PRODUCTION render path.
//
// A blocking finding publishes nothing at all, so a fault that costs one entity
// on the per-entity form costs a device all of its entities on a document. This
// is the gate Coordinator.buildBundles applies before anything is retracted;
// here it is asserted over the whole measured fleet.
func TestEveryPublishedBundleValidates(t *testing.T) {
	t.Parallel()
	documents, components := 0, 0
	prefix := config.DefaultHASSBaseTopic
	for _, sc := range surfaceScenarios() {
		in := buildHamqttInputs(t, sc)
		bundles, err := in.disc.RenderBundles(prefix, goldenBundleSW, in.points, in.infos, in.climateInfos)
		if err != nil {
			t.Fatal(err)
		}
		scheds, err := in.disc.RenderScheduleBundles(prefix, goldenBundleSW, in.schedules, in.configURL)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range append(bundles, scheds...) {
			documents++
			components += len(b.Bundle.Components)
			if err := discovery.Validate(b.Bundle); err != nil {
				t.Errorf("%s: %v", b.Topic, err)
			}
		}
	}
	if documents == 0 || components == 0 {
		t.Fatal("nothing validated")
	}
	t.Logf("validated %d device documents carrying %d components", documents, components)
}

// TestBundleDocumentsFitCommonBrokerLimits is the measurement behind
// Coordinator.bundleFits, taken rather than assumed.
//
// The preflight matters because go-mqtt refuses an oversized packet from its
// own write path — after publisher.Runtime.PublishBundle has already superseded
// every per-entity config the document carries. This records how much headroom
// there actually is, and fails if a document ever grows past a limit a hardened
// broker plausibly sets.
func TestBundleDocumentsFitCommonBrokerLimits(t *testing.T) {
	t.Parallel()
	// MQTT's own smallest interesting ceiling: a broker configured
	// `max_packet_size 65535`. Nothing this bridge renders comes close, and a
	// change that took it there is a change that needs a person to look.
	const hardenedBrokerLimit = 65535
	documents, largest, largestTopic := 0, 0, ""
	for _, sc := range surfaceScenarios() {
		for _, d := range buildBundleSurface(t, sc).Documents {
			raw, err := json.Marshal(d.Document)
			if err != nil {
				t.Fatal(err)
			}
			size := len(raw) + len(d.Topic) + bundlePublishOverhead
			documents++
			if size > largest {
				largest, largestTopic = size, d.Topic
			}
		}
	}
	if documents == 0 {
		t.Fatal("no documents measured")
	}
	if largest >= hardenedBrokerLimit {
		t.Errorf("the largest device document is %d bytes (%s) and no longer fits a broker capped at %d",
			largest, largestTopic, hardenedBrokerLimit)
	}
	t.Logf("%d device documents, largest packet %d bytes (%s)", documents, largest, largestTopic)
}

// --- golden plumbing --------------------------------------------------------

func canonicalBundleDigest(t *testing.T, doc bundleDoc) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeBundleGolden(t *testing.T, path string, doc bundleDoc) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("encode golden: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func readBundleGolden(t *testing.T, path string) bundleDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle golden (regenerate with -update-bundle-golden): %v", err)
	}
	var doc bundleDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode bundle golden: %v", err)
	}
	return doc
}

// diffBundles compares two document sets on canonical re-encoding, per topic,
// naming what moved rather than dumping two 14 KB documents.
func diffBundles(want, got bundleDoc) string {
	var b strings.Builder
	wi := indexBundles(want.Documents)
	gi := indexBundles(got.Documents)
	for _, k := range sortedKeys(wi) {
		if _, ok := gi[k]; !ok {
			fmt.Fprintf(&b, "- missing document: %s\n", k)
		}
	}
	for _, k := range sortedKeys(gi) {
		if _, ok := wi[k]; !ok {
			fmt.Fprintf(&b, "+ unexpected document: %s\n", k)
			continue
		}
		if wi[k] != gi[k] {
			fmt.Fprintf(&b, "~ %s\n    want %s\n    got  %s\n", k, wi[k], gi[k])
		}
	}
	return b.String()
}

// indexBundles keys each document by topic, and each of its components by
// component key, so a diff names the component that moved rather than the
// device.
func indexBundles(docs []bundleFile) map[string]string {
	out := map[string]string{}
	for _, d := range docs {
		sup, err := json.Marshal(d.Superseded)
		if err == nil {
			out[d.Topic+" [superseded]"] = string(sup)
		}
		for key, v := range d.Document {
			if key != "components" {
				b, err := json.Marshal(v)
				if err != nil {
					continue
				}
				out[d.Topic+" ."+key] = string(b)
				continue
			}
			comps, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for ck, cv := range comps {
				b, err := json.Marshal(cv)
				if err != nil {
					continue
				}
				out[d.Topic+" #"+ck] = string(b)
			}
		}
	}
	return out
}
