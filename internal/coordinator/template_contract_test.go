// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-hamqtt/publisher"
)

// The template contract: every entity's templates, rendered against the state
// the production publish path really writes, yield a value the entity's Home
// Assistant platform accepts.
//
// The builders are pinned against the bundle goldens and the publishers against
// the surface goldens, but nothing compared the two — so 0.14.0 shipped
// scheduler switches whose state_on/state_off said `on`/`off` while their
// status item carried a JSON boolean the template renders as `false`. Home
// Assistant's MQTT switch maps only state_on, state_off and `None`
// (components/mqtt/switch.py, `_is_on_map`) and drops anything else, so the
// switch stayed unknown — and both goldens blessed it. This test reads both
// halves out of the same production calls the goldens are built from
// (buildBundleSurface, buildSurfaceRecorder) and renders one against the other
// with jinja_test.go's evaluator, which errors rather than skips on a shape it
// cannot read.
//
// What it checks, per component of every scenario's device documents:
//
//   - every state template on the item's retained state: switch and
//     binary_sensor against their on/off payloads, number and the climate
//     temperatures as floats, select, enum sensor and the climate's
//     mode/fan/swing/preset lists by membership, a timestamp sensor as RFC 3339
//     with a zone, a numeric sensor as a float, a sensor rendering `None`
//     exactly when its item holds no value;
//   - a cleared item (an empty retained payload) the way Home Assistant then
//     receives it, live: a sensor must render `None` (unknown), not fail;
//   - every sensor template on the three empty forms — an empty payload,
//     `{"val":null}`, `{"val":""}` — to `None`, without a template error;
//   - json_attributes_template to a JSON object;
//   - every availability entry to its payload_available/payload_not_available;
//   - the command round trip: what Home Assistant sends for each option (a
//     switch's payload_on/payload_off through the set path's boolean
//     conversion, a select's or climate list's option through its command
//     template), published back as a status object, renders as that option.
//
// What it cannot see: a value the scenarios never put on a topic (every
// enumerated option is exercised only through the round trip, not through a
// device that reports it), and Home Assistant behaviour outside the platform
// rules restated in accept* below.
func TestTemplatesAcceptWhatThePublishPathWrites(t *testing.T) {
	t.Parallel()
	for _, sc := range surfaceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			retained, cleared := retainedState(buildSurfaceRecorder(t, sc).raw())
			checked := 0
			for _, doc := range buildBundleSurface(t, sc).Documents {
				comps, ok := doc.Document["components"].(map[string]any)
				if !ok {
					t.Fatalf("%s carries no components", doc.Topic)
				}
				keys := make([]string, 0, len(comps))
				for k := range comps {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, key := range keys {
					comp, _ := comps[key].(map[string]any)
					cc := contractComponent{t: t, id: doc.Topic + "#" + key, comp: comp, retained: retained, cleared: cleared}
					cc.check()
					checked += cc.renders
				}
			}
			if checked == 0 {
				t.Fatal("rendered no template at all")
			}
		})
	}
}

// retainedState replays a recording into what the broker retains: the last
// payload per topic, and the topics whose last write was a clear.
func retainedState(msgs []recordedMsg) (retained map[string]string, cleared map[string]bool) {
	retained, cleared = map[string]string{}, map[string]bool{}
	for _, m := range msgs {
		if !m.Retain {
			continue
		}
		if len(m.Payload) == 0 {
			delete(retained, m.Topic)
			cleared[m.Topic] = true
			continue
		}
		retained[m.Topic] = string(m.Payload)
		delete(cleared, m.Topic)
	}
	return retained, cleared
}

type contractComponent struct {
	t        *testing.T
	id       string
	comp     map[string]any
	retained map[string]string
	cleared  map[string]bool
	renders  int
	used     map[string]bool
}

func (c *contractComponent) str(key string) string {
	c.used[key] = true
	s, _ := c.comp[key].(string)
	return s
}

