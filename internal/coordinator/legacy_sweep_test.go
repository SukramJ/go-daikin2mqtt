// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// TestClassifyRetainedTouchesOnlyExactOwnedShapes is the sweep's decision,
// topic by topic, without a broker: exact 0.13 shapes for identifiers this
// instance owns are cleared, a current-layout status item of an owned device
// that this run does not publish is cleared, and nothing else — not a sibling's
// device, not a near-miss of an old shape, not anything whose second level is a
// function name, not another root, not the Faikin firmware's own topics.
func TestClassifyRetainedTouchesOnlyExactOwnedShapes(t *testing.T) {
	t.Parallel()
	// dev1 is served by the cloud, dev3 by a Faikin module (local mode).
	scope := sweepScope{
		Owned:            map[string]bool{"dev1": true, "dev3": true},
		Schedules:        map[string]bool{"werktag": true},
		Current:          map[string]bool{"daikin/status/dev1/climateControl/power": true},
		CloudServed:      map[string]bool{"dev1": true},
		OtherOwnerLeaves: map[string]bool{"schedule_state": true},
	}

	for _, tc := range []struct {
		topic string
		want  sweepVerdict
	}{
		// The 0.13 shapes of an owned device.
		{"daikin/dev1/climateControl/power/state", sweepLegacy},
		{"daikin/dev1/climateControl/power/set", sweepLegacy},
		{"daikin/dev1/climateControl/power/attributes", sweepLegacy},
		{"daikin/dev1/climateControl/climate/attributes", sweepLegacy},
		{"daikin/dev1/gateway/gateway_ip_address/state", sweepLegacy},
		{"daikin/bridge/status", sweepLegacy},
		{"daikin/scheduler/werktag/enabled/state", sweepLegacy},
		{"daikin/scheduler/werktag/enabled/set", sweepLegacy},

		// A sibling's device, a schedule this instance does not hold.
		{"daikin/dev2/climateControl/power/state", sweepKeep},
		{"daikin/scheduler/urlaub/enabled/state", sweepKeep},
		// Near-misses of an old shape are never a prefix match.
		{"daikin/dev1/climateControl/power", sweepKeep},
		{"daikin/dev1/climateControl/power/state/extra", sweepKeep},
		{"daikin/dev1/climateControl/power/online", sweepKeep},
		{"daikin/dev1/climateControl//state", sweepKeep},
		{"daikin/bridge/info", sweepKeep},
		{"daikin/bridge", sweepKeep},
		{"daikin/scheduler/werktag/other/state", sweepKeep},

		// The current layout: a function name at the second level.
		{"daikin/connected", sweepKeep},
		{"daikin/info", sweepKeep},
		{"daikin/maintenance/stats", sweepKeep},
		{"daikin/set/dev1/climateControl/power", sweepKeep},
		{"daikin/status/dev1/climateControl/power", sweepKeep},     // published this run
		{"daikin/status/dev1/climateControl/old_leaf", sweepStale}, // owned, no longer published
		{"daikin/status/dev1/climateControl/old_leaf/attributes", sweepStale},
		// The stale path never touches what the cloud poll does not write:
		// a device's online item (every entity's availability rests on it),
		// a Faikin-served device's items (its module may not have reported
		// yet), the scheduler's own sensors.
		{"daikin/status/dev1/online", sweepKeep},
		{"daikin/status/dev3/online", sweepKeep},
		{"daikin/status/dev3/climateControl/power_consumption", sweepKeep},
		{"daikin/status/dev3/climateControl/old_leaf", sweepKeep},
		{"daikin/status/dev1/climateControl/schedule_state", sweepKeep},
		{"daikin/dev3/climateControl/power/state", sweepLegacy}, // the 0.13 tree is still the sweep's job
		{"daikin/status/dev2/climateControl/power", sweepKeep},  // a sibling's current item
		{"daikin/status/scheduler/werktag/enabled", sweepKeep},  // no device of ours
		{"daikin/status/dev1", sweepKeep},

		// Not under this root at all.
		{"daikin2/dev1/climateControl/power/state", sweepKeep},
		{"state/Klima SZ", sweepKeep},
		{"command/Klima SZ/power", sweepKeep},
		{"homeassistant/device/daikin_dev1/config", sweepKeep},
	} {
		if got := classifyRetained("daikin", tc.topic, scope); got != tc.want {
			t.Errorf("classifyRetained(%q) = %v, want %v", tc.topic, got, tc.want)
		}
	}

	// A root named like the Faikin firmware's first level still leaves the
	// firmware's own topics alone: a Faikin host is no ONECTA device id.
	for _, topic := range []string{"state/Klima SZ", "command/Klima SZ/power", "state/dev1"} {
		if got := classifyRetained("state", topic, scope); got != sweepKeep {
			t.Errorf("root \"state\": classifyRetained(%q) = %v, want keep", topic, got)
		}
		if got := classifyRetained("command", topic, scope); got != sweepKeep {
			t.Errorf("root \"command\": classifyRetained(%q) = %v, want keep", topic, got)
		}
	}
}

