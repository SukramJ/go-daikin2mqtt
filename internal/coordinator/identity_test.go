// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// identityFile is one device document reduced to what Home Assistant keys its
// registries on: the discovery topic (and with it the node id), the device
// identifiers and parent, and per component its key, platform, unique_id and
// default_entity_id.
type identityFile struct {
	Identifiers []string                     `json:"identifiers"`
	ViaDevice   string                       `json:"via_device"`
	Components  map[string]identityComponent `json:"components"`
}

type identityComponent struct {
	Platform        string `json:"platform"`
	UniqueID        string `json:"unique_id"`
	DefaultEntityID string `json:"default_entity_id"`
}

// TestHAIdentityUnchangedByTheTopicMigration is openccu-loom ADR 0083's
// "unique_id, node ids, device identifiers and the discovery topic form are not
// changed", proved against the release before it.
//
// testdata/identity/v0.13.0.json is the identity projection of the twelve
// device-bundle goldens as v0.13.0 pinned them — extracted once from that
// release's testdata/bundle and never regenerated, because the point is that it
// is the OTHER side of the migration. This renders every scenario through the
// production path today, projects it the same way and requires equality: the
// same discovery topics, the same device identifiers and parents, the same
// component keys, platforms, unique_ids and entity-id seeds.
//
// Home Assistant re-points an existing entity to the new topics because its
// registry keys on unique_id; a single moved string here would instead strand
// the old entity and create a second one, with no migration path.
func TestHAIdentityUnchangedByTheTopicMigration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "identity", "v0.13.0.json"))
	if err != nil {
		t.Fatalf("read frozen identity: %v", err)
	}
	var before map[string]map[string]identityFile
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatalf("decode frozen identity: %v", err)
	}
	scenarios := surfaceScenarios()
	if len(before) != len(scenarios) {
		t.Errorf("frozen identity covers %d scenarios, the surface has %d", len(before), len(scenarios))
	}
	components := 0
	for _, sc := range scenarios {
		want, ok := before[sc.name]
		if !ok {
			t.Errorf("no frozen identity for scenario %q", sc.name)
			continue
		}
		got := identityOf(t, cachedBundleSurface(t, sc))
		if !reflect.DeepEqual(documentTopics(got), documentTopics(want)) {
			t.Errorf("%s: discovery topics changed:\n got %v\nwant %v", sc.name, documentTopics(got), documentTopics(want))
			continue
		}
		for topic, w := range want {
			g := got[topic]
			if !reflect.DeepEqual(g.Identifiers, w.Identifiers) || g.ViaDevice != w.ViaDevice {
				t.Errorf("%s %s: device identity %v via %q, was %v via %q",
					sc.name, topic, g.Identifiers, g.ViaDevice, w.Identifiers, w.ViaDevice)
			}
			if !reflect.DeepEqual(g.Components, w.Components) {
				t.Errorf("%s %s: component identities changed:\n got %v\nwant %v", sc.name, topic, g.Components, w.Components)
			}
			components += len(w.Components)
		}
	}
	// The measured fleet, so an empty projection cannot pass vacuously.
	if components < 250 {
		t.Errorf("compared %d components; the twelve scenarios carry over 250", components)
	}
}

// identityOf projects rendered device documents onto their identities.
func identityOf(t *testing.T, doc bundleDoc) map[string]identityFile {
	t.Helper()
	out := make(map[string]identityFile, len(doc.Documents))
	for _, d := range doc.Documents {
		raw, err := json.Marshal(d.Document)
		if err != nil {
			t.Fatalf("re-encode %s: %v", d.Topic, err)
		}
		var body struct {
			Device struct {
				Identifiers []string `json:"identifiers"`
				ViaDevice   string   `json:"via_device"`
			} `json:"device"`
			Components map[string]identityComponent `json:"components"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode %s: %v", d.Topic, err)
		}
		out[d.Topic] = identityFile{
			Identifiers: body.Device.Identifiers,
			ViaDevice:   body.Device.ViaDevice,
			Components:  body.Components,
		}
	}
	return out
}

func documentTopics(m map[string]identityFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
