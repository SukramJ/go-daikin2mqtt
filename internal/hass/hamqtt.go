// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"strings"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	"github.com/SukramJ/go-hamqtt/model"
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
// # The two library settings that would each have been a silent 264-row diff
//
//   - [discovery.RawEncoding]. The zero [discovery.Encoding] is
//     EnvelopeEncoding, which attaches a `value_template` reading
//     `value_json.value` to every entity that has a state topic. This bridge
//     publishes bare scalars.
//   - The zero [discovery.Origin] on [discovery.RenderComponent], which
//     attaches an `origin` block whenever its name is non-empty. This bridge
//     publishes none (F12, deliberately deferred past this step so it is not
//     a 264-row payload addition inside the byte-equality proof).

// hamqttLayout is this bridge's [hatopic.Layout]: a thin delegation to
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
// [hatopic.Default] is unusable here on every one of its four methods: its
// State inserts a bucket segment and has no /state leaf, its Command appends
// /set to that, its Availability names a per-device topic this bridge never
// writes, and its Bridge happens to agree only by coincidence.
type hamqttLayout struct{ root layout.Root }

var _ hatopic.Layout = hamqttLayout{}

// slot resolves a [model.Slot] to this bridge's topic family.
//
// Scope, Bucket and Path arity are deliberately NOT read, and that is a
// statement rather than an omission: this bridge's tree is exactly four levels
// — "<root>/<deviceID>/<embeddedID>/<topic>" — so a Slot that carried a scope
// or a bucket would have nowhere to put it, and silently dropping a segment
// moves a datapoint into another device's tree. [hamqttSlot] is the only
// constructor, it fills Address, Channel and a single-segment Path, and
// TestHamqttLayoutIgnoresOnlyWhatItDeclares asserts both halves: that a slot
// differing only in Bucket renders the same topic, and that no slot this path
// builds ever carries a scope or a multi-segment path.
func (l hamqttLayout) slot(s model.Slot) layout.Slot {
	return l.root.Slot(s.Address, s.Channel, strings.Join(s.Path, "/"))
}

// State implements [hatopic.Layout].
func (l hamqttLayout) State(s model.Slot) string { return l.slot(s).State() }

// Command implements [hatopic.Layout].
func (l hamqttLayout) Command(s model.Slot) string { return l.slot(s).Command() }

// Availability implements [hatopic.Layout] and deliberately renders NOTHING.
//
// This bridge has one availability level, the bridge LWT, and no per-device
// reachability topic at all (phase 8 measurement §4, decision F8). Returning a
// plausible-looking "<root>/<uid>/availability" would name a topic nobody
// writes, and under Home Assistant's default `all` mode every entity listing
// it is permanently unavailable with nothing in any log — which is the defect
// go-mtec2mqtt shipped.
//
// It cannot be reached as long as no description declares
// [model.LevelDevice]: [singularAvailability] refuses any component whose
// availability list is not exactly the bridge topic, so adopting the library's
// default would fail the render rather than publish an unreachable topic.
func (l hamqttLayout) Availability(model.Slot) string { return "" }

// Bridge implements [hatopic.Layout]: "<root>/bridge/status", the one topic
// all 264 payloads name.
func (l hamqttLayout) Bridge() string { return l.root.BridgeStatus() }

// Attributes is beyond [hatopic.Layout] — Home Assistant's
// `json_attributes_topic` has no Layout method — but it belongs here for the
// same reason the other four do: it is the fifth string internal/layout owns.
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
	}
	if e.build != nil {
		if err := e.build(ctx, comp); err != nil {
			return err
		}
	}
	return singularAvailability(comp)
}

