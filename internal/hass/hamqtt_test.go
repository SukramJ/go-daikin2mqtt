// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"strings"
	"testing"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// Unit pins for the ADR 0070 phase 8 step 4 rendering path.
//
// The byte-equality proof lives in internal/coordinator, against the twelve
// pinned scenario goldens. What lives here is everything that proof cannot
// reach: the places where a mutation changes no published byte, which is
// exactly where a blind spot hides. Each of these is a thing asserted directly
// because it could not be made to fail through the goldens.

func testDiscovery(lang string) *Discovery { return New("homeassistant", "daikin", lang, nil) }

func testPoint(topic, platform string) process.Point {
	return process.Point{
		DeviceID:   "809d41d9-4d42-45fa-af6a-84b512143672",
		EmbeddedID: "climateControl",
		MPType:     "climateControl",
		Topic:      topic,
		Entry:      catalog.Entry{Topic: topic, Name: "Probe", Platform: platform},
	}
}

// TestHamqttLayoutMatchesInternalLayout pins the library path's topics against
// the DAEMON's own builders, builder to builder — the F3 standard — rather
// than through a golden, which a regeneration would re-bless.
func TestHamqttLayoutMatchesInternalLayout(t *testing.T) {
	d := testDiscovery("en")
	lay := hamqttLayout{root: d.state}

	for _, tc := range []struct{ device, embedded, topic string }{
		{"809d41d9-4d42-45fa-af6a-84b512143672", "climateControl", "room_temperature"},
		{"809d41d9-4d42-45fa-af6a-84b512143672", "climateControl", HVACModeTopic},
		{"809d41d9-4d42-45fa-af6a-84b512143672", "gateway", "wifi_strength"},
		{"11112222-3333-4444-5555-666677778888", "outdoorUnit", "outdoor_silent"},
		{layout.SchedulerDeviceID, "werktag", layout.EnabledTopic},
	} {
		p := process.Point{DeviceID: tc.device, EmbeddedID: tc.embedded, Topic: tc.topic}
		slot := hamqttSlot(tc.device, tc.embedded, tc.topic)
		if got, want := lay.State(slot), d.StateTopic(p); got != want {
			t.Errorf("State: layout %q, Discovery.StateTopic %q", got, want)
		}
		if got, want := lay.Command(slot), d.CommandTopic(p); got != want {
			t.Errorf("Command: layout %q, Discovery.CommandTopic %q", got, want)
		}
		if got, want := lay.Attributes(slot), d.AttributesTopic(p); got != want {
			t.Errorf("Attributes: layout %q, Discovery.AttributesTopic %q", got, want)
		}
	}
	if got, want := lay.Bridge(), d.ConnectedTopic(); got != want {
		t.Errorf("Bridge: layout %q, Discovery.ConnectedTopic %q", got, want)
	}
	// SmartHomeLayout's three instance topics, against internal/layout.
	if lay.Connected() != lay.Bridge() {
		t.Errorf("Connected %q differs from Bridge %q; the publisher refuses such a layout", lay.Connected(), lay.Bridge())
	}
	if got, want := lay.Info(), d.state.Info(); got != want {
		t.Errorf("Info: layout %q, internal/layout %q", got, want)
	}
	if got, want := lay.Maintenance("set", "loglevel"), "daikin/maintenance/set/loglevel"; got != want {
		t.Errorf("Maintenance = %q, want %q", got, want)
	}
	// The schedule pair — one topic composed in two packages from two
	// constants, which is the shape the phase-8 step-2 corrections found F3
	// had missed. The library path must agree with both halves.
	sched := hamqttSlot(layout.SchedulerDeviceID, "werktag", layout.EnabledTopic)
	if got, want := lay.State(sched), d.ScheduleStateTopic("werktag"); got != want {
		t.Errorf("schedule state: layout %q, Discovery.ScheduleStateTopic %q", got, want)
	}
	if got, want := lay.Command(sched), d.ScheduleCommandTopic("werktag"); got != want {
		t.Errorf("schedule command: layout %q, Discovery.ScheduleCommandTopic %q", got, want)
	}
	climate := hamqttSlot("dev", "climateControl", layout.ClimateTopic)
	if got, want := lay.Attributes(climate), d.ClimateAttributesTopic("dev", "climateControl"); got != want {
		t.Errorf("climate attributes: layout %q, Discovery.ClimateAttributesTopic %q", got, want)
	}
}

