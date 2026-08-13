// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPublishSchedules(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "de", pub)

	published, err := d.PublishSchedules(context.Background(), []ScheduleInfo{
		{ID: "werktag", Name: "Werktag"},
		{ID: "urlaub", Name: "Urlaub"},
	}, "http://127.0.0.1:8080/")
	if err != nil {
		t.Fatalf("PublishSchedules: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d configs, want 2", len(published))
	}

	topic := "homeassistant/switch/daikin_schedule_werktag/config"
	if !published[topic] {
		t.Fatalf("published topics = %v, want %s", published, topic)
	}
	raw, ok := pub.msgs[topic]
	if !ok {
		t.Fatalf("nothing published on %s", topic)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	cases := []struct{ key, want string }{
		// The display name is the operator's own text — user data is not
		// localized, and there is no name_de for it.
		{"name", "Werktag"},
		// The entity id is seeded from the frozen slug, not from the name, so a
		// rename cannot move it.
		{"unique_id", "daikin_schedule_werktag"},
		{"object_id", "daikin_schedule_werktag"},
		{"default_entity_id", "switch.daikin_schedule_werktag"},
		{"state_topic", "daikin/scheduler/werktag/enabled/state"},
		{"command_topic", "daikin/scheduler/werktag/enabled/set"},
		{"availability_topic", "daikin/bridge/status"},
		{"entity_category", "config"},
		{"payload_on", "on"},
		{"payload_off", "off"},
	}
	for _, c := range cases {
		if got, _ := cfg[c.key].(string); got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}

	dev, _ := cfg["device"].(map[string]any)
	if dev == nil {
		t.Fatal("config carries no device block")
	}
	ids, _ := dev["identifiers"].([]any)
	if len(ids) != 1 || ids[0] != schedulerIdentifier {
		t.Errorf("device identifiers = %v, want [%s]", ids, schedulerIdentifier)
	}
	if got, _ := dev["configuration_url"].(string); got != "http://127.0.0.1:8080/" {
		t.Errorf("configuration_url = %q", got)
	}
}

func TestPublishSchedulesSkipsEmptyID(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	published, err := d.PublishSchedules(context.Background(), []ScheduleInfo{{Name: "No id"}}, "")
	if err != nil {
		t.Fatalf("PublishSchedules: %v", err)
	}
	if len(published) != 0 {
		t.Errorf("published = %v, want nothing for a schedule without an id", published)
	}
	if len(pub.msgs) != 0 {
		t.Errorf("published %d messages, want none", len(pub.msgs))
	}
}

func TestScheduleConfigIsRecognisedAsOwn(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	if _, err := d.PublishSchedules(context.Background(), []ScheduleInfo{{ID: "werktag", Name: "Werktag"}}, ""); err != nil {
		t.Fatalf("PublishSchedules: %v", err)
	}
	raw := pub.msgs["homeassistant/switch/daikin_schedule_werktag/config"]
	// The orphan reconcile only clears configs it recognises as ours; a
	// deleted schedule must therefore be recognised here.
	if !d.IsOwnConfig(raw) {
		t.Error("a schedule config must be recognised as this daemon's own")
	}
}

func TestScheduleTopicsSanitizeID(t *testing.T) {
	pub := &capturePub{}
	d := New("homeassistant", "daikin", "en", pub)
	if _, err := d.PublishSchedules(context.Background(), []ScheduleInfo{{ID: "a-b_c", Name: "X"}}, ""); err != nil {
		t.Fatalf("PublishSchedules: %v", err)
	}
	for topic := range pub.msgs {
		if strings.Contains(topic, " ") {
			t.Errorf("topic %q contains a space", topic)
		}
	}
}
