// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"strings"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"
	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// This file is ADR 0070 phase 8 step 4: the parallel rendering path.
//
// It models this bridge's published surface as go-hamqtt's model — a
// [model.Device] per Home Assistant device, a [model.Entity] per entity with a
// [model.Description] and [model.Binding]s, an own [hatopic.Layout] and an own
// [discovery.Context] — and renders the SAME per-entity discovery payloads the
// hand-written builders in discovery.go, climate.go and schedule.go produce.
//
// It publishes nothing. Nothing in this file is reachable from
// [Discovery.Publish], [Discovery.PublishSchedules] or any coordinator path;
// the only callers are tests. The proof that the two paths agree is
// TestHamqttReproducesPublishedConfigs in internal/coordinator, which compares
// this path's output against the twelve pinned scenario goldens — the files,
// not the builders — without regenerating any of them.
//
// # What the library replaces, and what it does not
//
// The library replaces the RENDERING. It does not replace this bridge's domain
// logic: [Discovery.entityIdentity] still decides which Home Assistant device
// an entity belongs to and what its unique-id namespace is,
// [climateEligibleGroups] still decides which management points become a
// composite climate, [survivingPoints] still breaks the shared-sub-device tie,
// and the three normalisers ([slugify], [sanitize] and schedule.Slug, wrapped
// here and in [hamqttContext]) stay this bridge's own — decided in writing at
// ADR 0070 phase 8 step 2, finding F4. Nothing here reaches for
// [hatopic.Slug].
//
// # The library settings this bridge's surface rests on
//
//   - [discovery.StatusObjectEncoding] (0.14.0, openccu-loom ADR 0083). Every
//     status item is mqtt-smarthome 2.0's `{"val","ts","lc"}` object, so every
//     value template reads `value_json.val`; switches and binary sensors read
//     `{{ value_json.val | lower }}` against `true`/`false` payloads; a select
//     whose labels differ from its tokens gets the token/label template pair.
//     The composite climate spells its own per-role templates, because the
//     library projects only the plain value_template.
//   - [hatopic.SmartHomeLayout], through [hamqttLayout]. It is what makes the
//     bridge-level availability entry read `<name>/connected` at ≥ 2 and what
//     turns the runtime's Last Will into the plain `0`.
//
// The zero [discovery.Origin] on [discovery.RenderComponent] is still
// load-bearing for the per-entity render: it attaches an `origin` block
// whenever its name is non-empty, and the per-entity form never carried one.

// hamqttLayout is this bridge's [hatopic.SmartHomeLayout]: a thin delegation to
// internal/layout, so the library path and the daemon's own publish path
// cannot disagree about a topic.
//
// It delegates rather than reimplements on purpose. F3 of the phase 8
// measurement is that this bridge composed its state topic in twelve places
// and nothing compared them; internal/layout is step 2's fix, and a Layout
// that formatted its own strings would be the thirteenth. Pinned builder
// against builder by TestHamqttLayoutMatchesInternalLayout, which never reads
// testdata.
//
// [hatopic.SmartHome]'s own State and Command are not used: they would put the
// slot's bucket into the item path (`…/values/<topic>`), and this bridge's item
// is exactly `<deviceID>/<embeddedID>/<topic>`.
type hamqttLayout struct{ root layout.Root }

var _ hatopic.SmartHomeLayout = hamqttLayout{}

// slot resolves a [model.Slot] to this bridge's item.
//
// Scope, Bucket and Path arity are deliberately NOT read, and that is a
// statement rather than an omission: this bridge's item is exactly three
// levels — "<deviceID>/<embeddedID>/<topic>" — so a Slot that carried a scope
// or a bucket would have nowhere to put it, and silently dropping a segment
// moves a datapoint into another device's tree. [hamqttSlot] is the only
// constructor, it fills Address, Channel and a single-segment Path, and
// TestHamqttLayoutIgnoresOnlyWhatItDeclares asserts both halves: that a slot
// differing only in Bucket renders the same topic, and that no slot this path
// builds ever carries a scope or a multi-segment path.
func (l hamqttLayout) slot(s model.Slot) layout.Slot {
	return l.root.Slot(s.Address, s.Channel, strings.Join(s.Path, "/"))
}

// State implements [hatopic.Layout]: the slot's status item.
func (l hamqttLayout) State(s model.Slot) string { return l.slot(s).State() }

// Command implements [hatopic.Layout]: the slot's set item.
func (l hamqttLayout) Command(s model.Slot) string { return l.slot(s).Command() }