// TestHamqttLayoutIgnoresOnlyWhatItDeclares closes the blind spot the layout's
// own doc comment names.
//
// hamqttLayout reads a slot's Address, Channel and Path and nothing else. A
// mutation that started reading Bucket or Scope would change no published byte
// — every slot this path builds has the same bucket and no scope — so the
// goldens can never catch one. Both halves are asserted directly: that Bucket
// is inert, and that no slot the path builds carries a scope or a path this
// bridge's four-level tree could not render.
func TestHamqttLayoutIgnoresOnlyWhatItDeclares(t *testing.T) {
	lay := hamqttLayout{root: layout.New("daikin")}
	base := hamqttSlot("dev", "mp", "leaf")
	want := lay.State(base)
	for _, b := range []hamodel.Bucket{
		hamodel.BucketUnset, hamodel.BucketValues, hamodel.BucketMaster,
		hamodel.BucketCalculated, hamodel.BucketCustom,
	} {
		s := base
		s.Bucket = b
		if got := lay.State(s); got != want {
			t.Errorf("bucket %v changed the state topic: %q, want %q", b, got, want)
		}
	}
	// Availability renders nothing at all: the library would address it by
	// the Home Assistant device's UID, which is not the ONECTA device id the
	// online item is keyed by, so the entity appends its online entry itself.
	if got := lay.Availability(base); got != "" {
		t.Errorf("Availability rendered %q; the online entry is the entity's, not the layout's", got)
	}
	// hamqttSlot is the only constructor, and it is the reason the two
	// ignored fields are safe to ignore.
	s := hamqttSlot("dev", "mp", "leaf")
	if len(s.Scope) != 0 {
		t.Errorf("hamqttSlot produced a scope %v; hamqttLayout would drop it silently", s.Scope)
	}
	if len(s.Path) != 1 || s.Path[0] == "" {
		t.Errorf("hamqttSlot produced path %v; this bridge's tree has exactly one leaf segment", s.Path)
	}
	if s.Address == "" || s.Channel == "" {
		t.Errorf("hamqttSlot produced an unaddressable slot %+v", s)
	}
}

// TestHamqttLayoutIsNotTheLibraryDefault records why topic.Default cannot be
// used, so a later reader does not try.
func TestHamqttLayoutIsNotTheLibraryDefault(t *testing.T) {
	lay := hamqttLayout{root: layout.New("daikin")}
	def := hatopic.Default{Root: "daikin"}
	s := hamqttSlot("dev", "mp", "leaf")
	if lay.State(s) == def.State(s) {
		t.Error("topic.Default reproduces this bridge's state topic; the delegation would be unnecessary")
	}
	if lay.Command(s) == def.Command(s) {
		t.Error("topic.Default reproduces this bridge's command topic")
	}
	if def.Availability(s) == "" {
		t.Error("topic.Default renders no per-device availability topic; F8's trap would not exist")
	}
	// Nor is the library's own SmartHome State usable: it puts the slot's
	// bucket into the item path.
	sh := layout.New("daikin").SmartHome()
	if lay.State(s) == sh.State(s) {
		t.Error("topic.SmartHome reproduces this bridge's status item; the override would be unnecessary")
	}
}

