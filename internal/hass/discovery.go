// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package hass builds and publishes Home Assistant MQTT discovery configs
// for the resolved Daikin data points, so entities appear automatically.
package hass

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// Discovery publishes retained HA MQTT discovery configs.
type Discovery struct {
	baseTopic string      // e.g. "homeassistant"
	state     layout.Root // the state-plane topic layout, rooted at e.g. "daikin"
	lang      string
	pub       mqtt.Publisher

	mu sync.RWMutex
	// owned is the set of state-plane device segments this instance writes —
	// see [Discovery.ClaimDevices]. Empty until the first resolved poll, and
	// an empty set claims nothing.
	owned map[string]bool
}

// New returns a Discovery publisher. baseTopic is the HA discovery prefix,
// stateRoot the MQTT topic root the daemon publishes state under.
func New(baseTopic, stateRoot, lang string, pub mqtt.Publisher) *Discovery {
	return &Discovery{baseTopic: baseTopic, state: layout.New(stateRoot), lang: lang, pub: pub}
}

// SubDevice is the metadata for an auxiliary Daikin component (gateway or
// outdoor unit) that is surfaced as its own Home Assistant device, linked to
// the main device via via_device.
type SubDevice struct {
	Model        string
	ModelID      string
	SWVersion    string
	SerialNumber string
	MAC          string
}

// DeviceInfo is the per-Daikin-device metadata used to build rich Home
// Assistant device-registry entries. The main fields describe the primary
// (indoor/climate) device; Gateway and Outdoor, when set, are emitted as
// separate HA devices nested under the main one.
type DeviceInfo struct {
	Name          string // friendly name (e.g. the climate "name" characteristic)
	Model         string // descriptive model (e.g. indoor unit modelInfo)
	ModelID       string // device model code (e.g. "dx4")
	SWVersion     string // indoor unit software version
	HWVersion     string // hardware revision, if known
	SerialNumber  string // indoor unit serial number
	SuggestedArea string // optional HA area suggestion
	// ConfigurationURL overrides the default (ONECTA) device link. In local mode
	// it points at the unit's Faikin web UI, mirroring Faikin's own discovery.
	ConfigurationURL string
	Gateway          *SubDevice
	Outdoor          *SubDevice
}

// device is the HA device grouping. Fields mirror Home Assistant's
// device-registry schema.
type device struct {
	Identifiers      []string    `json:"identifiers"`
	Name             string      `json:"name"`
	Manufacturer     string      `json:"manufacturer"`
	Model            string      `json:"model,omitempty"`
	ModelID          string      `json:"model_id,omitempty"`
	SWVersion        string      `json:"sw_version,omitempty"`
	HWVersion        string      `json:"hw_version,omitempty"`
	SerialNumber     string      `json:"serial_number,omitempty"`
	ConfigurationURL string      `json:"configuration_url,omitempty"`
	SuggestedArea    string      `json:"suggested_area,omitempty"`
	ViaDevice        string      `json:"via_device,omitempty"`
	Connections      [][2]string `json:"connections,omitempty"`
}

// configurationURL points operators at the Daikin ONECTA web app.
const configurationURL = "https://onecta.daikineurope.com"

// RefreshTopic is the synthetic topic of the manual cloud-refresh button. It is
// not a device characteristic: a press on
// <root>/<deviceID>/<embeddedID>/refresh/set makes the coordinator run a poll
// cycle immediately instead of waiting for the next scheduled one.
const RefreshTopic = "refresh"

// mainIdentifier returns the HA identifier of a device's main entry.
func mainIdentifier(deviceID string) string { return "daikin_" + deviceID }