// Availability implements [hatopic.Layout] and deliberately renders NOTHING.
//
// The library resolves [model.LevelDevice] against a slot whose address is the
// Home Assistant device's UID — `daikin_<uuid>`, `daikin_outdoor_<serial>` —
// while this bridge's reachability item is keyed by the ONECTA device id whose
// topics the entity reads (`<name>/status/<uuid>/online`). The two are not the
// same string for a shared outdoor unit or a gateway, so no description
// declares LevelDevice and [renderEntity.BuildDiscovery] appends the online
// entry itself, from the point it was built from. Returning a plausible topic
// here would name one nobody writes, and under `availability_mode: all` every
// entity listing it would be permanently unavailable with nothing in any log.
func (l hamqttLayout) Availability(model.Slot) string { return "" }

// Bridge implements [hatopic.Layout]: `<name>/connected`, the one topic every
// payload's bridge-level availability entry names.
func (l hamqttLayout) Bridge() string { return l.root.Connected() }

// Connected implements [hatopic.SmartHomeLayout]; it equals [hamqttLayout.Bridge].
func (l hamqttLayout) Connected() string { return l.root.Connected() }

// Info implements [hatopic.SmartHomeLayout].
func (l hamqttLayout) Info() string { return l.root.Info() }

// Maintenance implements [hatopic.SmartHomeLayout].
func (l hamqttLayout) Maintenance(item ...string) string {
	return l.root.SmartHome().Maintenance(item...)
}

// Attributes is beyond [hatopic.Layout] — Home Assistant's
// `json_attributes_topic` has no Layout method — but it belongs here for the
// same reason the other four do: it is a string internal/layout owns.
func (l hamqttLayout) Attributes(s model.Slot) string { return l.slot(s).Attributes() }

// hamqttSlot is the only [model.Slot] constructor this path uses: the
// coordinate of one point's topic family.
func hamqttSlot(deviceID, embeddedID, topic string) model.Slot {
	return model.S(deviceID, embeddedID, model.BucketValues, topic)
}

// ---------------------------------------------------------------------------
// the entity
// ---------------------------------------------------------------------------

// renderEntity is one entity of the parallel rendering path.
//
// The identity fields are the composition inputs, not finished strings:
// [hamqttContext] renders them with this bridge's own normalisers, so a
// mutation to [sanitize], [slugify] or [entityObjectID] moves the rendered
// bytes exactly as it moves the published ones.
type renderEntity struct {
	model.Basic

	// idBase is the unique-id namespace the entity key hangs off —
	// "daikin_<deviceID>", "daikin_outdoor_<serial>",
	// "daikin_gateway_<serial>" or "daikin_schedule". The unique id is
	// sanitize(idBase + "_" + Key()).
	idBase string
	// seed is the LANGUAGE-INDEPENDENT entity-id seed (F2): a main device's
	// own name, or a shared sub-device's name composed with its ENGLISH
	// label. Never the localized display name.
	seed string
	// seedKey overrides the entity key in the entity-id seed. The composite
	// climate is the only user: its key is "climate" (the suppression and
	// component key) while its published entity id reads "…_thermostat".
	seedKey string
	// objectIDIsUniqueID makes the entity-id seed the unique id verbatim,
	// which is what the schedule switches publish. It is not the same string
	// as entityObjectID would build: schedule.Slug preserves a hyphen and
	// [slugify] folds it, so a schedule id containing one diverges.
	objectIDIsUniqueID bool

	// attributes is the json_attributes_topic, or "" for an entity that
	// publishes none (the schedule switches).
	attributes string
	// online is the reachability item of the ONECTA device whose topics the
	// entity reads, or "" for an entity of the daemon's own (the schedule
	// switches), which depends on `<name>/connected` alone.
	online string
	// fields is the platform Fields struct, or nil.
	fields any
	// suppresses names the sibling entity keys this one replaces.
	suppresses []string
	// build, when set, is run last and fills the composite keys only the
	// entity knows how to spell.
	build func(ctx discovery.Context, comp *discovery.Component) error
}

var (
	_ model.Entity      = (*renderEntity)(nil)
	_ model.Suppressor  = (*renderEntity)(nil)
	_ discovery.Builder = (*renderEntity)(nil)
)

// Suppresses implements [model.Suppressor].
func (e *renderEntity) Suppresses() []string { return e.suppresses }

// BuildDiscovery implements [discovery.Builder].
func (e *renderEntity) BuildDiscovery(ctx discovery.Context, comp *discovery.Component) error {
	if e.fields != nil {
		comp.Fields = e.fields
	}
	if e.attributes != "" {
		comp.JSONAttributesTopic = e.attributes
		comp.JSONAttributesTemplate = AttributesTemplate
	}
	if e.build != nil {
		if err := e.build(ctx, comp); err != nil {
			return err
		}
	}
	return smartHomeAvailability(comp, e.online)
}

// AttributesTemplate hands Home Assistant the attributes document out of its
// status object. The document is the item's `val`; without the template Home
// Assistant would show `val`, `ts` and `lc` as three attributes instead of
// `data_source`.
const AttributesTemplate = `{{ value_json.val | tojson }}`