// TestHamqttContextUsesThisBridgesNormalisers proves the three Context
// overrides are load-bearing rather than a restatement of the library's own
// defaults — the question decision F4 settled and this step may not reopen.
//
// UniqueID and ObjectID are both caught by the twelve goldens (the fixtures
// carry "Küche" and "Außengerät", and every device id is a hyphenated UUID).
// NodeID is NOT: no published topic of this bridge contains a node id, so a
// mutation there moves no pinned byte. It is asserted here instead.
func TestHamqttContextUsesThisBridgesNormalisers(t *testing.T) {
	d := testDiscovery("en")
	ctx, ok := d.HamqttContext().(hamqttContext)
	if !ok {
		t.Fatalf("HamqttContext is %T, want hamqttContext", d.HamqttContext())
	}

	// The entity-id seed. topic.Slug expands "ä" to "ae"; this bridge's
	// slugify expands it to "a", matching Home Assistant's own slugify, and
	// discovery.go says so at the point of definition. Four of the twelve
	// scenarios carry a German device name, so the goldens catch a swap here
	// too — asserted anyway, because the divergence class is the whole of
	// decision F4.
	kitchen := hamqttDevice(device{Identifiers: []string{"daikin_809d41d9-4d42-45fa-af6a-84b512143672"}, Name: "Küche"})
	sensor := &renderEntity{
		EntityKey: "room_temperature", EntityPlatform: hacatalog.Platform("sensor"),
		idBase: "daikin_809d41d9-4d42-45fa-af6a-84b512143672",
		seed:   "Küche",
	}
	const wantObject = "kuche_room_temperature"
	if got := ctx.ObjectID(kitchen, sensor); got != wantObject {
		t.Errorf("ObjectID = %q, want %q", got, wantObject)
	}
	if got := discovery.ObjectID(kitchen, sensor); got == wantObject {
		t.Errorf("the library's own ObjectID also yields %q; the override would be inert", got)
	}

	// The unique id and the node id. On a main device they are NOT
	// distinguishable — an ONECTA device id is a lower-case hyphenated UUID
	// and topic.Slug preserves both — so the probe is the shape where they
	// diverge and which this bridge really publishes: a shared sub-device
	// keyed on an UPPER-CASE component serial. sanitize preserves case;
	// topic.Slug lower-cases. Fifteen of the 264 pinned unique ids carry one.
	outdoor := hamqttDevice(device{Identifiers: []string{"daikin_outdoor_ODU0000000001"}, Name: "Daikin Outdoor unit"})
	silent := &renderEntity{
		EntityKey: "outdoor_silent", EntityPlatform: hacatalog.Platform("switch"),
		idBase: "daikin_outdoor_ODU0000000001",
		seed:   "Daikin Outdoor unit",
	}
	const wantUID = "daikin_outdoor_ODU0000000001_outdoor_silent"
	if got := ctx.UniqueID(outdoor, silent); got != wantUID {
		t.Errorf("UniqueID = %q, want %q", got, wantUID)
	}
	if got := discovery.UniqueID("", outdoor, silent); got == wantUID {
		t.Errorf("the library's own UniqueID also yields %q; the override would be inert", got)
	}

	// The node id reaches no retained topic of this bridge — the config topic
	// is four segments (F5) — so no golden can catch a mutation here. It is
	// asserted directly, on the same case-bearing probe.
	const wantNode = "daikin_outdoor_ODU0000000001"
	if got := ctx.NodeID(outdoor); got != wantNode {
		t.Errorf("NodeID = %q, want %q", got, wantNode)
	}
	if got := discovery.NodeID(outdoor); got == wantNode {
		t.Errorf("the library's own NodeID also yields %q; the override would be inert", got)
	}

	// And the one that is genuinely equivalent, named rather than left as a
	// blind spot: on a main device — a lower-case hyphenated UUID — sanitize
	// and topic.Slug agree, so 249 of the 264 unique ids would survive the
	// swap unchanged. The proof rests on the other fifteen.
	main := &renderEntity{
		EntityKey: "room_temperature", EntityPlatform: hacatalog.Platform("sensor"),
		idBase: "daikin_809d41d9-4d42-45fa-af6a-84b512143672",
	}
	if ctx.UniqueID(kitchen, main) != discovery.UniqueID("", kitchen, main) {
		t.Error("sanitize and topic.Slug now disagree on a lower-case hyphenated UUID; " +
			"the comment above is stale and the divergence class has grown")
	}
}