func (c *contractComponent) list(key string) []string {
	c.used[key] = true
	raw, _ := c.comp[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func (c *contractComponent) render(label, tmpl, payload string) (string, bool) {
	c.t.Helper()
	c.renders++
	if tmpl == "" {
		return strings.TrimSpace(payload), true
	}
	out, err := renderJinja(tmpl, payload)
	if err != nil {
		c.t.Errorf("%s %s: %v (payload %q, template %s)", c.id, label, err, payload, tmpl)
		return "", false
	}
	return out, true
}

// state renders the template of one state-reading role against its item's
// retained state and hands the result to accept. A cleared item is rendered
// against the empty payload Home Assistant receives live; an item nothing
// writes is an error.
func (c *contractComponent) state(label, topicKey, tmplKey string, accept func(out, payload string) error) {
	c.t.Helper()
	topic := c.str(topicKey)
	tmpl := c.str(tmplKey)
	if topic == "" {
		c.t.Errorf("%s: %s without %s", c.id, tmplKey, topicKey)
		return
	}
	payload, ok := c.retained[topic]
	if !ok {
		if !c.cleared[topic] {
			c.t.Errorf("%s %s: %s is advertised but nothing the publish path writes", c.id, label, topic)
			return
		}
		payload = ""
	}
	out, ok := c.render(label, tmpl, payload)
	if !ok {
		return
	}
	if err := accept(out, payload); err != nil {
		c.t.Errorf("%s %s: %v (payload %q, template %s)", c.id, label, err, payload, tmpl)
	}
}

func (c *contractComponent) check() {
	c.t.Helper()
	c.used = map[string]bool{}
	platform := c.str("platform")

	if c.comp["json_attributes_topic"] != nil {
		c.state("json_attributes", "json_attributes_topic", "json_attributes_template", acceptObject)
	}
	c.availability()

	switch platform {
	case "sensor":
		c.sensor()
	case "binary_sensor":
		on, off := orDefault(c.str("payload_on"), "ON"), orDefault(c.str("payload_off"), "OFF")
		c.state("state", "state_topic", "value_template", acceptOneOf(on, off))
	case "switch":
		c.switchPlatform()
	case "number":
		c.state("state", "state_topic", "value_template", acceptFloat)
		c.str("command_topic")
	case "select":
		opts := c.list("options")
		c.state("state", "state_topic", "value_template", acceptOneOf(opts...))
		c.roundTrip("option", opts, "value_template", "command_template")
		c.str("command_topic")
	case "button":
		c.str("command_topic")
		c.str("payload_press")
	case "climate":
		c.climate()
	default:
		c.t.Errorf("%s: platform %q has no contract here — add one rather than letting it pass unread", c.id, platform)
	}

	// Every template key must have been read by one of the rules above, so a
	// new template cannot slip past this test unrendered.
	for key := range c.comp {
		if strings.HasSuffix(key, "_template") && !c.used[key] {
			c.t.Errorf("%s: %s is not covered by the contract", c.id, key)
		}
	}
}

func (c *contractComponent) availability() {
	c.t.Helper()
	c.used["availability"] = true
	entries, _ := c.comp["availability"].([]any)
	for i, raw := range entries {
		e, _ := raw.(map[string]any)
		topic, _ := e["topic"].(string)
		tmpl, _ := e["value_template"].(string)
		avail, _ := e["payload_available"].(string)
		notAvail, _ := e["payload_not_available"].(string)
		avail, notAvail = orDefault(avail, "online"), orDefault(notAvail, "offline")
		payload, ok := c.retained[topic]
		if !ok {
			c.t.Errorf("%s availability[%d]: %s is never written", c.id, i, topic)
			continue
		}
		label := fmt.Sprintf("availability[%d]", i)
		if out, ok := c.render(label, tmpl, payload); ok && out != avail && out != notAvail {
			c.t.Errorf("%s %s: renders %q on %q, want %q or %q", c.id, label, out, payload, avail, notAvail)
		}
	}
}

// numericDeviceClasses are the sensor device classes Home Assistant parses as
// numbers (anything but none, enum, date and timestamp).
func sensorExpectsNumber(comp map[string]any) bool {
	dc, _ := comp["device_class"].(string)
	switch dc {
	case "enum", "date", "timestamp":
		return false
	case "":
		_, unit := comp["unit_of_measurement"]
		_, sc := comp["state_class"]
		return unit || sc
	}
	return true
}

func (c *contractComponent) sensor() {
	c.t.Helper()
	tmpl := c.str("value_template")
	opts := c.list("options")
	dc := c.str("device_class")
	var accept func(string) error
	switch {
	case len(opts) > 0:
		accept = func(out string) error { return acceptOneOf(opts...)(out, "") }
	case dc == "timestamp":
		accept = acceptTimestamp
	case sensorExpectsNumber(c.comp):
		accept = func(out string) error { return acceptFloat(out, "") }
	default:
		accept = func(out string) error {
			if out == "" || len(out) > 255 {
				return fmt.Errorf("renders %q, which a text sensor shows as an empty or refused state", out)
			}
			return nil
		}
	}
	c.state("state", "state_topic", "value_template", func(out, payload string) error {
		if empty := statusHoldsNoValue(payload); empty != (out == "None") {
			if empty {
				return fmt.Errorf("an item without a value renders %q, want None (unknown)", out)
			}
			return fmt.Errorf("an item with a value renders None")
		}
		if out == "None" {
			return nil
		}
		return accept(out)
	})
	// A sensor must turn every empty form into unknown, without an error —
	// the live delivery of a clear, a null and an empty text.
	for _, payload := range []string{"", `{"val":null,"ts":1,"lc":1}`, `{"val":"","ts":1,"lc":1}`} {
		if out, ok := c.render("empty value", tmpl, payload); ok && out != "None" {
			c.t.Errorf("%s: empty value %q renders %q, want None so Home Assistant shows unknown", c.id, payload, out)
		}
	}
}

// statusHoldsNoValue reports whether payload is an item without a value: a
// clear, or a status object whose val is null or "".
func statusHoldsNoValue(payload string) bool {
	if payload == "" {
		return true
	}
	var doc map[string]any
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return false
	}
	v, ok := doc["val"]
	return !ok || v == nil || v == ""
}

func (c *contractComponent) switchPlatform() {
	c.t.Helper()
	on, off := orDefault(c.str("payload_on"), "ON"), orDefault(c.str("payload_off"), "OFF")
	stateOn, stateOff := orDefault(c.str("state_on"), on), orDefault(c.str("state_off"), off)
	c.state("state", "state_topic", "value_template", acceptOneOf(stateOn, stateOff))
	c.str("command_topic")
	// What Home Assistant sends is payload_on/payload_off verbatim; the set
	// path reads it with spec §5.3's boolean conversion and the status item
	// then carries that boolean. Rendered back, it must be the matching state.
	tmpl := c.str("value_template")
	for _, tc := range []struct{ send, want string }{{on, stateOn}, {off, stateOff}} {
		b, err := (publisher.SetValue{Text: tc.send}).Bool()
		if err != nil {
			c.t.Errorf("%s: the set path refuses the command %q Home Assistant sends: %v", c.id, tc.send, err)
			continue
		}
		payload := fmt.Sprintf(`{"val":%t,"ts":1,"lc":1}`, b)
		if out, ok := c.render("command round trip", tmpl, payload); ok && out != tc.want {
			c.t.Errorf("%s: after the command %q the item is %s, which renders %q — not the state %q, so Home Assistant ignores it", c.id, tc.send, payload, out, tc.want)
		}
	}
}

func (c *contractComponent) climate() {
	c.t.Helper()
	modes := c.list("modes")
	c.state("mode", "mode_state_topic", "mode_state_template", acceptOneOf(modes...))
	c.str("mode_command_topic")
	c.state("temperature", "temperature_state_topic", "temperature_state_template", acceptFloat)
	c.str("temperature_command_topic")
	if c.comp["current_temperature_topic"] != nil {
		c.state("current_temperature", "current_temperature_topic", "current_temperature_template", acceptFloat)
	}
	for _, role := range []struct{ name, list, stateTmpl string }{
		{"fan_mode", "fan_modes", "fan_mode_state_template"},
		{"swing_mode", "swing_modes", "swing_mode_state_template"},
		{"swing_horizontal_mode", "swing_horizontal_modes", "swing_horizontal_mode_state_template"},
		{"preset_mode", "preset_modes", "preset_mode_value_template"},
	} {
		if c.comp[role.name+"_state_topic"] == nil && c.comp[role.stateTmpl] == nil {
			continue
		}
		opts := c.list(role.list)
		accepted := opts
		if role.name == "preset_mode" {
			// Home Assistant prepends `none` to every preset list.
			accepted = append([]string{"none"}, opts...)
		}
		c.state(role.name, role.name+"_state_topic", role.stateTmpl, acceptOneOf(accepted...))
		c.str(role.name + "_command_topic")
		c.roundTrip(role.name, opts, role.stateTmpl, role.name+"_command_template")
	}
}

// roundTrip sends each option through the command template, publishes the
// result back as a status item the way the set path would, and requires the
// state template to show the same option again.
func (c *contractComponent) roundTrip(label string, opts []string, stateKey, commandKey string) {
	c.t.Helper()
	stateTmpl, cmdTmpl := c.str(stateKey), c.str(commandKey)
	for _, opt := range opts {
		sent, ok := c.render(label+" command", cmdTmpl, opt)
		if !ok {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"val": sent, "ts": 1, "lc": 1})
		if out, ok := c.render(label+" round trip", stateTmpl, string(payload)); ok && out != opt {
			c.t.Errorf("%s %s: option %q is sent as %q and reads back as %q", c.id, label, opt, sent, out)
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func acceptOneOf(allowed ...string) func(out, payload string) error {
	return func(out, _ string) error {
		for _, a := range allowed {
			if out == a {
				return nil
			}
		}
		return fmt.Errorf("renders %q, which is none of %q — Home Assistant drops it", out, allowed)
	}
}

func acceptFloat(out, _ string) error {
	if _, err := strconv.ParseFloat(out, 64); err != nil {
		return fmt.Errorf("renders %q, which is no number", out)
	}
	return nil
}

func acceptTimestamp(out string) error {
	if _, err := time.Parse(time.RFC3339, out); err != nil {
		return fmt.Errorf("renders %q, which is no RFC 3339 timestamp with a zone", out)
	}
	return nil
}

func acceptObject(out, _ string) error {
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc == nil {
		return fmt.Errorf("renders %q, which is no JSON object", out)
	}
	return nil
}
