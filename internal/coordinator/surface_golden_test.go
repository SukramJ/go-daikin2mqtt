// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// The published-surface pin (ADR 0070, phase 8 step 0).
//
// These goldens are the compatibility contract for everything this bridge puts
// on a broker: for every scenario, every MQTT publish the real publish path
// makes — the HA discovery config topics AND the state/command/attribute topics
// — together with its QoS and retain flag, and the payload stored DECODED (a
// JSON object for a discovery config, the literal string for a state payload).
//
// Rules that make the pin load-bearing rather than decorative:
//
//   - The messages are produced by the real builders (Coordinator.pollOnce ->
//     hass.Discovery.Publish / PublishSchedules / the coordinator's own inline
//     state-topic formatting) against the real characteristics.yaml and the
//     real ONECTA fixtures. Nothing here is a stub except the cloud transport
//     and the broker.
//   - Comparison is on CANONICAL RE-ENCODING, not on file bytes: the golden is
//     re-marshalled through encoding/json (which sorts object keys) and so is
//     the freshly built message. Whitespace, key order and CRLF cannot make a
//     run pass or fail. (The repo also pins eol=lf via .gitattributes, so a
//     Windows checkout does not perturb the digests below either.)
//   - goldenDigests holds a SHA-256 per scenario OUTSIDE the golden files and
//     is NOT rewritten by -update-surface-golden. Regenerating the goldens
//     therefore fails until a human copies the printed digest in, so a silent
//     regeneration cannot pass unnamed.
//
// Regenerate with:
//
//	go test ./internal/coordinator -run TestPublishedSurfaceGolden -update-surface-golden
//
// then paste the printed digests into goldenDigests.
var updateSurfaceGolden = flag.Bool("update-surface-golden", false,
	"rewrite the published-surface goldens under testdata/surface (digests must then be updated by hand)")

// goldenDigests is the SHA-256 of each scenario's canonical encoding, held
// deliberately OUTSIDE testdata so that regenerating a golden cannot silently
// re-bless a changed surface. -update-surface-golden never writes this map.
var goldenDigests = map[string]string{
	"air-to-air-dx4.en":             "d883f6e9ab20085ee3aca056f917cc05b16ca0fe86a0006df2e25f9187f3b2f4",
	"air-to-air-dx4.de":             "cb5a0071429d696479892f364f6bed1d8579fa4767739d5fe79c951a60004a19",
	"airpurifier.en":                "9232bf061598ea96a7f59d4eb7467a3cd618679e1e672ddd7fc8f8e531a8056f",
	"airpurifier.de":                "7c43bb3b324c081b819d8a6412daec7c970a1531c18a46439086f45751919732",
	"altherma-air-to-water-wlan.en": "d97e6bfb90b073a0a84347d2a66bb719a2dd89c8951704b111169e1abe5a5d4d",
	"altherma-air-to-water-wlan.de": "dc81ae70db58df36f4690333eae968b8a295fb00eeb2c5d3f4163264b714105b",
	"d2cnd-gas-boiler.en":           "6b483b00cc3f220b3bcbc3251377f67161338a7a9646210cd8206292b2ad9d86",
	"d2cnd-gas-boiler.de":           "5139888de2ac08f7c03175c209010608780e51a375f59a86a4c35922d905f55e",
	"multisplit.en":                 "94ff2a7b98d04de02a1992bb72604ab076fdc5906d4709e41a150d7e49a9c7a1",
	"multisplit.de":                 "1a8f2e63d3ae2fe1a73b79f5c8faa904b2dd33452a534baba8f1a2d2b1ec4c1a",
	"multisplit.local.en":           "3808b86a5f27262d46c2210f69c71acd159510ccf9ea2b275609c37bb6b81f17",
	"multisplit.scheduler.en":       "8b26fd8fef9a3ec0dc0caaaeb7e96d9edb70bf4d6bcfb746a8c4f8d7ea31cf1b",
	"air-to-air-dx4.scheduler.en":   "4d1ad09e820aa501b871a053e9df2e1be36de993e1006b7637c2d389d929a933",
}

// --- recording broker ------------------------------------------------------

