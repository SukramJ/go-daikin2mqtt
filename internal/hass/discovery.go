// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package hass builds and publishes Home Assistant MQTT discovery configs
// for the resolved Daikin data points, so entities appear automatically.
package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SukramJ/go-daikin2mqtt/internal/mqtt"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
)

// Discovery publishes retained HA MQTT discovery configs.
type Discovery struct {
	baseTopic string // e.g. "homeassistant"
	stateRoot string // e.g. "daikin"
	lang      string
	pub       mqtt.Publisher
}

// New returns a Discovery publisher. baseTopic is the HA discovery prefix,
// stateRoot the MQTT topic root the daemon publishes state under.
func New(baseTopic, stateRoot, lang string, pub mqtt.Publisher) *Discovery {
	return &Discovery{baseTopic: baseTopic, stateRoot: stateRoot, lang: lang, pub: pub}
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
	Gateway       *SubDevice
	Outdoor       *SubDevice
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

// mainIdentifier returns the HA identifier of a device's main entry.
func mainIdentifier(deviceID string) string { return "daikin_" + deviceID }

// deviceBlock builds the HA device-registry block for the main (indoor /
// climate) Daikin device.
func (d *Discovery) deviceBlock(deviceID string, info DeviceInfo) device {
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
		ConfigurationURL: configurationURL,
	}
}