// TestHamqttEncodingIsStatusObject pins the encoding every status item is read
// with: mqtt-smarthome 2.0's `{"val","ts","lc"}` object (openccu-loom ADR 0083).
func TestHamqttEncodingIsStatusObject(t *testing.T) {
	if got := testDiscovery("en").HamqttContext().Encoding(); got != discovery.StatusObjectEncoding {
		t.Errorf("Encoding = %v, want discovery.StatusObjectEncoding", got)
	}
}

// TestHamqttAvailabilityIsConnectedAndOnline pins ADR 0083's availability:
// `<name>/connected` read at ≥ 2, plus the device's own `online` item, combined
// with `availability_mode: all`, each entry carrying only the four keys spec §8
// allows.
//
// The library's zero model.Availability resolves to {LevelBridge,
// LevelDevice}, and LevelDevice would be addressed by the Home Assistant
// device's UID — `daikin_<uuid>` — rather than the ONECTA device id the online
// item is keyed by. smartHomeAvailability refuses that shape loudly; this
// asserts the refusal too.
func TestHamqttAvailabilityIsConnectedAndOnline(t *testing.T) {
	d := testDiscovery("en")
	ctx := d.HamqttContext()
	dev := hamqttDevice(device{Identifiers: []string{"daikin_x"}, Name: "Probe"})

	p := testPoint("room_temperature", "sensor")
	e, ok := d.pointEntity(hamqttLayout{root: d.state}, p, "daikin_x", "Probe")
	if !ok {
		t.Fatal("pointEntity declined a sensor")
	}
	levels, _ := e.Desc().Availability.Resolved()
	if len(levels) != 1 || levels[0] != hamodel.LevelBridge {
		t.Errorf("availability levels = %v, want exactly {LevelBridge}", levels)
	}

	body := renderBody(t, d, e)
	list, ok := body["availability"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("availability = %v, want a two-entry list", body["availability"])
	}
	want := []map[string]any{
		{
			"topic": "daikin/connected", "value_template": "{{ 'online' if value | int(0) >= 2 else 'offline' }}",
			"payload_available": "online", "payload_not_available": "offline",
		},
		{
			"topic": "daikin/status/" + p.DeviceID + "/online", "value_template": "{{ value_json.val | lower }}",
			"payload_available": "true", "payload_not_available": "false",
		},
	}
	for i, w := range want {
		got, _ := list[i].(map[string]any)
		if len(got) != len(w) {
			t.Errorf("entry %d has keys %v, want exactly %v", i, got, w)
		}
		for k, v := range w {
			if got[k] != v {
				t.Errorf("entry %d: %s = %v, want %v", i, k, got[k], v)
			}
		}
	}
	if body["availability_mode"] != "all" {
		t.Errorf("availability_mode = %v, want all", body["availability_mode"])
	}
	for _, k := range []string{"availability_topic", "payload_available", "payload_not_available", "availability_template"} {
		if _, present := body[k]; present {
			t.Errorf("the single-topic key %q survived beside the list", k)
		}
	}
	if got := d.OnlineTopic(p.DeviceID); got != "daikin/status/"+p.DeviceID+"/online" {
		t.Errorf("OnlineTopic = %q", got)
	}

	// The default is refused, loudly, rather than published.
	e.Description.Availability = hamodel.Availability{}
	if _, err := discovery.RenderComponent(ctx, dev, e, discovery.Origin{}); err == nil {
		t.Error("the library's default availability rendered without complaint")
	} else if !strings.Contains(err.Error(), "model.BridgeOnly") {
		t.Errorf("refusal does not name the setting: %v", err)
	}
}