// smartHomeAvailability completes the availability list ADR 0083 asks for:
// `<name>/connected` at ≥ 2, plus the device's `online` item, combined with
// `availability_mode: all`.
//
// The library renders the bridge entry itself (the layout is a
// [hatopic.SmartHomeLayout]); the device entry is appended here because only
// the entity knows which ONECTA device's item it reads — see
// [hamqttLayout.Availability]. Each entry carries exactly the four keys spec §8
// allows, through the library's own constructors.
//
// It refuses anything other than one connected-level bridge source from the
// library, which is what keeps [model.BridgeOnly] load-bearing: the library's
// zero [model.Availability] resolves to {LevelBridge, LevelDevice}, and with
// the layout rendering no device topic that would fail here instead of
// publishing an entry whose topic is empty.
func smartHomeAvailability(comp *discovery.Component, online string) error {
	if len(comp.Availability) != 1 {
		return fmt.Errorf("hamqtt: want exactly one bridge availability source (model.BridgeOnly), got %d", len(comp.Availability))
	}
	if want := discovery.ConnectedTemplate(discovery.ConnectedOperational); comp.Availability[0].ValueTemplate != want {
		return fmt.Errorf("hamqtt: bridge availability source reads %q, want %q", comp.Availability[0].ValueTemplate, want)
	}
	if online == "" {
		// One entry needs no combining rule.
		comp.AvailabilityMode = ""
		return nil
	}
	comp.Availability = append(comp.Availability, discovery.OnlineAvailability(online, discovery.StatusObjectEncoding))
	comp.AvailabilityMode = string(model.AvailabilityAll)
	return nil
}

// ---------------------------------------------------------------------------
// the context
// ---------------------------------------------------------------------------

// hamqttContext is [discovery.StdContext] with the three identity strings
// overridden onto this bridge's own normalisers.
//
// All three have to be overridden and none of them is decoration. The
// package's own [discovery.NodeID], [discovery.ObjectID] and
// [discovery.UniqueID] are built from [hatopic.Slug], which folds this
// bridge's ONECTA UUIDs at the hyphen and expands "ä" to "ae" where
// [slugify] — matching Home Assistant's own slugify, as discovery.go says at
// the point of definition — expands it to "a". Home Assistant has no migration
// path for any of the three.
type hamqttContext struct {
	discovery.StdContext
}

var _ discovery.Context = hamqttContext{}

// newHamqttContext builds the context for one language.
func newHamqttContext(lay hamqttLayout, lang string) hamqttContext {
	return hamqttContext{
		Layout: lay,
		Lang:   lang,
		// mqtt-smarthome status objects on every status item; see the file
		// comment.
		Enc: discovery.StatusObjectEncoding,
	}
}

// UniqueID implements [discovery.Context]: sanitize(idBase + "_" + key).
//
// [sanitize] is the third of this bridge's normalisers and the one most easily
// missed, because it is not a slug: it PRESERVES case and keeps "-", which is
// what leaves the ONECTA UUID's four hyphens standing in
// "daikin_809d41d9-4d42-45fa-af6a-84b512143672_room_temperature".
func (c hamqttContext) UniqueID(_ *model.Device, e model.Entity) string {
	re, ok := e.(*renderEntity)
	if !ok {
		return ""
	}
	return sanitize(re.idBase + "_" + e.Key())
}

// ObjectID implements [discovery.Context]: the `default_entity_id` seed.
func (c hamqttContext) ObjectID(dev *model.Device, e model.Entity) string {
	re, ok := e.(*renderEntity)
	if !ok {
		return ""
	}
	if re.objectIDIsUniqueID {
		return c.UniqueID(dev, e)
	}
	key := e.Key()
	if re.seedKey != "" {
		key = re.seedKey
	}
	return entityObjectID(re.seed, key)
}

// NodeID implements [discovery.Context].
//
// Nothing in this bridge's published surface contains a node id — the config
// topic is four segments (F5) — so this string reaches no retained topic
// today. It is spelled with [sanitize], the same normaliser the device
// identifier it is derived from goes through, so that the bundle step 6
// renders is addressed consistently with the identity it carries rather than
// with [hatopic.Slug]'s different folding.
func (c hamqttContext) NodeID(dev *model.Device) string { return sanitize(dev.UID()) }

// ---------------------------------------------------------------------------
// the device
// ---------------------------------------------------------------------------