// recordedMsg is one publish exactly as the bridge made it.
type recordedMsg struct {
	Topic  string `json:"topic"`
	QoS    int    `json:"qos"`
	Retain bool   `json:"retain"`
	// Exactly one of JSON / Text / Empty is set. JSON holds a decoded discovery
	// config (or attributes document); Text a scalar state payload; Empty marks
	// a zero-length payload (a retained-config clear).
	JSON  map[string]any `json:"json,omitempty"`
	Text  *string        `json:"text,omitempty"`
	Empty bool           `json:"empty,omitempty"`
}

// surfaceDoc is one scenario's golden.
type surfaceDoc struct {
	Scenario string        `json:"scenario"`
	Messages []recordedMsg `json:"messages"`
}

// recorderMQTT implements mqtt.Client and records every publish in order,
// including QoS and retain. Subscribe accepts and delivers nothing, which is
// what a broker with no retained state does.
type recorderMQTT struct {
	mu   sync.Mutex
	msgs []recordedMsg
	subs []string
}

func (r *recorderMQTT) Publish(_ context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	m := recordedMsg{Topic: topic, QoS: int(qos), Retain: retain}
	switch {
	case len(payload) == 0:
		m.Empty = true
	default:
		var obj map[string]any
		if json.Unmarshal(payload, &obj) == nil && obj != nil {
			m.JSON = obj
		} else {
			s := string(payload)
			m.Text = &s
		}
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return nil
}

func (r *recorderMQTT) Subscribe(_ context.Context, filter string, _ mqtt.QoS, _ mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	r.mu.Lock()
	r.subs = append(r.subs, filter)
	r.mu.Unlock()
	return mqtt.SubscribeResult{}, nil
}

func (r *recorderMQTT) Unsubscribe(_ context.Context, _ string) error { return nil }

// snapshot returns the recorded messages sorted by topic (stable, so repeated
// publishes to one topic keep their order) — the surface is a set of retained
// topics, and sorting removes goroutine-scheduling noise from the pin.
//
// Byte-identical repeats are collapsed: the harness runs two poll cycles (the
// first fills the caches the synthetic points hang off), so every steady-state
// topic is written twice with the same payload. A repeat that differs in
// payload, QoS or retain is NOT collapsed — that is a real surface fact.
func (r *recorderMQTT) snapshot() []recordedMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedMsg, 0, len(r.msgs))
	seen := map[string]bool{}
	for _, m := range r.msgs {
		b, err := json.Marshal(m)
		if err == nil {
			if seen[string(b)] {
				continue
			}
			seen[string(b)] = true
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// --- scenarios -------------------------------------------------------------

// surfaceScenario is one measured installation.
type surfaceScenario struct {
	name    string
	fixture string // path to a GetDevices response
	lang    string
	local   bool
	devMap  map[string]string
	sched   *schedule.Document
}

// goldenClock is fixed so the energy resolver's month bucket (and anything else
// time-dependent) cannot make the pin flap.
var goldenClock = time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

func surfaceScenarios() []surfaceScenario {
	const pd = "../process/testdata/"
	return []surfaceScenario{
		{name: "air-to-air-dx4.en", fixture: pd + "air-to-air-dx4.json", lang: "en"},
		{name: "air-to-air-dx4.de", fixture: pd + "air-to-air-dx4.json", lang: "de"},
		{name: "altherma-air-to-water-wlan.en", fixture: pd + "altherma-air-to-water-wlan.json", lang: "en"},
		{name: "altherma-air-to-water-wlan.de", fixture: pd + "altherma-air-to-water-wlan.json", lang: "de"},
		{name: "d2cnd-gas-boiler.en", fixture: pd + "d2cnd-gas-boiler.json", lang: "en"},
		{name: "d2cnd-gas-boiler.de", fixture: pd + "d2cnd-gas-boiler.json", lang: "de"},
		{name: "airpurifier.en", fixture: pd + "airpurifier.json", lang: "en"},
		{name: "airpurifier.de", fixture: pd + "airpurifier.json", lang: "de"},
		// Two indoor units sharing one outdoor serial: the only shape that
		// exercises the scope:outdoor dedup and the shared sub-device blocks.
		{name: "multisplit.en", fixture: "testdata/multisplit-two-indoor.json", lang: "en"},
		{name: "multisplit.de", fixture: "testdata/multisplit-two-indoor.json", lang: "de"},
		// Local-first mode: adds the localOnlyPoints synthetic entities.
		{
			name: "multisplit.local.en", fixture: "testdata/multisplit-two-indoor.json", lang: "en",
			local: true,
			devMap: map[string]string{
				"809d41d9-4d42-45fa-af6a-84b512143672": "Klima WZ",
				"11112222-3333-4444-5555-666677778888": "Klima KU",
			},
		},
		// Scheduler on: adds the per-schedule switches on the daemon device and
		// the per-device schedule_state / schedule_next_change sensors.
		{name: "multisplit.scheduler.en", fixture: "testdata/multisplit-two-indoor.json", lang: "en", sched: goldenScheduleDoc()},
	}
}

// The two ids the scheduler scenario drives its schedule-state plane from.
const (
	goldenSchedDeviceID      = "809d41d9-4d42-45fa-af6a-84b512143672"
	goldenSchedOutdoorSerial = "ODU0000000001"
)

// goldenFaikinState is one fixed Faikin AC document, so the local read path
// publishes a complete, deterministic set of per-unit values.
func goldenFaikinState() *faikin.State {
	return &faikin.State{
		Host: "Klima", HasAC: true, Online: true,
		Power: true, Mode: "heat", Fan: "3", Swing: "V",
		Quiet: true, Econo: false, Powerful: false, Streamer: true, Comfort: false,
		Target: 21.5, Temp: 20.5, Hum: 44, Outside: 4.5, Liquid: 31.5, Demand: 80,
		Consumption: 610, Comp: 42, FanFreq: 14.5,
		Energy: 1234500, EnergyHeat: 900000, EnergyCool: 334500,
		IPv4: "192.168.0.31",
	}
}

// goldenScheduleDoc is a fixed two-schedule programme (one indoor, one outdoor)
// so the scheduler scenario covers both schedule types' discovery.
func goldenScheduleDoc() *schedule.Document {
	doc := schedule.NewDocument()
	doc.Schedules = append(doc.Schedules,
		schedule.Schedule{ID: "werktag", Name: "Werktag", Type: schedule.TypeIndoor},
		schedule.Schedule{ID: "nacht_leise", Name: "Nacht leise", Type: schedule.TypeOutdoor},
	)
	return doc
}

// buildSurface runs one scenario through the real publish path and returns the
// recorded messages.
func buildSurface(t *testing.T, sc surfaceScenario) []recordedMsg {
	t.Helper()

	raw, err := os.ReadFile(sc.fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	cat, err := catalog.LoadFile("../../characteristics.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	rec := &recorderMQTT{}
	cfg := &config.Config{
		MQTTTopic:     config.TopicRoot,
		HASSBaseTopic: config.DefaultHASSBaseTopic,
		Language:      sc.lang,
	}
	if sc.local {
		cfg.LocalMode = true
		cfg.LocalDeviceMap = sc.devMap
	}
	c := New(Deps{
		Cfg:     cfg,
		Client:  &stubCloud{devices: raw},
		MQTT:    rec,
		Catalog: cat,
		HASS:    hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, rec),
		Logger:  slog.New(slog.DiscardHandler),
		Clock:   func() time.Time { return goldenClock },
	})
	if sc.local {
		c.deps.FaikinMQTT = rec
	}
	if sc.sched != nil {
		c.AttachScheduler(&stubScheduler{doc: sc.sched})
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The bridge announces availability once at startup and then polls. Two
	// polls: the first populates the embedded-id / mode caches the synthetic
	// points (refresh button, schedule sensors, local-only entities) hang off,
	// the second therefore publishes the complete steady-state surface.
	c.PublishOnline(ctx)
	c.pollOnce(ctx)
	c.pollOnce(ctx)
	if sc.local {
		// The Faikin read path owns the per-unit topics for a mapped device —
		// the cloud poll skips them (localTopics). Feed each mapped device one
		// full AC state so the pinned surface carries what that path publishes
		// too, instead of leaving 22 advertised topics looking dead.
		ids := slices.Sorted(maps.Keys(sc.devMap))
		for _, id := range ids {
			c.publishLocalState(ctx, id, goldenFaikinState())
		}
	}
	if sc.sched != nil {
		// The scheduler's own state plane does not run from the poll loop: the
		// engine drives it. Call the two entry points directly so the pinned
		// surface holds the topics the schedule configs advertise.
		c.PublishScheduleSwitches(ctx, sc.sched)
		c.PublishScheduleState(ctx, schedule.Target{DeviceID: goldenSchedDeviceID}, schedule.DeviceState{})
		c.PublishScheduleState(ctx, schedule.Target{OutdoorSerial: goldenSchedOutdoorSerial}, schedule.DeviceState{})
	}
	return rec.snapshot()
}

// --- the pin ---------------------------------------------------------------

func TestPublishedSurfaceGolden(t *testing.T) {
	t.Parallel()

	var printed []string
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			got := surfaceDoc{Scenario: sc.name, Messages: buildSurface(t, sc)}
			path := filepath.Join("testdata", "surface", sc.name+".json")

			if *updateSurfaceGolden {
				writeGolden(t, path, got)
				printed = append(printed, fmt.Sprintf("\t%q: %q,", sc.name, canonicalDigest(t, got)))
				return
			}

			want := readGolden(t, path)
			if diff := diffSurface(want, got); diff != "" {
				t.Errorf("published surface changed for %s:\n%s", sc.name, diff)
			}
			// The digest lives outside the golden, so a regenerated golden
			// still fails here until the digest is updated deliberately.
			wantDigest, ok := goldenDigests[sc.name]
			if !ok {
				t.Fatalf("no digest pinned for scenario %q — add it to goldenDigests", sc.name)
			}
			if d := canonicalDigest(t, got); d != wantDigest {
				t.Errorf("scenario %q digest %s, pinned %s", sc.name, d, wantDigest)
			}
		})
	}
	if *updateSurfaceGolden && len(printed) > 0 {
		t.Errorf("goldens rewritten; paste these into goldenDigests:\n%s", strings.Join(printed, "\n"))
	}
}

// canonicalDigest is the SHA-256 over the canonical (sorted-key) JSON encoding
// of a scenario, so it is independent of file formatting and line endings.
func canonicalDigest(t *testing.T, doc surfaceDoc) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeGolden(t *testing.T, path string, doc surfaceDoc) {
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

func readGolden(t *testing.T, path string) surfaceDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-surface-golden): %v", err)
	}
	var doc surfaceDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return doc
}