// TestHamqttStatusTemplates pins how each platform reads a status object, the
// one place a wrong template is invisible until an entity shows `unknown` or
// the whole `{"val":…}` document.
func TestHamqttStatusTemplates(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		d := testDiscovery(lang)
		lay := hamqttLayout{root: d.state}
		render := func(p process.Point) map[string]any {
			t.Helper()
			e, ok := d.pointEntity(lay, p, "daikin_x", "Probe")
			if !ok {
				t.Fatalf("pointEntity declined %s", p.Entry.Platform)
			}
			return renderBody(t, d, e)
		}

		sensor := render(testPoint("room_temperature", "sensor"))
		// A sensor renders `None` (unknown) for a cleared or empty item; see
		// SensorValueTemplate.
		if sensor["value_template"] != SensorValueTemplate {
			t.Errorf("%s sensor value_template = %v", lang, sensor["value_template"])
		}
		if sensor["json_attributes_template"] != AttributesTemplate {
			t.Errorf("%s json_attributes_template = %v, want %q", lang, sensor["json_attributes_template"], AttributesTemplate)
		}
		if sensor["json_attributes_topic"] != "daikin/status/809d41d9-4d42-45fa-af6a-84b512143672/climateControl/room_temperature/attributes" {
			t.Errorf("%s json_attributes_topic = %v", lang, sensor["json_attributes_topic"])
		}

		for _, platform := range []string{"switch", "binary_sensor"} {
			body := render(testPoint("power", platform))
			if body["value_template"] != "{{ value_json.val | lower }}" {
				t.Errorf("%s %s value_template = %v", lang, platform, body["value_template"])
			}
			if body["payload_on"] != "true" || body["payload_off"] != "false" {
				t.Errorf("%s %s payloads = %v/%v, want true/false", lang, platform, body["payload_on"], body["payload_off"])
			}
			for _, k := range []string{"state_on", "state_off"} {
				if _, present := body[k]; present {
					t.Errorf("%s %s carries %s; it defaults to the payload", lang, platform, k)
				}
			}
		}

		sel := testPoint("operation_mode", "select")
		sel.Entry.Values = []catalog.ValueLabel{
			{Value: "heating", Label: "Heating", LabelDE: "Heizen"},
			{Value: "fanOnly", Label: "Fan only", LabelDE: "Nur Lüfter"},
		}
		body := render(sel)
		label := map[string]string{"en": "Heating", "de": "Heizen"}[lang]
		vt, _ := body["value_template"].(string)
		ct, _ := body["command_template"].(string)
		if !strings.Contains(vt, "'heating': '"+label+"'") || !strings.Contains(vt, "value_json.val") {
			t.Errorf("%s select value_template does not map the token to %q: %q", lang, label, vt)
		}
		if !strings.Contains(ct, "'"+label+"': 'heating'") {
			t.Errorf("%s select command_template does not map %q back to the token: %q", lang, label, ct)
		}
		opts, _ := body["options"].([]any)
		if len(opts) != 2 || opts[0] != label {
			t.Errorf("%s select options = %v, want the labels", lang, opts)
		}

		// The scheduler's sensor: token on the wire, label in Home Assistant,
		// and no options list (its other states are free text).
		sched := testPoint("schedule_state", "sensor")
		sched.Entry.Values = []catalog.ValueLabel{{Value: "idle", Label: "No block", LabelDE: "Kein Block"}}
		body = render(sched)
		idle := map[string]string{"en": "No block", "de": "Kein Block"}[lang]
		if vt, _ := body["value_template"].(string); !strings.Contains(vt, "'idle': '"+idle+"'") ||
			!strings.Contains(vt, "m.get(value_json.val, value_json.val)") {
			t.Errorf("%s schedule_state value_template = %q", lang, vt)
		}
		if _, present := body["options"]; present {
			t.Errorf("%s schedule_state carries options; a free-text sensor would be refused", lang)
		}
	}
}