// hamqttDevice converts one of this bridge's HA device blocks into a
// [model.Device], through the library's own [discovery.DeviceFromInfo] rather
// than by hand: the conversion exists for exactly this, the middle of a
// migration where the blocks are still harvested the old way.
func hamqttDevice(d device) *model.Device {
	info := discovery.DeviceInfo{
		Identifiers:      d.Identifiers,
		Connections:      d.Connections,
		Name:             d.Name,
		Manufacturer:     d.Manufacturer,
		Model:            d.Model,
		ModelID:          d.ModelID,
		SWVersion:        d.SWVersion,
		HWVersion:        d.HWVersion,
		SerialNumber:     d.SerialNumber,
		SuggestedArea:    d.SuggestedArea,
		ConfigurationURL: d.ConfigurationURL,
		ViaDevice:        d.ViaDevice,
	}
	return discovery.DeviceFromInfo(info)
}

// ---------------------------------------------------------------------------
// the render
// ---------------------------------------------------------------------------

// HamqttContext returns the [discovery.Context] this bridge renders with.
// Exported so a test can drive [discovery.Render] and the publisher's legacy
// topic forms against the same context the byte-equality proof uses, rather
// than against a second one that might disagree.
func (d *Discovery) HamqttContext() discovery.Context {
	return newHamqttContext(hamqttLayout{root: d.state}, d.lang)
}

// HamqttDevice is one rendered Home Assistant device: the model device and the
// entities that belong to it, suppression NOT yet applied.
type HamqttDevice struct {
	Device   *model.Device
	Entities []model.Entity
}

// HamqttConfig is one rendered per-entity discovery config.
type HamqttConfig struct {
	Topic    string
	Platform string
	Body     []byte
}

// RenderHamqtt models this bridge's entity set as go-hamqtt's model and
// renders the per-entity discovery configs — the same ones [Discovery.Publish]
// would publish — WITHOUT publishing anything.
//
// The inputs are exactly [Discovery.Publish]'s, so the two paths are fed from
// one place and a divergence is a divergence in rendering rather than in what
// was rendered.
func (d *Discovery) RenderHamqtt(points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) ([]HamqttConfig, error) {
	devs, err := d.HamqttModel(points, infos, climateInfos)
	if err != nil {
		return nil, err
	}
	return d.renderDevices(devs)
}

// RenderHamqttSchedules is [RenderHamqtt] for the weekly scheduler's switches,
// which live on the daemon's own Home Assistant device and are built from a
// schedule list rather than from points.
func (d *Discovery) RenderHamqttSchedules(schedules []ScheduleInfo, configURL string) ([]HamqttConfig, error) {
	devs, err := d.HamqttScheduleModel(schedules, configURL)
	if err != nil {
		return nil, err
	}
	return d.renderDevices(devs)
}

// renderDevices applies suppression and renders each surviving entity as a
// standalone per-entity config.
//
// [model.ApplySuppression] is what removes the composite climate's three
// consumed controls, and it is deliberately the LIBRARY's suppression rather
// than this bridge's `consumed` set: the entities are all handed over and the
// model decides, which is the thing ADR 0070 §3.5 nominates this bridge to
// prove.
func (d *Discovery) renderDevices(devs []HamqttDevice) ([]HamqttConfig, error) {
	lay := hamqttLayout{root: d.state}
	ctx := newHamqttContext(lay, d.lang)

	var out []HamqttConfig
	for _, hd := range devs {
		for _, e := range model.ApplySuppression(hd.Entities) {
			// The zero Origin is load-bearing: RenderComponent attaches an
			// origin block whenever its name is non-empty, and this bridge
			// publishes none (F12).
			comp, err := discovery.RenderComponent(ctx, hd.Device, e, discovery.Origin{})
			if err != nil {
				return nil, err
			}
			body, err := comp.EntityJSON()
			if err != nil {
				return nil, err
			}
			out = append(out, HamqttConfig{
				// The config topic comes from this bridge's ONE builder, the
				// same call [Discovery.buildConfig] makes. Which library
				// LegacyTopicFunc reproduces it is a separate question,
				// answered against the pinned topics by
				// TestHamqttLegacyTopicForm.
				Topic:    d.ConfigTopic(string(comp.Platform), comp.UniqueID),
				Platform: string(comp.Platform),
				Body:     body,
			})
		}
	}
	return out, nil
}