// deviceBlock builds the HA device-registry block for the main (indoor /
// climate) Daikin device.
func (d *Discovery) deviceBlock(deviceID string, info DeviceInfo) device {
	cu := configurationURL
	if info.ConfigurationURL != "" {
		cu = info.ConfigurationURL
	}
	return device{
		Identifiers:      []string{mainIdentifier(deviceID)},
		Name:             orDefault(info.Name, "Daikin "+orDefault(info.ModelID, deviceID)),
		Manufacturer:     "Daikin",
		Model:            info.Model,
		ModelID:          info.ModelID,
		SWVersion:        info.SWVersion,
		HWVersion:        info.HWVersion,
		SerialNumber:     info.SerialNumber,
		SuggestedArea:    info.SuggestedArea,
		ConfigurationURL: cu,
	}
}

// subDeviceBlock builds a nested HA device (gateway / outdoor unit) linked to
// the main device via via_device. suffix disambiguates the identifier;
// labelEN/labelDE are appended to the base name.
func (d *Discovery) subDeviceBlock(deviceID, suffix, labelEN, labelDE, baseName string, sub *SubDevice) (dev device, seed string) {
	label := labelEN
	if d.lang == "de" && labelDE != "" {
		label = labelDE
	}
	base := orDefault(baseName, "Daikin "+deviceID)
	// seed is the same name with the ENGLISH label, always. See [entityObjectID].
	seed = base + " " + labelEN
	dev = device{
		Identifiers:      []string{mainIdentifier(deviceID) + "_" + suffix},
		Name:             base + " " + label,
		Manufacturer:     "Daikin",
		ViaDevice:        mainIdentifier(deviceID),
		ConfigurationURL: configurationURL,
	}
	if sub != nil {
		dev.Model = sub.Model
		dev.ModelID = sub.ModelID
		dev.SWVersion = sub.SWVersion
		dev.SerialNumber = sub.SerialNumber
		if sub.MAC != "" {
			dev.Connections = [][2]string{{"mac", sub.MAC}}
		}
	}
	return dev, seed
}

// sharedSubDevice builds an HA device for an auxiliary component (gateway /
// outdoor unit), keyed by its serial so identical serials across the API
// devices of a multi-split system deduplicate to a single HA device. viaDevice
// links it under a parent device when it belongs to one (per-unit gateways
// nest under their indoor unit); pass "" for genuinely shared components with
// no single parent (e.g. one outdoor unit serving several indoor units).
func (d *Discovery) sharedSubDevice(identifier, viaDevice, labelEN, labelDE, baseName string, sub *SubDevice) (dev device, seed string) {
	label := labelEN
	if d.lang == "de" && labelDE != "" {
		label = labelDE
	}
	// Name the device after its associated unit when known (e.g. "Gateway
	// Wohnzimmer") so multiple gateways are distinguishable; fall back to a
	// generic name for truly shared components (e.g. one outdoor unit).
	compose := func(l string) string {
		if baseName != "" {
			return l + " " + baseName
		}
		return "Daikin " + l
	}
	name := compose(label)
	// seed is the same name with the ENGLISH label, always. See [entityObjectID].
	seed = compose(labelEN)
	dev = device{
		Identifiers:      []string{identifier},
		Name:             name,
		Manufacturer:     "Daikin",
		ViaDevice:        viaDevice,
		ConfigurationURL: configurationURL,
	}
	if sub != nil {
		dev.Model = sub.Model
		dev.ModelID = sub.ModelID
		dev.SWVersion = sub.SWVersion
		dev.SerialNumber = sub.SerialNumber
		if sub.MAC != "" {
			dev.Connections = [][2]string{{"mac", sub.MAC}}
		}
	}
	return dev, seed
}

// entityIdentity returns the HA unique_id, the device block, and the
// LANGUAGE-INDEPENDENT name the entity id is seeded from, for a point.
//
// Gateways have per-unit serials (one per indoor unit), so they are keyed by
// serial and nested under their indoor unit (via_device → main). Outdoor units
// are commonly shared across the indoor units of a multi-split system
// (identical serials), so when a serial is known they deduplicate to a single
// standalone HA device with no parent; without a serial they fall back to a
// per-device nested sub-device (via_device → main).
//
// seed is returned separately from dev.Name because the two auxiliary device
// kinds compose their display name from a TRANSLATED label, and seeding an
// entity id from that makes the entity id move with LANGUAGE (F2). For a main
// device the two are the same string: its name is the operator's own text.
func (d *Discovery) entityIdentity(p process.Point, info DeviceInfo) (uid string, dev device, seed string) {
	base, dev, seed := d.entityIDBase(p, info)
	return sanitize(base + "_" + p.Topic), dev, seed
}