// TestHamqttButtonDeclinesAStateTopic renders deliberately wrong input.
//
// This bridge's button builder clears the state topic it just computed, and the
// library instead declines to project state_topic onto a platform whose schema
// does not declare one. Rendering only correct input never exercises that
// guarantee — so the button is given a readable state binding on purpose, and
// the assertion is that the rendered payload still carries no state_topic.
func TestHamqttButtonDeclinesAStateTopic(t *testing.T) {
	d := testDiscovery("en")
	lay := hamqttLayout{root: d.state}
	e, ok := d.pointEntity(lay, testPoint(RefreshTopic, "button"), "daikin_x", "Probe")
	if !ok {
		t.Fatal("pointEntity declined a button")
	}
	if _, found := hamodel.Bind(e, hamodel.RoleState); !found {
		t.Fatal("the button has no readable state binding; the guarantee is not being exercised")
	}
	body := renderBody(t, d, e)
	if _, present := body["state_topic"]; present {
		t.Errorf("button carries a state_topic: %v", body["state_topic"])
	}
	if body["command_topic"] == nil || body["payload_press"] != "PRESS" {
		t.Errorf("button lost its command surface: %v", body)
	}
}

// TestHamqttCompositeClimateIsBindingsAndABuilder pins the shape of the one
// composite entity in the programme.
//
// Five of its seven roles are SYNTHETIC — backed by no catalogue entry and no
// register — and ADR 0070 §3.5 says that class must need no runtime special
// case. This asserts it does not: they are ordinary model.Slots on the same
// device and management point, resolved by the same Layout, and the only thing
// that knows how Home Assistant spells each role is the entity's own Builder.
func TestHamqttCompositeClimateIsBindingsAndABuilder(t *testing.T) {
	d := testDiscovery("en")
	lay := hamqttLayout{root: d.state}

	power := testPoint("power", "switch")
	mode := testPoint("operation_mode", "select")
	mode.Entry.Values = []catalog.ValueLabel{{Value: "heating"}, {Value: "cooling"}}
	setpoint := testPoint("temperature_setpoint", "number")
	current := testPoint("room_temperature", "sensor")
	g := &climateGroup{
		deviceID: power.DeviceID, embeddedID: power.EmbeddedID,
		power: &power, mode: &mode, setpoint: &setpoint, current: &current,
	}
	ci := ClimateInfo{
		FanModes: &hamodel.Enum{Codes: []string{"auto", "quiet", "3"}, Labels: map[string]hamodel.Localized{
			"auto": {Lang: map[string]string{"de": "Automatik"}},
		}},
		SwingModes:           &hamodel.Enum{Codes: []string{"stop", "swing"}},
		SwingHorizontalModes: &hamodel.Enum{Codes: []string{"stop"}},
		PresetModes:          &hamodel.Enum{Codes: []string{"boost"}},
	}
	e := d.climateEntity(lay, g, DeviceInfo{Name: "Küche"}, ci)

	// Suppression, not filtering: the composite names what it replaces and the
	// model removes it.
	wantSuppressed := []string{"power", "operation_mode", "temperature_setpoint"}
	if got := e.Suppresses(); strings.Join(got, ",") != strings.Join(wantSuppressed, ",") {
		t.Errorf("Suppresses() = %v, want %v", got, wantSuppressed)
	}
	siblings := make([]hamodel.Entity, 0, 5)
	siblings = append(siblings, e)
	for _, p := range []process.Point{power, mode, setpoint, current} {
		se, ok := d.pointEntity(lay, p, "daikin_x", "Küche")
		if !ok {
			t.Fatalf("pointEntity declined %q", p.Topic)
		}
		siblings = append(siblings, se)
	}
	survivors := hamodel.ApplySuppression(siblings)
	if len(survivors) != 2 {
		t.Fatalf("suppression left %d entities, want the composite plus room_temperature", len(survivors))
	}

	// Seven roles, five of them synthetic, all ordinary slots.
	roles := map[string]bool{}
	for _, b := range e.Bindings() {
		roles[b.Role] = true
		if !b.Slot.Valid() {
			t.Errorf("role %q has an invalid slot %+v", b.Role, b.Slot)
		}
		if b.Slot.Address != g.deviceID || b.Slot.Channel != g.embeddedID {
			t.Errorf("role %q escaped the management point: %+v", b.Role, b.Slot)
		}
	}
	for _, want := range []string{
		roleMode, roleTemperature, roleCurrentTemperature,
		roleFanMode, roleSwingMode, roleSwingHorizontalMode, rolePresetMode,
	} {
		if !roles[want] {
			t.Errorf("composite climate has no %q binding", want)
		}
	}
	synthetic := 0
	for _, leaf := range []string{HVACModeTopic, FanModeTopic, SwingModeTopic, SwingHModeTopic, PresetModeTopic} {
		for _, b := range e.Bindings() {
			if b.Slot.Leaf() == leaf {
				synthetic++
			}
		}
	}
	if synthetic != 5 {
		t.Errorf("found %d synthetic slots, want 5", synthetic)
	}

	body := renderBody(t, d, e)
	// The composite reads a SIBLING component's state topic — the open
	// question the measurement flagged as needing a live Home Assistant.
	// Nothing in the library objects; the question is Home Assistant's, and
	// it is recorded rather than answered here.
	if body["current_temperature_topic"] != d.StateTopic(current) {
		t.Errorf("current_temperature_topic = %v, want %q", body["current_temperature_topic"], d.StateTopic(current))
	}
	if body["state_topic"] != nil {
		t.Errorf("climate carries a plain state_topic: %v", body["state_topic"])
	}
	for _, key := range []string{
		"mode_state_topic", "mode_command_topic", "temperature_state_topic",
		"temperature_command_topic", "fan_mode_state_topic", "swing_mode_state_topic",
		"swing_horizontal_mode_state_topic", "preset_mode_state_topic",
	} {
		if body[key] == nil {
			t.Errorf("composite climate lost %q", key)
		}
	}
	if body["default_entity_id"] != "climate.kuche_thermostat" {
		t.Errorf("default_entity_id = %v, want climate.kuche_thermostat", body["default_entity_id"])
	}
	// Every role topic of Home Assistant's MQTT climate schema reads the
	// status object through its own template — the platform has no shared
	// value_template — and the mode/temperature commands carry the plain
	// token or number, so they need no command template.
	for key, want := range map[string]string{
		"mode_state_template":                  "{{ value_json.val }}",
		"temperature_state_template":           "{{ value_json.val }}",
		"current_temperature_template":         "{{ value_json.val }}",
		"fan_mode_state_template":              "{{ value_json.val }}",
		"swing_mode_state_template":            "{{ value_json.val }}",
		"swing_horizontal_mode_state_template": "{{ value_json.val }}",
		"preset_mode_value_template":           "{{ value_json.val }}",
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %q", key, body[key], want)
		}
	}
	for _, key := range []string{
		"mode_command_template", "temperature_command_template", "fan_mode_command_template",
		"swing_mode_command_template", "swing_horizontal_mode_command_template", "preset_mode_command_template",
		"value_template",
	} {
		if _, present := body[key]; present {
			t.Errorf("English climate carries %s = %v; its options already are the tokens", key, body[key])
		}
	}
	if body["json_attributes_template"] != AttributesTemplate {
		t.Errorf("climate json_attributes_template = %v", body["json_attributes_template"])
	}
	if list, _ := body["availability"].([]any); len(list) != 2 {
		t.Errorf("climate availability = %v, want connected + online", body["availability"])
	}

	// In German the fan option lists the label, and the pair maps it both
	// ways; an unlabelled code (a numeric speed) passes through both.
	de := testDiscovery("de")
	body = renderBody(t, de, de.climateEntity(hamqttLayout{root: de.state}, g, DeviceInfo{Name: "Küche"}, ci))
	if fans, _ := body["fan_modes"].([]any); len(fans) != 3 || fans[0] != "Automatik" || fans[2] != "3" {
		t.Errorf("German fan_modes = %v, want [Automatik quiet 3]", body["fan_modes"])
	}
	st, _ := body["fan_mode_state_template"].(string)
	ct, _ := body["fan_mode_command_template"].(string)
	if !strings.Contains(st, "'auto': 'Automatik'") || !strings.Contains(st, "value_json.val") {
		t.Errorf("German fan_mode_state_template = %q", st)
	}
	if !strings.Contains(ct, "'Automatik': 'auto'") {
		t.Errorf("German fan_mode_command_template = %q", ct)
	}
	if body["swing_mode_state_template"] != "{{ value_json.val }}" {
		t.Errorf("an unlabelled swing list grew a mapping: %v", body["swing_mode_state_template"])
	}
	if body["unique_id"] != "daikin_"+g.deviceID+"_climate" {
		t.Errorf("unique_id = %v", body["unique_id"])
	}
}