// TestLegacySweepClearsOnlyThisInstancesOldTopics drives the real sweep against
// a broker holding what an upgrade actually finds: this instance's 0.13 tree, a
// sibling instance's tree on the same root (old and new layout), another root,
// the Faikin firmware's own retained state, and a current status item this run
// no longer publishes. Only this instance's old shapes and its stale item go.
func TestLegacySweepClearsOnlyThisInstancesOldTopics(t *testing.T) {
	t.Parallel()
	const dev, emb = "dev1", "climateControl"
	br := newRetainedBroker()

	ours := []string{
		"daikin/dev1/climateControl/power/state",
		"daikin/dev1/climateControl/power/attributes",
		"daikin/dev1/climateControl/operation_mode/state",
		"daikin/dev1/climateControl/climate/attributes",
		"daikin/dev1/climateControl/hvac_mode/state",
		"daikin/bridge/status",
		"daikin/scheduler/werktag/enabled/state",
	}
	survivors := map[string]string{
		// A sibling go-daikin2mqtt instance on the same root and account
		// family, polling another device — both layouts.
		"daikin/dev2/climateControl/power/state":  "on",
		"daikin/dev2/climateControl/power/set":    "on",
		"daikin/status/dev2/climateControl/power": `{"val":true,"ts":1,"lc":1}`,
		"daikin/status/dev2/online":               `{"val":true,"ts":1,"lc":1}`,
		// A schedule this instance does not hold.
		"daikin/scheduler/urlaub/enabled/state": "off",
		// Another root entirely.
		"daikin2/dev1/climateControl/power/state": "on",
		// The Faikin firmware's own topics.
		"state/Klima SZ":         `{"power":true}`,
		"command/Klima SZ/power": "true",
		// A near-miss of an old shape.
		"daikin/dev1/climateControl/power/state/x": "x",
	}
	for _, topic := range ours {
		br.retain(topic, "on")
	}
	for topic, payload := range survivors {
		br.retain(topic, payload)
	}
	const stale = "daikin/status/dev1/climateControl/old_leaf"
	br.retain(stale, `{"val":1,"ts":1,"lc":1}`)

	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{devices: devicesJSON(dev, emb)}, MQTT: br,
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.collectWindow = 20 * time.Millisecond
	c.AttachScheduler(&stubScheduler{doc: &schedule.Document{Schedules: []schedule.Schedule{{ID: "werktag", Name: "Werktag"}}}})
	ctx := context.Background()

	c.pollOnce(ctx)
	c.maybeSweepLegacy(ctx)

	for _, topic := range append(ours, stale) {
		if v, ok := br.retained(topic); !ok || v != "" {
			t.Errorf("%s = %q (present %v), want cleared", topic, v, ok)
		}
	}
	for topic, payload := range survivors {
		if v, _ := br.retained(topic); v != payload {
			t.Errorf("%s = %q, want it untouched (%q)", topic, v, payload)
		}
	}
	// What this run publishes is not swept.
	if v, _ := br.retained("daikin/status/dev1/climateControl/power"); v == "" {
		t.Error("the sweep cleared a status item this run publishes")
	}

	// Subscribed only to the subtrees that can hold this instance's topics —
	// never "daikin/#", which would overlap the live set subscription.
	subs := br.log()
	if slices.Contains(subs, "sub daikin/#") {
		t.Errorf("the sweep subscribed the whole root: %v", subs)
	}
	for _, want := range []string{"sub daikin/dev1/#", "sub daikin/status/dev1/#", "sub daikin/bridge/#", "sub daikin/scheduler/#"} {
		if !slices.Contains(subs, want) {
			t.Errorf("the sweep did not subscribe %q: %v", want, subs)
		}
	}
	for _, s := range subs {
		if len(s) > 4 && s[:4] == "sub " && !slices.Contains(subs, "unsub "+s[4:]) {
			t.Errorf("%q was never unsubscribed", s[4:])
		}
	}

	// Once per process: a second poll does not open another window.
	before := len(br.log())
	c.pollOnce(ctx)
	c.maybeSweepLegacy(ctx)
	if len(br.log()) != before {
		t.Errorf("the sweep ran again: %v", br.log()[before:])
	}
	// And idempotent: run directly again, it finds nothing left to clear.
	sent := br.publishes()
	c.sweepLegacyTopics(ctx, []string{dev})
	if got := br.publishOrder()[sent:]; len(got) != 0 {
		t.Errorf("a repeated sweep published %v", got)
	}
}