// entityIDBase is [Discovery.entityIdentity] without the final composition:
// it returns the unique-id NAMESPACE the entity key hangs off, rather than the
// finished unique id, so that the ADR 0070 phase 8 step 4 rendering path can
// compose the same string through a go-hamqtt discovery.Context instead of
// being handed a ready-made one. entityIdentity is sanitize(base + "_" +
// p.Topic) and nothing else, which is what keeps the two spellings one
// spelling.
func (d *Discovery) entityIDBase(p process.Point, info DeviceInfo) (base string, dev device, seed string) {
	// Outdoor-shared settings (scope: outdoor, e.g. outdoor silent) are a single
	// knob on the outdoor unit exposed per indoor unit. Key them by the outdoor
	// serial and attach them to the outdoor device so all the indoor units'
	// points collapse to one entity (deduplicated by the shared uid).
	if p.Entry.Scope == "outdoor" && info.Outdoor != nil && info.Outdoor.SerialNumber != "" {
		base := "daikin_outdoor_" + info.Outdoor.SerialNumber
		dev, seed := d.sharedSubDevice(base, "", "Outdoor unit", "Außengerät", "", info.Outdoor)
		return base, dev, seed
	}
	switch p.MPType {
	case "gateway":
		if info.Gateway != nil && info.Gateway.SerialNumber != "" {
			// Per-unit gateway: name it after its unit and nest it under the
			// indoor unit so it appears as a sub-device rather than standalone.
			base := "daikin_gateway_" + info.Gateway.SerialNumber
			dev, seed := d.sharedSubDevice(base, mainIdentifier(p.DeviceID), "Gateway", "Gateway", info.Name, info.Gateway)
			return base, dev, seed
		}
		// No gateway serial (e.g. a Home Hub that is itself the gateway):
		// attach the entity to the main device so it appears as one device
		// rather than an empty main plus a gateway sub-device.
		dev := d.deviceBlock(p.DeviceID, info)
		return mainIdentifier(p.DeviceID), dev, dev.Name
	case "outdoorUnit":
		if info.Outdoor != nil && info.Outdoor.SerialNumber != "" {
			// Outdoor units are commonly shared across indoor units; keep a
			// generic name so it is not tied to one room.
			base := "daikin_outdoor_" + info.Outdoor.SerialNumber
			dev, seed := d.sharedSubDevice(base, "", "Outdoor unit", "Außengerät", "", info.Outdoor)
			return base, dev, seed
		}
		dev, seed := d.subDeviceBlock(p.DeviceID, "outdoor", "Outdoor unit", "Außengerät", info.Name, info.Outdoor)
		return mainIdentifier(p.DeviceID), dev, seed
	default:
		dev := d.deviceBlock(p.DeviceID, info)
		return mainIdentifier(p.DeviceID), dev, dev.Name
	}
}