// diffSurface compares two surfaces on canonical re-encoding, per message, and
// reports the first differences in a readable form.
func diffSurface(want, got surfaceDoc) string {
	var b strings.Builder
	wi := indexSurface(want.Messages)
	gi := indexSurface(got.Messages)

	for _, k := range sortedKeys(wi) {
		if _, ok := gi[k]; !ok {
			fmt.Fprintf(&b, "- missing message: %s\n", k)
		}
	}
	for _, k := range sortedKeys(gi) {
		if _, ok := wi[k]; !ok {
			fmt.Fprintf(&b, "+ unexpected message: %s\n", k)
			continue
		}
		if wi[k] != gi[k] {
			fmt.Fprintf(&b, "~ %s\n    want %s\n    got  %s\n", k, wi[k], gi[k])
		}
	}
	return b.String()
}

// indexSurface keys each message by topic + occurrence and values it by its
// canonical encoding (payload, QoS and retain together).
func indexSurface(msgs []recordedMsg) map[string]string {
	out := make(map[string]string, len(msgs))
	seen := map[string]int{}
	for _, m := range msgs {
		key := m.Topic
		if n := seen[m.Topic]; n > 0 {
			key = fmt.Sprintf("%s#%d", m.Topic, n)
		}
		seen[m.Topic]++
		b, err := json.Marshal(m)
		if err != nil {
			out[key] = "<unencodable>"
			continue
		}
		out[key] = string(b)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loadRealCatalog loads the shipped characteristics.yaml — the real catalogue,
// never a test fixture.
func loadRealCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.LoadFile("../../characteristics.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}