// singularAvailability rewrites the library's `availability` LIST into the
// singular `availability_topic` + top-level payload keys this bridge's 264
// retained configs carry.
//
// Home Assistant accepts both spellings identically and an entity's registry
// keys are unaffected, so this is the one key group where phase 8's
// "byte-equal" is bought by a deliberate re-spelling rather than by the
// library's default path — named here, and only here, exactly as decision F8
// requires.
//
// It refuses anything that is not one plain bridge-level source, which is what
// keeps [model.BridgeOnly] load-bearing: the library's zero
// [model.Availability] resolves to {LevelBridge, LevelDevice}, so dropping
// BridgeOnly produces two entries and fails the render instead of silently
// publishing a second, never-written topic that would grey out all 264
// entities.
func singularAvailability(comp *discovery.Component) error {
	if len(comp.Availability) != 1 {
		return fmt.Errorf("hamqtt: want exactly one availability source (model.BridgeOnly), got %d", len(comp.Availability))
	}
	a := comp.Availability[0]
	if a.ValueTemplate != "" {
		return fmt.Errorf("hamqtt: availability source carries a value template %q", a.ValueTemplate)
	}
	comp.AvailabilityTopic = a.Topic
	comp.PayloadAvailable = a.PayloadAvailable
	comp.PayloadNotAvail = a.PayloadNotAvailable
	comp.Availability = nil
	// A mode beside no list is the one combination Home Assistant reads as a
	// contradiction, and this bridge publishes neither.
	comp.AvailabilityMode = ""
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
	return hamqttContext{StdContext: discovery.StdContext{
		Layout: lay,
		Lang:   lang,
		// Bare scalars on every state topic; see the file comment.
		Enc: discovery.RawEncoding,
	}}
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
	case "binary_sensor":
		desc.DeviceClass = model.DeviceClass(p.Entry.DeviceClass)
		fields = discovery.BinarySensorFields{PayloadOn: "true", PayloadOff: "false"}
	case "switch":
		writable()
		fields = discovery.SwitchFields{PayloadOn: "on", PayloadOff: "off", StateOn: "on", StateOff: "off"}
	case "select":
		writable()
		// The options are localized labels and the state is published as the
		// label too; [model.Enum] is the model's own spelling of exactly that
		// pairing, so the localization goes through the model rather than
		// being flattened to a []string by the caller.
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
		Basic: model.Basic{
			EntityKey:      p.Topic,
			EntityPlatform: hacatalog.Platform(p.Entry.Platform),
			Description:    desc,
			Binds:          binds,
		},
		idBase:     idBase,
		seed:       seed,
		attributes: lay.Attributes(slot),
		fields:     fields,
	}
	return e, true
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
	if len(ci.FanModes) > 0 {
		binds = append(binds, model.Binding{Role: roleFanMode, Slot: aux(FanModeTopic), Mode: model.ReadWrite})
	}
	if len(ci.SwingModes) > 0 {
		binds = append(binds, model.Binding{Role: roleSwingMode, Slot: aux(SwingModeTopic), Mode: model.ReadWrite})
	}
	if len(ci.SwingHorizontalModes) > 0 {
		binds = append(binds, model.Binding{Role: roleSwingHorizontalMode, Slot: aux(SwingHModeTopic), Mode: model.ReadWrite})
	}
	if len(ci.PresetModes) > 0 {
		binds = append(binds, model.Binding{Role: rolePresetMode, Slot: aux(PresetModeTopic), Mode: model.ReadWrite})
	}

	modes := []string{"off"}
	for _, v := range g.mode.Entry.Values {
		if m, mapped := daikinToHA[v.Value]; mapped {
			modes = append(modes, m)
		}
	}

	e := &renderEntity{
		Basic: model.Basic{
			EntityKey:      layout.ClimateTopic,
			EntityPlatform: hacatalog.Platform("climate"),
			Description: model.Description{
				Name:         model.L("Thermostat"),
				Availability: model.BridgeOnly(),
			},
			Binds: binds,
		},
		idBase: mainIdentifier(g.deviceID),
		seed:   info.Name,
		// The suppression key is "climate"; the entity id reads "thermostat".
		seedKey:    "thermostat",
		attributes: lay.Attributes(aux(layout.ClimateTopic)),
		suppresses: climateConsumedKeys(g),
	}
	e.build = func(ctx discovery.Context, comp *discovery.Component) error {
		f := discovery.ClimateFields{
			Modes:    modes,
			MinTemp:  g.setpoint.Min,
			MaxTemp:  g.setpoint.Max,
			TempStep: g.setpoint.Step,
		}
		for _, b := range e.Bindings() {
			switch b.Role {
			case roleMode:
				f.ModeStateTopic, f.ModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
			case roleTemperature:
				f.TemperatureStateTopic, f.TemperatureCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
			case roleCurrentTemperature:
				f.CurrentTemperatureTopic = ctx.StateTopic(b.Slot)
			case roleFanMode:
				f.FanModes = ci.FanModes
				f.FanModeStateTopic, f.FanModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
			case roleSwingMode:
				f.SwingModes = ci.SwingModes
				f.SwingModeStateTopic, f.SwingModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
			case roleSwingHorizontalMode:
				f.SwingHorizontalModes = ci.SwingHorizontalModes
				f.SwingHorizontalModeStateTopic, f.SwingHorizontalModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
			case rolePresetMode:
				f.PresetModes = ci.PresetModes
				f.PresetModeStateTopic, f.PresetModeCommandTopic = ctx.StateTopic(b.Slot), ctx.CommandTopic(b.Slot)
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
		Basic: model.Basic{
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
		},
		idBase: "daikin_schedule",
		// The entity id is the unique id verbatim, seeded from the slug frozen
		// at the schedule's creation rather than from its display name, so
		// renaming a schedule leaves switch.daikin_schedule_<slug> untouched.
		objectIDIsUniqueID: true,
		fields:             discovery.SwitchFields{PayloadOn: "on", PayloadOff: "off", StateOn: "on", StateOff: "off"},
	}
}