// configPayload is the union of fields used across the platforms we emit.
// omitempty keeps each entity's config minimal.
type configPayload struct {
	Name string `json:"name"`
	// DefaultEntityID seeds the Home Assistant entity_id. Without a seed HA
	// derives the entity_id from the device + entity name, which is localized —
	// yielding e.g. a German id. We build the seed from the device name plus the
	// English, language-independent topic (see entityObjectID) so entity_ids stay
	// English (e.g. sensor.galerie_room_temperature) while Name is localized for
	// display. UniqueID is independent of the seed, so the entity identity never
	// changes with the name/language.
	//
	// The older object_id field is deliberately NOT published: it was replaced by
	// default_entity_id and no MQTT platform schema accepts it any more, so HA
	// silently drops it (the discovery schemas are extra=REMOVE_EXTRA).
	DefaultEntityID string `json:"default_entity_id"`
	UniqueID        string `json:"unique_id"`
	EntityCategory  string `json:"entity_category,omitempty"`
	Icon            string `json:"icon,omitempty"`
	// StateTopic is omitted for stateless platforms: Home Assistant's MQTT
	// button has no state and rejects a config carrying an unknown key.
	StateTopic        string   `json:"state_topic,omitempty"`
	CommandTopic      string   `json:"command_topic,omitempty"`
	PayloadPress      string   `json:"payload_press,omitempty"`
	UnitOfMeasurement string   `json:"unit_of_measurement,omitempty"`
	DeviceClass       string   `json:"device_class,omitempty"`
	StateClass        string   `json:"state_class,omitempty"`
	PayloadOn         string   `json:"payload_on,omitempty"`
	PayloadOff        string   `json:"payload_off,omitempty"`
	StateOn           string   `json:"state_on,omitempty"`
	StateOff          string   `json:"state_off,omitempty"`
	Options           []string `json:"options,omitempty"`
	Min               *float64 `json:"min,omitempty"`
	Max               *float64 `json:"max,omitempty"`
	Step              *float64 `json:"step,omitempty"`

	AvailabilityTopic   string `json:"availability_topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`

	JSONAttributesTopic string `json:"json_attributes_topic,omitempty"`

	Device device `json:"device"`
}

// BridgeStatusTopic returns the LWT/availability topic.
func (d *Discovery) BridgeStatusTopic() string { return d.state.BridgeStatus() }

// slot is the point's topic family. Every state / command / attributes topic
// this package advertises goes through it, so the retained config can only
// name topics composed exactly the way the coordinator composes them (F3).
func (d *Discovery) slot(p process.Point) layout.Slot {
	return d.state.Slot(p.DeviceID, p.EmbeddedID, p.Topic)
}

// AttributesTopic returns the per-entity JSON-attributes topic (a sibling of the
// state topic), used to expose the entity's data source (cloud vs local Faikin).
func (d *Discovery) AttributesTopic(p process.Point) string {
	return d.slot(p).Attributes()
}

// StateTopic returns the state topic for a point.
func (d *Discovery) StateTopic(p process.Point) string {
	return d.slot(p).State()
}

// CommandTopic returns the /set topic for a point.
func (d *Discovery) CommandTopic(p process.Point) string {
	return d.slot(p).Command()
}

// Publish emits a retained per-entity discovery config for every point.
//
// NOT ON THE PUBLISH PATH since ADR 0070 phase 8 step 6: the daemon publishes
// device documents ([Discovery.RenderBundles]) and this builder is kept because
// it is what the twelve pinned scenario goldens are built from, and those
// goldens are the migration's retraction contract — the set of topics every
// device document must supersede before it lands
// (TestTheRetractionCoversTheWholePinnedFleet). It is removed when that pin is,
// and not before.
//
// Points that map to an unsupported platform are skipped. infos maps a device ID to its
// rich device metadata (may be absent; a fallback name is used). It returns the
// set of config topics it published, so the caller can clear orphaned ones.
func (d *Discovery) Publish(ctx context.Context, points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) (published map[string]bool, err error) {
	published = map[string]bool{}
	var firstErr error
	pub := func(topic string, payload []byte) {
		published[topic] = true
		if e := d.pub.Publish(ctx, topic, payload, mqtt.QoS0, true); e != nil && firstErr == nil {
			firstErr = e
		}
	}

	// Combined climate entities replace the individual power/mode/setpoint
	// controls for climateControl management points that have both.
	climates, consumed := d.climateEntities(points, infos, climateInfos)
	for _, m := range climates {
		pub(m.topic, m.payload)
	}

	// Shared sub-device entities (gateway / outdoor unit) repeat across the API
	// devices of a multi-split system and collapse to one entity. Which member
	// survives decides which device's state topic the retained config names, so
	// it is chosen deterministically rather than by arrival order — see
	// [survivingPoints].
	survives := survivingPoints(d, points, infos, consumed)
	for i := range points {
		p := points[i]
		if !survives[i] {
			continue
		}
		uid, dev, seed := d.entityIdentity(p, infos[p.DeviceID])
		topic, payload, ok := d.buildConfig(p, uid, dev, seed)
		if !ok {
			continue
		}
		pub(topic, payload)
	}
	return published, firstErr
}