// TestLegacySweepWaitsForTheFirstPicture pins that nothing is swept before a
// poll has resolved the devices this instance owns: ownership that cannot be
// proven is not claimed.
func TestLegacySweepWaitsForTheFirstPicture(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	br.retain("daikin/bridge/status", "online")
	c := New(Deps{
		Cfg: testConfig(), Client: &stubCloud{getErr: errors.New("cloud down")}, MQTT: br,
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.collectWindow = 5 * time.Millisecond
	c.pollOnce(context.Background())
	c.maybeSweepLegacy(context.Background())
	if v, _ := br.retained("daikin/bridge/status"); v != "online" {
		t.Errorf("swept before any device was known: bridge/status = %q", v)
	}
	if len(br.log()) != 0 {
		t.Errorf("subscribed before any device was known: %v", br.log())
	}
}

// TestLegacySweepLeavesAFaikinServedDeviceAlone is finding 3 of the 0.14.0
// review: in local mode a mapped device's `online` item and its Faikin-owned
// leaves are written only by the Faikin read path. When no module has reported
// by the time the first poll's sweep runs, 0.14.0's stale path cleared the
// previous run's retained values — and under availability_mode: all a cleared
// `online` makes every entity of the device unavailable until the module's
// next state message. The sweep must leave them, while still clearing the
// device's 0.13 tree.
func TestLegacySweepLeavesAFaikinServedDeviceAlone(t *testing.T) {
	t.Parallel()
	const dev, emb = "dev1", "climateControl"
	br := newRetainedBroker()
	keep := map[string]string{
		"daikin/status/dev1/online":                              `{"val":true,"ts":1,"lc":1}`,
		"daikin/status/dev1/climateControl/power":                `{"val":true,"ts":1,"lc":1}`,
		"daikin/status/dev1/climateControl/power_consumption":    `{"val":120,"ts":1,"lc":1}`,
		"daikin/status/dev1/climateControl/fan_mode":             `{"val":"auto","ts":1,"lc":1}`,
		"daikin/status/dev1/climateControl/power_consumption/xx": `{"val":1,"ts":1,"lc":1}`,
	}
	for topic, payload := range keep {
		br.retain(topic, payload)
	}
	const legacy = "daikin/dev1/climateControl/power/state"
	br.retain(legacy, "on")

	cfg := testConfig()
	cfg.LocalMode = true
	cfg.LocalDeviceMap = map[string]string{dev: "Klima SZ"}
	c := New(Deps{
		Cfg: cfg, Client: &stubCloud{devices: devicesJSON(dev, emb)}, MQTT: br, FaikinMQTT: newStubMQTT(),
		Catalog: loadTestCatalog(t), Logger: slog.New(slog.DiscardHandler), Clock: fixedClock(),
	})
	c.collectWindow = 20 * time.Millisecond
	ctx := context.Background()

	c.pollOnce(ctx) // no Faikin state has arrived
	c.maybeSweepLegacy(ctx)

	for topic, payload := range keep {
		if v, _ := br.retained(topic); v != payload {
			t.Errorf("%s = %q, want it untouched (%q): the Faikin plane has not reported yet", topic, v, payload)
		}
	}
	if v, ok := br.retained(legacy); !ok || v != "" {
		t.Errorf("%s = %q (present %v), want the 0.13 topic cleared", legacy, v, ok)
	}
}