// TestHamqttScheduleEntityIDIsTheUniqueID pins the one entity-id composition
// that is NOT entityObjectID.
//
// A schedule's slug is frozen at creation and preserves a hyphen, which
// slugify folds — so seeding the entity id the usual way would diverge for any
// schedule whose id contains one. Every shipped scenario's schedule ids happen
// to contain none, so the goldens cannot catch it; the divergent input is
// asserted here.
func TestHamqttScheduleEntityIDIsTheUniqueID(t *testing.T) {
	d := testDiscovery("en")
	e := d.scheduleEntity(ScheduleInfo{ID: "mo-fr", Name: "Mo-Fr"})
	body := renderBody(t, d, e)
	if body["unique_id"] != "daikin_schedule_mo-fr" {
		t.Errorf("unique_id = %v, want daikin_schedule_mo-fr", body["unique_id"])
	}
	if body["default_entity_id"] != "switch.daikin_schedule_mo-fr" {
		t.Errorf("default_entity_id = %v, want switch.daikin_schedule_mo-fr — "+
			"entityObjectID would fold the hyphen and strand the registered entity",
			body["default_entity_id"])
	}
	if entityObjectID("daikin_schedule", "mo-fr") == "daikin_schedule_mo-fr" {
		t.Error("entityObjectID no longer folds the hyphen; the override is inert and the case is untested")
	}
	if body["json_attributes_topic"] != nil {
		t.Errorf("schedule switch gained a json_attributes_topic: %v", body["json_attributes_topic"])
	}
	// The daemon's own switch depends on `<name>/connected` alone: there is no
	// device whose online item it could read.
	if list, _ := body["availability"].([]any); len(list) != 1 {
		t.Errorf("schedule availability = %v, want connected alone", body["availability"])
	}
	if _, present := body["availability_mode"]; present {
		t.Errorf("one availability entry carries a mode: %v", body["availability_mode"])
	}
	if body["state_topic"] != "daikin/status/scheduler/mo-fr/enabled" || body["command_topic"] != "daikin/set/scheduler/mo-fr/enabled" {
		t.Errorf("schedule topics = %v / %v", body["state_topic"], body["command_topic"])
	}
}