// survivingPoints decides, for each deduplicated unique_id, WHICH of the points
// sharing it the published config is built from. It returns a set of indices
// into points.
//
// This matters because the surviving point's device and embedded id are what
// the retained config's state_topic and json_attributes_topic name. A
// `scope: outdoor` catalogue entry resolves to one point per INDOOR unit, all
// sharing a unique_id derived from the outdoor serial; the same is true of a
// gateway shared across a multi-split.
//
// It used to be "whichever came first", which is the order process.ResolveAt
// walked the devices, which is the order the ONECTA `GET /devices` array
// arrived in. Nothing in the API documentation promises that order is stable.
// If it flips, discoverySignature changes, discovery is republished, and the
// retained config now names a DIFFERENT device's topic — silently. The entity
// keeps its unique_id (the serial), so it is not re-registered; it just starts
// reading somewhere else, with a retained value that may be a poll old. On a
// two-indoor multi-split that is up to thirteen entities per outdoor unit.
//
// The tie is now broken on (DeviceID, EmbeddedID), lexicographically lowest,
// which is a property of the installation rather than of a response. This is
// F7 of the ADR 0070 phase 8 measurement, and it does not depend on whether
// the array order actually varies — it removes the dependency instead of
// hoping.
//
// The non-surviving members' state is still published (every member carries
// the value), deliberately: see publishOutdoorShared and
// publishOutdoorScheduleState in internal/coordinator. That redundancy is what
// made the arrival-order version work at all, and it stays, because a
// deterministic choice is not the same as a choice this daemon can make before
// it has seen every device.
func survivingPoints(d *Discovery, points []process.Point, infos map[string]DeviceInfo, consumed map[string]bool) map[int]bool {
	best := map[string]int{}
	for i := range points {
		p := points[i]
		if consumed[p.DeviceID+"|"+p.EmbeddedID+"|"+p.Topic] {
			continue
		}
		uid, _, _ := d.entityIdentity(p, infos[p.DeviceID])
		j, seen := best[uid]
		if !seen || lessPoint(p, points[j]) {
			best[uid] = i
		}
	}
	out := make(map[int]bool, len(best))
	for _, i := range best {
		out[i] = true
	}
	return out
}

// lessPoint orders two points sharing a unique_id. Only the device and its
// management point can differ — the topic is what made them share the id.
func lessPoint(a, b process.Point) bool {
	if a.DeviceID != b.DeviceID {
		return a.DeviceID < b.DeviceID
	}
	return a.EmbeddedID < b.EmbeddedID
}

// UniqueIDPrefix is the namespace every unique_id this daemon publishes starts
// with. It is a compile-time literal rather than a function of MQTT_TOPIC, so
// changing the MQTT root orphans no entity — see [mainIdentifier].
const UniqueIDPrefix = "daikin_"

// ClaimDevices states which device segments of the state plane this instance
// actually writes: the ONECTA device ids of the last resolved poll, plus
// [layout.SchedulerDeviceID] when the weekly scheduler is running.
//
// It is what makes [Discovery.IsOwnConfig] an answer about THIS instance rather
// than about the `daikin_` namespace, and it is deliberately push-based: the
// set is a fact about the last poll, and a Discovery that has not been told
// claims nothing.
func (d *Discovery) ClaimDevices(ids []string) {
	owned := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			owned[id] = true
		}
	}
	d.mu.Lock()
	d.owned = owned
	d.mu.Unlock()
}