// subDeviceBlock builds a nested HA device (gateway / outdoor unit) linked to
// the main device via via_device. suffix disambiguates the identifier;
// labelEN/labelDE are appended to the base name.
func (d *Discovery) subDeviceBlock(deviceID, suffix, labelEN, labelDE, baseName string, sub *SubDevice) device {
	label := labelEN
	if d.lang == "de" && labelDE != "" {
		label = labelDE
	}
	dev := device{
		Identifiers:      []string{mainIdentifier(deviceID) + "_" + suffix},
		Name:             orDefault(baseName, "Daikin "+deviceID) + " " + label,
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
	return dev
}

// sharedSubDevice builds a standalone HA device for a component that is shared
// across several API devices (gateway / outdoor unit), keyed by its serial so
// it is deduplicated to a single HA device. No via_device is set because the
// component has no single parent.
func (d *Discovery) sharedSubDevice(identifier, labelEN, labelDE, baseName string, sub *SubDevice) device {
	label := labelEN
	if d.lang == "de" && labelDE != "" {
		label = labelDE
	}
	// Name the device after its associated unit when known (e.g. "Gateway
	// Wohnzimmer") so multiple gateways are distinguishable; fall back to a
	// generic name for truly shared components (e.g. one outdoor unit).
	name := "Daikin " + label
	if baseName != "" {
		name = label + " " + baseName
	}
	dev := device{
		Identifiers:      []string{identifier},
		Name:             name,
		Manufacturer:     "Daikin",
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
	return dev
}

// entityIdentity returns the HA unique_id and device block for a point.
//
// Gateway and outdoor-unit components are shared across the multiple API
// devices of a multi-split system (identical serial numbers), so when a
// serial is known they are keyed by serial — yielding a single deduplicated
// HA device and one set of entities. Without a serial they fall back to a
// per-device nested sub-device (via_device → main).
func (d *Discovery) entityIdentity(p process.Point, info DeviceInfo) (uid string, dev device) {
	switch p.MPType {
	case "gateway":
		if info.Gateway != nil && info.Gateway.SerialNumber != "" {
			// Gateways have per-unit serials, so name each after its unit.
			base := "daikin_gateway_" + info.Gateway.SerialNumber
			return sanitize(base + "_" + p.Topic), d.sharedSubDevice(base, "Gateway", "Gateway", info.Name, info.Gateway)
		}
		// No gateway serial (e.g. a Home Hub that is itself the gateway):
		// attach the entity to the main device so it appears as one device
		// rather than an empty main plus a gateway sub-device.
		return sanitize("daikin_" + p.DeviceID + "_" + p.Topic), d.deviceBlock(p.DeviceID, info)
	case "outdoorUnit":
		if info.Outdoor != nil && info.Outdoor.SerialNumber != "" {
			// Outdoor units are commonly shared across indoor units; keep a
			// generic name so it is not tied to one room.
			base := "daikin_outdoor_" + info.Outdoor.SerialNumber
			return sanitize(base + "_" + p.Topic), d.sharedSubDevice(base, "Outdoor unit", "Außengerät", "", info.Outdoor)
		}
		return sanitize("daikin_" + p.DeviceID + "_" + p.Topic),
			d.subDeviceBlock(p.DeviceID, "outdoor", "Outdoor unit", "Außengerät", info.Name, info.Outdoor)
	default:
		return sanitize("daikin_" + p.DeviceID + "_" + p.Topic), d.deviceBlock(p.DeviceID, info)
	}
}

// configPayload is the union of fields used across the platforms we emit.
// omitempty keeps each entity's config minimal.
type configPayload struct {
	Name string `json:"name"`
	// DefaultEntityID seeds the Home Assistant entity_id. Current HA replaced
	// the older object_id discovery field with default_entity_id (a full
	// "<domain>.<object_id>"); without it HA derives the entity_id from the
	// device + entity name, which is localized — yielding e.g. a German id. We
	// set it to the English, language-independent topic so entity_ids stay
	// English (e.g. sensor.<device>_room_temperature) while Name is localized.
	DefaultEntityID   string   `json:"default_entity_id"`
	UniqueID          string   `json:"unique_id"`
	EntityCategory    string   `json:"entity_category,omitempty"`
	StateTopic        string   `json:"state_topic"`
	CommandTopic      string   `json:"command_topic,omitempty"`
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

	Device device `json:"device"`
}

// BridgeStatusTopic returns the LWT/availability topic.
func (d *Discovery) BridgeStatusTopic() string { return d.stateRoot + "/bridge/status" }

// StateTopic returns the state topic for a point.
func (d *Discovery) StateTopic(p process.Point) string {
	return fmt.Sprintf("%s/%s/%s/%s/state", d.stateRoot, p.DeviceID, p.EmbeddedID, p.Topic)
}

// CommandTopic returns the /set topic for a point.
func (d *Discovery) CommandTopic(p process.Point) string {
	return fmt.Sprintf("%s/%s/%s/%s/set", d.stateRoot, p.DeviceID, p.EmbeddedID, p.Topic)
}

// Publish emits a retained discovery config for every point. Points that
// map to an unsupported platform are skipped. infos maps a device ID to its
// rich device metadata (may be absent; a fallback name is used).
func (d *Discovery) Publish(ctx context.Context, points []process.Point, infos map[string]DeviceInfo, climateInfos map[string]ClimateInfo) error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Combined climate entities replace the individual power/mode/setpoint
	// controls for climateControl management points that have both.
	climates, consumed := d.climateEntities(points, infos, climateInfos)
	for _, m := range climates {
		record(d.pub.Publish(ctx, m.topic, m.payload, mqtt.QoS0, true))
	}

	// seen deduplicates shared sub-device entities (gateway / outdoor unit)
	// that repeat across the API devices of a multi-split system.
	seen := map[string]bool{}
	for i := range points {
		p := points[i]
		if consumed[p.DeviceID+"|"+p.EmbeddedID+"|"+p.Topic] {
			continue
		}
		uid, dev := d.entityIdentity(p, infos[p.DeviceID])
		if seen[uid] {
			continue
		}
		seen[uid] = true
		topic, payload, ok := d.buildConfig(p, uid, dev)
		if !ok {
			continue
		}
		record(d.pub.Publish(ctx, topic, payload, mqtt.QoS0, true))
	}
	return firstErr
}

// buildConfig renders the discovery topic and JSON payload for a point, using
// the precomputed unique id and device block (see [Discovery.entityIdentity]).
func (d *Discovery) buildConfig(p process.Point, uid string, dev device) (topic string, payload []byte, ok bool) {
	cfg := configPayload{
		Name:                p.Entry.LocalizedName(d.lang),
		DefaultEntityID:     p.Entry.Platform + "." + uid,
		UniqueID:            uid,
		EntityCategory:      p.Entry.Category,
		StateTopic:          d.StateTopic(p),
		AvailabilityTopic:   d.BridgeStatusTopic(),
		PayloadAvailable:    "online",
		PayloadNotAvailable: "offline",
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
	default:
		return "", nil, false
	}

	topic = fmt.Sprintf("%s/%s/%s/config", d.baseTopic, p.Entry.Platform, uid)
	payload, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, false
	}
	return topic, payload, true
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