// TestHamqttOriginIsAbsent pins the third library setting that would have been
// a silent 264-row diff. RenderComponent attaches an origin block whenever its
// name is non-empty; this bridge publishes none (F12, deferred past this step
// deliberately so it is not a payload addition inside the byte-equality proof).
func TestHamqttOriginIsAbsent(t *testing.T) {
	d := testDiscovery("en")
	lay := hamqttLayout{root: d.state}
	e, _ := d.pointEntity(lay, testPoint("room_temperature", "sensor"), "daikin_x", "Probe")
	if body := renderBody(t, d, e); body["origin"] != nil {
		t.Errorf("payload carries an origin block: %v", body["origin"])
	}
}

// renderBody renders one entity as the per-entity discovery body Home
// Assistant would receive.
func renderBody(t *testing.T, d *Discovery, e hamodel.Entity) map[string]any {
	t.Helper()
	dev := hamqttDevice(device{Identifiers: []string{"daikin_x"}, Name: "Probe", Manufacturer: "Daikin"})
	comp, err := discovery.RenderComponent(d.HamqttContext(), dev, e, discovery.Origin{})
	if err != nil {
		t.Fatalf("RenderComponent: %v", err)
	}
	raw, err := comp.EntityJSON()
	if err != nil {
		t.Fatalf("EntityJSON: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["platform"]; present {
		t.Error("the per-entity body carries a platform key, which Home Assistant declares on no platform")
	}
	return body
}