// ClaimedDevices returns the device segments currently claimed, sorted — for a
// log line and for the tests that drive the sweep.
func (d *Discovery) ClaimedDevices() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.owned))
	for id := range d.owned {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsOwnConfig reports whether a retained HA discovery config payload was
// published by THIS instance: its unique_id is in the `daikin_` namespace AND
// every topic it names lives under this instance's own MQTT root and under a
// device segment this instance actually polls ([Discovery.ClaimDevices]).
//
// The second half is unconditional, and that is the whole point (F14). Two
// go-daikin2mqtt instances on one broker derive every identity string from the
// ONECTA device id or a component serial: the legacy config topic is keyed on
// unique_id and the bundle node id is sanitize(dev.UID()), so BOTH are
// byte-identical between two instances seeing one device and no predicate over
// the TOPIC can separate them. The payload can — an instance that does not poll
// a device never writes a topic under that device's segment.
//
// The failure this closes is go-mtec2mqtt's, proved by its reviewer and fixed
// in its PR #54: a staggered upgrade where one instance publishes a device
// bundle and retracts "its" per-entity configs deletes the sibling's entire
// fleet, permanently, because the sibling has no reason to republish. The
// earlier version of that fix made the identity check opt-in; under the default
// the hazard was untouched. This one has no flag.
//
// It returns false before the first poll has resolved anything, for the same
// reason: ownership that cannot be proven is not claimed, and a retraction
// taken on state the daemon has not learned yet is a deletion it cannot undo.
//
// availability_topic is excluded from the check because it is bridge-level by
// construction (`<root>/bridge/status`, the same string on every config); the
// root half is still checked, by every other topic key.
func (d *Discovery) IsOwnConfig(payload []byte) bool {
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil || body == nil {
		return false
	}
	uid, _ := body["unique_id"].(string)
	if !strings.HasPrefix(uid, UniqueIDPrefix) {
		return false
	}
	owned := d.ownedDevices()
	if len(owned) == 0 {
		return false
	}
	ok, named := d.ownsNamedTopics(owned, body)
	// A payload naming no topic of ours at all is not evidence of ownership.
	return ok && named > 0
}

// ownedDevices returns the claimed device-segment set. Read once and used
// throughout a predicate, so a concurrent [Discovery.ClaimDevices] cannot make
// one payload be judged against two different claim sets.
func (d *Discovery) ownedDevices() map[string]bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.owned
}

// ownsNamedTopics applies the topic half of [Discovery.IsOwnConfig] to one
// decoded discovery body — a per-entity config, or one component of a device
// document. It reports whether every topic key it names is ours, and how many
// it named.
//
// availability_topic is excluded because it is bridge-level by construction
// ("<root>/bridge/status", the same string on every config and every
// component): it is not instance-specific, so keying on it would make the rule
// collapse to a compile-time literal for exactly the payloads that carry
// nothing else — which is F18, the defect that swept a sibling's 14 composite
// climate entities and 10 refresh buttons. The root half of it is still
// checked, by every other topic key. `named` is returned rather than folded
// into the boolean precisely so a caller must refuse a payload that proved
// nothing.
func (d *Discovery) ownsNamedTopics(owned map[string]bool, body map[string]any) (ok bool, named int) {
	for key, v := range body {
		if !strings.HasSuffix(key, "_topic") || key == "availability_topic" {
			continue
		}
		t, isStr := v.(string)
		if !isStr || t == "" {
			continue
		}
		if !d.ownsTopic(owned, t) {
			return false, named
		}
		named++
	}
	return true, named
}

// ownsTopic reports whether t is "<our root>/<a claimed device segment>/…".
func (d *Discovery) ownsTopic(owned map[string]bool, t string) bool {
	rest, ok := strings.CutPrefix(t, d.state.String()+"/")
	if !ok {
		return false
	}
	seg, _, ok := strings.Cut(rest, "/")
	return ok && owned[seg]
}