// HamqttModel builds the [model.Device] / [model.Entity] graph for a point
// set. Exported so a test can assert over the model itself — the bindings, the
// suppression, the availability declaration — rather than only over the bytes
// it renders to.
func (d *Discovery) HamqttModel(points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) ([]HamqttDevice, error) {
	lay := hamqttLayout{root: d.state}

	// Order is by first appearance so the output is deterministic; the
	// comparison is by topic, so it does not depend on this.
	var order []string
	byID := map[string]*HamqttDevice{}
	add := func(dev device, e model.Entity) error {
		if len(dev.Identifiers) == 0 || dev.Identifiers[0] == "" {
			return fmt.Errorf("hamqtt: device block with no identifier for entity %q", e.Key())
		}
		id := dev.Identifiers[0]
		hd, ok := byID[id]
		if !ok {
			hd = &HamqttDevice{Device: hamqttDevice(dev)}
			byID[id] = hd
			order = append(order, id)
		}
		hd.Entities = append(hd.Entities, e)
		return nil
	}

	// The composite climate entities first, so their suppression keys are
	// declared before the entities they replace are added.
	groups := climateEligibleGroups(points)
	for _, g := range groups {
		info := infos[g.deviceID]
		e := d.climateEntity(lay, g, info, climateInfos[g.deviceID+"|"+g.embeddedID])
		if err := add(d.deviceBlock(g.deviceID, info), e); err != nil {
			return nil, err
		}
	}

	// Every catalogue-derived entity, INCLUDING the three the composite
	// replaces: they are handed to the model and [model.ApplySuppression]
	// removes them, rather than being filtered out before the model sees
	// them. Passing nil for `consumed` is what makes that possible; the
	// publish path passes the climate's consumed set instead.
	survives := survivingPoints(d, points, infos, nil)
	for i := range points {
		if !survives[i] {
			continue
		}
		p := points[i]
		base, dev, seed := d.entityIDBase(p, infos[p.DeviceID])
		e, ok := d.pointEntity(lay, p, base, seed)
		if !ok {
			continue
		}
		if err := add(dev, e); err != nil {
			return nil, err
		}
	}

	out := make([]HamqttDevice, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// HamqttScheduleModel builds the daemon's own scheduler device and one switch
// entity per weekly schedule.
func (d *Discovery) HamqttScheduleModel(schedules []ScheduleInfo, configURL string) ([]HamqttDevice, error) {
	dev := schedulerDevice(configURL)
	hd := HamqttDevice{Device: hamqttDevice(dev)}
	for _, s := range schedules {
		if s.ID == "" {
			continue
		}
		hd.Entities = append(hd.Entities, d.scheduleEntity(s))
	}
	if len(hd.Entities) == 0 {
		return nil, nil
	}
	return []HamqttDevice{hd}, nil
}

// pointEntity models one catalogue-derived point.
//
// The platform switch mirrors [Discovery.buildConfig] key for key, and the
// difference is where each key goes: what Home Assistant declares on nearly
// every platform becomes [model.Description] vocabulary and is projected by
// the library, what one platform spells its own way becomes a Fields struct.
// A key emitted on the wrong platform is dropped by Home Assistant in silence,
// so this split is the library's job and not a formatting preference.
func (d *Discovery) pointEntity(lay hamqttLayout, p process.Point, idBase, seed string) (*renderEntity, bool) {
	slot := hamqttSlot(p.DeviceID, p.EmbeddedID, p.Topic)

	desc := model.Description{
		Name:         model.Localized{Default: p.Entry.Name, Lang: map[string]string{"de": p.Entry.NameDE}},
		Icon:         p.Entry.Icon,
		Category:     hacatalog.EntityCategory(p.Entry.Category),
		Availability: model.BridgeOnly(),
	}

	// Read on every platform, write only where the published config carries a
	// command topic. The library then projects each onto the platforms whose
	// schema declares the key — which is what drops `state_topic` from the
	// button without this bridge having to clear it afterwards.
	binds := []model.Binding{{Role: model.RoleState, Slot: slot, Mode: model.Read}}
	writable := func() {
		binds = append(binds, model.Binding{Role: model.RoleCommand, Slot: slot, Mode: model.Write})
	}

	var fields any
	switch p.Entry.Platform {
	case "sensor":
		desc.Unit = model.Unit(p.Unit)
		desc.DeviceClass = model.DeviceClass(p.Entry.DeviceClass)
		desc.StateClass = hacatalog.StateClass(p.Entry.StateClass)
		desc.ValueTemplate = SensorValueTemplate
	case "binary_sensor":
		desc.DeviceClass = model.DeviceClass(p.Entry.DeviceClass)
		fields = discovery.BinarySensorFields{PayloadOn: discovery.PayloadTrue, PayloadOff: discovery.PayloadFalse}
	case "switch":
		writable()
		// The status item carries a JSON boolean, which the value template
		// lowers to `true`/`false`; the command is the same word, which the
		// set path reads with spec §5.3's boolean conversions.
		fields = boolSwitchFields()
	case "select":
		writable()
		// The options are localized labels while the status item carries the
		// API token; [model.Enum] is the model's own spelling of exactly that
		// pairing, and under the status-object encoding the library derives
		// the token→label value template and the label→token command template
		// from it.
		desc.Options = entryEnum(p.Entry)
	case "number":
		writable()
		desc.Unit = model.Unit(p.Unit)
		desc.DeviceClass = model.DeviceClass(p.Entry.DeviceClass)
		desc.Min, desc.Max, desc.Step = p.Min, p.Max, p.Step
	case "button":
		writable()
		fields = discovery.ButtonFields{PayloadPress: "PRESS"}
	default:
		return nil, false
	}

	e := &renderEntity{
		EntityKey:      p.Topic,
		EntityPlatform: hacatalog.Platform(p.Entry.Platform),
		Description:    desc,
		Binds:          binds,
		idBase:         idBase,
		seed:           seed,
		attributes:     lay.Attributes(slot),
		online:         lay.root.Online(p.DeviceID),
		fields:         fields,
	}
	if p.Entry.Platform == "sensor" && len(p.Entry.Values) > 0 {
		// A sensor with catalogue values (the scheduler's "no block in force")
		// publishes the token and shows the label. It is no enum sensor —
		// its other states are the operator's own schedule names — so it has
		// no `options` list, and the library derives no template for it: the
		// mapping is stated here, falling back to the value itself for every
		// state the catalogue does not name.
		enum := entryEnum(p.Entry)
		e.build = func(ctx discovery.Context, comp *discovery.Component) error {
			comp.ValueTemplate = sensorEnumValueTemplate(enum, ctx.Language())
			return nil
		}
	}
	return e, true
}

// sensorValueGuard is the condition under which a sensor's status item holds
// a value: a status object whose `val` is present, not null and not the empty
// string. Every operand short-circuits before the next one touches an
// undefined, so Home Assistant never logs a template error for an empty
// payload (a cleared item) or a document without `val`.
const sensorValueGuard = `value_json is defined and value_json.val is defined and value_json.val is not none and value_json.val != ''`

// SensorValueTemplate reads a sensor's value out of its status object and
// renders `None` when there is none.
//
// `None` is the one payload Home Assistant's MQTT sensor turns into an unknown
// state for every sensor kind — plain, numeric, enum and timestamp alike — and
// it is checked before any of them parses the value
// (homeassistant/components/mqtt/sensor.py, `payload == PAYLOAD_NONE`). The
// plain `{{ value_json.val }}` cannot do that: an empty payload leaves
// value_json undefined, the render fails, and the sensor keeps showing the
// value the item was cleared from (the render error makes it return before the
// state is touched) — which is how 0.14.0 kept a cleared error code or the
// next change of a disabled schedule on screen until Home Assistant restarted.
const SensorValueTemplate = `{% if ` + sensorValueGuard + ` %}{{ value_json.val }}{% else %}None{% endif %}`

// sensorEnumValueTemplate is [SensorValueTemplate] for a sensor whose catalogue
// names some of its states: a named token shows as its label, anything else as
// itself, and no value as `None`.
func sensorEnumValueTemplate(enum *model.Enum, lang string) string {
	var b strings.Builder
	b.WriteString(`{% set m = {`)
	for i, code := range enum.Codes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(discovery.JinjaQuote(code) + ": " + discovery.JinjaQuote(enum.Label(code, lang)))
	}
	b.WriteString(`} %}{% if ` + sensorValueGuard + ` %}{{ m.get(value_json.val, value_json.val) }}{% else %}None{% endif %}`)
	return b.String()
}

// boolSwitchFields are the payloads of every switch this bridge publishes: the
// spec §5.1 boolean spellings, which is what `{{ value_json.val | lower }}`
// renders a JSON boolean as.
func boolSwitchFields() discovery.SwitchFields {
	return discovery.SwitchFields{PayloadOn: discovery.PayloadTrue, PayloadOff: discovery.PayloadFalse}
}

// entryEnum is a catalogue entry's value list as a [model.Enum], preserving
// declaration order — which is the order Home Assistant shows a select's
// options — and reproducing the per-language fallback
// [catalog.Entry.LocalizedLabel] implements: the German label, else the
// English one, else the raw API code.
//
// [model.Enum] renders that fallback by itself: a code with no [model.Localized]
// text in the requested language falls back to Default, and a code whose
// Localized is empty altogether falls back to the code.
func entryEnum(entry catalog.Entry) *model.Enum {
	if len(entry.Values) == 0 {
		return nil
	}
	enum := &model.Enum{
		Codes:  make([]string, 0, len(entry.Values)),
		Labels: make(map[string]model.Localized, len(entry.Values)),
	}
	for _, v := range entry.Values {
		enum.Codes = append(enum.Codes, v.Value)
		enum.Labels[v.Value] = model.Localized{Default: v.Label, Lang: map[string]string{"de": v.LabelDE}}
	}
	return enum
}

// climateEntity models the composite climate — the entity ADR 0070 §3.5
// nominates this bridge to prove, and the only one in the programme.
//
// It is a [model.Suppressor] over the three controls it replaces, a set of
// role [model.Binding]s over seven slots of which FIVE are synthetic (backed
// by no catalogue entry and no register, composed by the coordinator), and a
// [discovery.Builder] that spells the per-role keys Home Assistant's climate
// platform uses instead of the plain state_topic/command_topic pair.
//
// The five synthetic slots are the case ADR 0070 §3.5 says must need no
// runtime special case, and they do not: they are ordinary [model.Slot]s on
// the same device and management point, with a synthetic leaf, resolved by the
// same [hamqttLayout] as everything else. A composite is bindings plus a
// builder, not a code path.
func (d *Discovery) climateEntity(lay hamqttLayout, g *climateGroup, info DeviceInfo, ci ClimateInfo) *renderEntity {
	aux := func(suffix string) model.Slot { return hamqttSlot(g.deviceID, g.embeddedID, suffix) }
	pointOf := func(p *process.Point) model.Slot { return hamqttSlot(p.DeviceID, p.EmbeddedID, p.Topic) }

	binds := []model.Binding{
		{Role: roleMode, Slot: aux(HVACModeTopic), Mode: model.ReadWrite},
		{Role: roleTemperature, Slot: pointOf(g.setpoint), Mode: model.ReadWrite},
	}
	if g.current != nil {
		binds = append(binds, model.Binding{Role: roleCurrentTemperature, Slot: pointOf(g.current), Mode: model.Read})
	}
	if hasCodes(ci.FanModes) {
		binds = append(binds, model.Binding{Role: roleFanMode, Slot: aux(FanModeTopic), Mode: model.ReadWrite})
	}
	if hasCodes(ci.SwingModes) {
		binds = append(binds, model.Binding{Role: roleSwingMode, Slot: aux(SwingModeTopic), Mode: model.ReadWrite})
	}
	if hasCodes(ci.SwingHorizontalModes) {
		binds = append(binds, model.Binding{Role: roleSwingHorizontalMode, Slot: aux(SwingHModeTopic), Mode: model.ReadWrite})
	}
	if hasCodes(ci.PresetModes) {
		binds = append(binds, model.Binding{Role: rolePresetMode, Slot: aux(PresetModeTopic), Mode: model.ReadWrite})
	}

	modes := []string{"off"}
	for _, v := range g.mode.Entry.Values {
		if m, mapped := daikinToHA[v.Value]; mapped {
			modes = append(modes, m)
		}
	}

	e := &renderEntity{
		EntityKey:      layout.ClimateTopic,
		EntityPlatform: hacatalog.Platform("climate"),
		Description: model.Description{
			Name:         model.L("Thermostat"),
			Availability: model.BridgeOnly(),
		},
		Binds:  binds,
		idBase: mainIdentifier(g.deviceID),
		seed:   info.Name,
		// The suppression key is "climate"; the entity id reads "thermostat".
		seedKey:    "thermostat",
		attributes: lay.Attributes(aux(layout.ClimateTopic)),
		online:     lay.root.Online(g.deviceID),
		suppresses: climateConsumedKeys(g),
	}
	e.build = func(ctx discovery.Context, comp *discovery.Component) error {
		f := discovery.ClimateFields{
			Modes:    modes,
			MinTemp:  g.setpoint.Min,
			MaxTemp:  g.setpoint.Max,
			TempStep: g.setpoint.Step,
		}
		lang := ctx.Language()
		// Every role key reads a status object, so every one needs its own
		// template: Home Assistant's climate platform has no shared
		// value_template, and a role topic without one would show the whole
		// `{"val":…}` document as the state. The mode and both temperatures
		// carry Home Assistant's own tokens and plain numbers; fan, swing and
		// preset carry the API tokens and map them onto the listed labels.
		for _, b := range e.Bindings() {
			switch b.Role {
			case roleMode:
				f.ModeStateTopic, f.ModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				f.ModeStateTemplate = discovery.StatusValueTemplate
			case roleTemperature:
				f.TemperatureStateTopic, f.TemperatureCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				f.TemperatureStateTemplate = discovery.StatusValueTemplate
			case roleCurrentTemperature:
				f.CurrentTemperatureTopic = ctx.StateTopic(b.Slot)
				f.CurrentTemperatureTemplate = discovery.StatusValueTemplate
			case roleFanMode:
				f.FanModes = ci.FanModes.Options(lang)
				f.FanModeStateTopic, f.FanModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				f.FanModeStateTemplate, f.FanModeCommandTemplate = climateEnumTemplates(ci.FanModes, lang)
			case roleSwingMode:
				f.SwingModes = ci.SwingModes.Options(lang)
				f.SwingModeStateTopic, f.SwingModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				f.SwingModeStateTemplate, f.SwingModeCommandTemplate = climateEnumTemplates(ci.SwingModes, lang)
			case roleSwingHorizontalMode:
				f.SwingHorizontalModes = ci.SwingHorizontalModes.Options(lang)
				f.SwingHorizontalModeStateTopic, f.SwingHorizontalModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				f.SwingHorizontalModeStateTemplate, f.SwingHorizontalModeCommandTemplate = climateEnumTemplates(ci.SwingHorizontalModes, lang)
			case rolePresetMode:
				f.PresetModes = ci.PresetModes.Options(lang)
				f.PresetModeStateTopic, f.PresetModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
				// `none` is a state but never an option (Home Assistant
				// rejects it in preset_modes); the templates pass an
				// unlisted value through unchanged, so it still reads.
				f.PresetModeValueTemplate, f.PresetModeCommandTemplate = climateEnumTemplates(ci.PresetModes, lang)
			default:
				return fmt.Errorf("hamqtt: climate binding on unknown role %q", b.Role)
			}
		}
		comp.Fields = f
		return nil
	}
	return e
}

// The composite climate's binding roles. They are the model's own vocabulary
// for "which part of the entity does this datapoint feed", not topic strings —
// [renderEntity.build] is the only thing that knows how Home Assistant's
// climate platform spells each of them.
const (
	roleMode                = "mode"
	roleTemperature         = "temperature"
	roleCurrentTemperature  = "current_temperature"
	roleFanMode             = "fan_mode"
	roleSwingMode           = "swing_mode"
	roleSwingHorizontalMode = "swing_horizontal_mode"
	rolePresetMode          = "preset_mode"
)

// scheduleEntity models one weekly schedule's enable switch.
func (d *Discovery) scheduleEntity(s ScheduleInfo) *renderEntity {
	slot := hamqttSlot(layout.SchedulerDeviceID, s.ID, layout.EnabledTopic)
	return &renderEntity{
		EntityKey:      s.ID,
		EntityPlatform: hacatalog.Platform("switch"),
		Description: model.Description{
			// A schedule name is the operator's own text, published
			// verbatim in every language.
			Name:         model.L(s.Name),
			Icon:         "mdi:calendar-check",
			Category:     hacatalog.EntityCategory("config"),
			Availability: model.BridgeOnly(),
		},
		Binds: []model.Binding{
			{Role: model.RoleState, Slot: slot, Mode: model.Read},
			{Role: model.RoleCommand, Slot: slot, Mode: model.Write},
		},
		idBase: "daikin_schedule",
		// The entity id is the unique id verbatim, seeded from the slug frozen
		// at the schedule's creation rather than from its display name, so
		// renaming a schedule leaves switch.daikin_schedule_<slug> untouched.
		objectIDIsUniqueID: true,
		// The same payloads as every device switch: the status item carries
		// the schedule's enabled flag as a JSON boolean, which the library's
		// value template lowers to `true`/`false`. Until 0.14.1 this said
		// `on`/`off`, which no rendered state matched, so Home Assistant
		// dropped every update and the switch stayed unknown. The command is
		// the same word, read by handleSchedulerWrite with spec §5.3's boolean
		// conversions (which also still take `on`/`off`).
		fields: boolSwitchFields(),
	}
}

// --- ADR 0070 phase 8 step 5: what the RUNTIME reads ------------------------

// Layout is the go-hamqtt [hatopic.SmartHomeLayout] for the instance named
// stateRoot.
//
// Exported at step 5 because publisher.Config takes it: with a Layout set, the
// runtime derives Config.StatusTopic from Layout.Bridge() and refuses a
// StatusTopic that disagrees with it. That is what makes the connected level,
// the Last Will and the bridge availability entry of every discovery payload
// one string by construction rather than by three literals that happen to
// match — the drift go-mtec2mqtt shipped, where a will nobody reads is
// indistinguishable from no will at all. Being a SmartHomeLayout is what makes
// the runtime write `0`/`1`/`2` there rather than online/offline, and what
// publisher.Instance takes for `<name>/info` and the maintenance topics.
func Layout(stateRoot string) hatopic.SmartHomeLayout {
	return hamqttLayout{root: layout.New(stateRoot)}
}

// LegacyConfigTopicForms is [LegacyConfigTopicForm] as the value
// publisher.Config.LegacyEntityTopics takes.
//
// Stated at step 5 although nothing publishes a bundle yet, because the field
// REPLACES the library's default rather than extending it: an unstated list is
// the five-segment publisher.LegacyTopicWithNodeID, which reproduces 0 of this
// bridge's 264 config topics and would therefore retract nothing at step 6 —
// leaving every per-entity config retained under the bundle and Home Assistant
// refusing the document with a single WARNING line. Saying it here means step 6
// cannot forget it, and publisher.Runtime.LegacyForms() makes the choice
// assertable (TestRuntimeStatesTheLegacyTopicForm) and visible in the
// publisher.legacy_forms boot log line.
func LegacyConfigTopicForms() []publisher.LegacyTopicFunc {
	return []publisher.LegacyTopicFunc{publisher.LegacyTopicByUniqueID}
}