// ConfigFilter is the MQTT filter matching this daemon's discovery config topics
// (e.g. "homeassistant/+/+/config"), for collecting retained configs to reconcile.
func (d *Discovery) ConfigFilter() string { return d.baseTopic + "/+/+/config" }

// ConfigTopic is the retained discovery topic of one entity:
// "<prefix>/<platform>/<unique_id>/config". Four segments, no node id.
//
// One function, three callers — the catalogue entity builder, the composite
// climate builder and the schedule switch builder — because the form is a
// contract with the installed base rather than a local formatting choice, and
// because [LegacyConfigTopicForm] has to name something that is true of all
// three. Verified over all 264 config topics of the twelve pinned scenarios by
// TestConfigTopicForm.
func (d *Discovery) ConfigTopic(platform, uid string) string {
	return d.baseTopic + "/" + platform + "/" + uid + "/config"
}

// LegacyConfigTopicForm names the go-hamqtt publisher.LegacyTopicFunc that
// reproduces this bridge's retained per-entity config topics, and is therefore
// the one ADR 0070 phase 8 step 6 must set in
// publisher.Config.LegacyEntityTopics.
//
// It is publisher.LegacyTopicByUniqueID — the FOUR-segment form
// "<prefix>/<platform>/<unique_id>/config" — and not the five-segment
// publisher.LegacyTopicWithNodeID that a consumer gets by saying nothing.
// Measured, not assumed: all 264 config topics across the twelve pinned
// scenarios have exactly four levels, with the third byte-equal to the
// payload's own unique_id and no node-id level anywhere
// (TestConfigTopicForm).
//
// Stating it is the whole fix, because Config.LegacyEntityTopics REPLACES the
// default rather than extending it.
//
// Getting it wrong is silent and total: the five-segment default would retract
// none of the retained per-entity configs, the device bundle would be published
// while all of them were still retained, and Home Assistant refuses that with a
// single "WARNING [mqtt.entity] Received a conflicting MQTT discovery message".
// No entities appear, and nothing on the wire says why.
//
// It is a documented constant rather than a wired-up setting because nothing
// publishes a bundle yet; step 6 is what consumes it. The value is the
// function's name as a string precisely so that recording it costs this module
// no dependency on go-hamqtt one step early.
const LegacyConfigTopicForm = "publisher.LegacyTopicByUniqueID"

// buildConfig renders the discovery topic and JSON payload for a point, using
// the precomputed unique id, device block and entity-id seed (see
// [Discovery.entityIdentity]).
func (d *Discovery) buildConfig(p process.Point, uid string, dev device, seed string) (topic string, payload []byte, ok bool) {
	cfg := configPayload{
		Name:                p.Entry.LocalizedName(d.lang),
		DefaultEntityID:     p.Entry.Platform + "." + entityObjectID(seed, p.Topic),
		UniqueID:            uid,
		EntityCategory:      p.Entry.Category,
		Icon:                p.Entry.Icon,
		StateTopic:          d.StateTopic(p),
		AvailabilityTopic:   d.BridgeStatusTopic(),
		PayloadAvailable:    "online",
		PayloadNotAvailable: "offline",
		JSONAttributesTopic: d.AttributesTopic(p),
		Device:              dev,
	}

	switch p.Entry.Platform {
	case "sensor":
		cfg.UnitOfMeasurement = p.Unit
		cfg.DeviceClass = p.Entry.DeviceClass
		cfg.StateClass = p.Entry.StateClass
	case "binary_sensor":
		cfg.DeviceClass = p.Entry.DeviceClass
		cfg.PayloadOn = "true"
		cfg.PayloadOff = "false"
	case "switch":
		cfg.CommandTopic = d.CommandTopic(p)
		cfg.PayloadOn, cfg.PayloadOff = "on", "off"
		cfg.StateOn, cfg.StateOff = "on", "off"
	case "select":
		cfg.CommandTopic = d.CommandTopic(p)
		// Options are localized labels; state is published as the localized
		// label too, and the write path maps the chosen label back to the raw
		// API code via the catalog (CodeForLabel).
		for _, v := range p.Entry.Values {
			cfg.Options = append(cfg.Options, p.Entry.LocalizedLabel(v.Value, d.lang))
		}
	case "number":
		cfg.CommandTopic = d.CommandTopic(p)
		cfg.UnitOfMeasurement = p.Unit
		cfg.DeviceClass = p.Entry.DeviceClass
		cfg.Min, cfg.Max, cfg.Step = p.Min, p.Max, p.Step
	case "button":
		// A button is command-only: HA's MQTT button schema has no state_topic
		// and rejects the config if one is present.
		cfg.StateTopic = ""
		cfg.CommandTopic = d.CommandTopic(p)
		cfg.PayloadPress = "PRESS"
	default:
		return "", nil, false
	}

	topic = d.ConfigTopic(p.Entry.Platform, uid)
	payload, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, false
	}
	return topic, payload, true
}

// umlautReplacer transliterates German umlauts so a device name like
// "Außengerät" slugs to "aussengerat" (matching Home Assistant's slugify)
// rather than dropping the non-ASCII runes.
var umlautReplacer = strings.NewReplacer(
	"ä", "a", "ö", "o", "ü", "u", "ß", "ss",
)

// slugify lowercases, transliterates umlauts and reduces any run of
// non-alphanumeric characters to a single underscore (trimmed at the ends),
// mirroring how HA derives an object_id from a name.
func slugify(s string) string {
	s = umlautReplacer.Replace(strings.ToLower(s))
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

// collapseTokens drops adjacent duplicate underscore-separated tokens, so a
// "Gateway Galerie" device combined with a "gateway_..." topic does not repeat
// "gateway" (e.g. gateway_galerie_gateway_... is left as-is, but
// galerie_gateway + gateway_... collapses to galerie_gateway_...).
func collapseTokens(s string) string {
	parts := strings.Split(s, "_")
	out := parts[:0]
	for _, p := range parts {
		if p == "" || (len(out) > 0 && out[len(out)-1] == p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "_")
}

// entityObjectID builds a clean, English, language-independent object id from
// a device-name SEED (a stable room/label prefix) and the English topic (the
// measurement), e.g. "galerie_room_temperature". It seeds default_entity_id so
// HA entity_ids stay English while the display name is localized.
//
// The seed is not always the device block's Name. For a main device it is —
// that name is the operator's own text and does not move with LANGUAGE. But a
// shared gateway or outdoor sub-device has no operator text to use, so this
// bridge composes its display name from a TRANSLATED label ("Outdoor unit" /
// "Außengerät"), and seeding an entity id from that made the entity id move
// with LANGUAGE: an operator switching to German got a SECOND set of entities
// for everything on the outdoor unit, with the first set left behind as
// orphans, because Home Assistant never renames a registered entity. On a real
// multi-split that is up to thirteen entities per outdoor unit (twelve
// scope: outdoor catalogue entries plus the refresh button).
//
// [Discovery.entityIdentity] therefore returns the English-label form of the
// name as a separate seed. This is F2 of the ADR 0070 phase 8 measurement, and
// it is a direct violation of the invariant this repository's own CLAUDE.md
// states in bold: "unique_id and default_entity_id are English and
// language-independent."
func entityObjectID(seed, topic string) string {
	return collapseTokens(slugify(seed + "_" + topic))
}

// sanitize keeps only characters valid in HA object/unique ids.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ConfigUniqueID returns the top-level unique_id of a retained discovery config
// payload, or "" when there is none (a device bundle carries none either).
//
// Exported for the sweep's report, which distinguishes a config another
// go-daikin2mqtt instance published — same namespace, different device — from
// one another integration published onto a topic that merely looks like ours.
// Neither is ever retracted; telling them apart is what makes the report
// reviewable.
func ConfigUniqueID(payload []byte) string {
	var body struct {
		UniqueID string `json:"unique_id"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return ""
	}
	return body.UniqueID
}
