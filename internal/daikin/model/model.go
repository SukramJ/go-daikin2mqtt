// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package model parses the JSON returned by the Daikin ONECTA
// "gateway-devices" endpoint into a typed representation.
//
// The endpoint returns an array of devices. Each device carries an
// identifier, a model string and a list of management points. A
// management point groups a set of named characteristics, each of which
// is a value wrapper that may hold a scalar (string, number, bool) or a
// nested JSON object/array.
//
// Parsing is intentionally tolerant: unknown fields are ignored and
// missing optional fields decode to their zero value (nil pointers or
// empty slices/maps).
package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Device is a single ONECTA gateway device.
type Device struct {
	ID               string
	Model            string // from deviceModel or type
	ManagementPoints []ManagementPoint
	Raw              json.RawMessage // original device JSON
}

// ManagementPoint groups the characteristics of one functional unit of a
// device (for example a climate control zone, a hot water tank or the
// gateway itself).
type ManagementPoint struct {
	EmbeddedID      string
	Type            string // managementPointType
	Category        string // managementPointCategory
	Characteristics map[string]Characteristic
}

// Characteristic is a single value wrapper attached to a management
// point. The wrapped value may be a scalar or a nested object/array; use
// the accessor methods to interpret it.
type Characteristic struct {
	Value     json.RawMessage // raw value (may be scalar or object)
	Settable  bool
	MinValue  *float64
	MaxValue  *float64
	StepValue *float64
	Values    []string        // enum options if present (string list)
	Raw       json.RawMessage // original characteristic JSON
}

// reserved holds the management-point keys that describe the management
// point itself and therefore must not be exposed as characteristics.
var reserved = map[string]struct{}{
	"embeddedId":              {},
	"managementPointType":     {},
	"managementPointCategory": {},
}

// rawDevice mirrors the on-the-wire device object. Only the fields we
// care about are listed; everything else is ignored by encoding/json.
type rawDevice struct {
	ID           string `json:"id"`
	UnderscoreID string `json:"_id"`
	DeviceModel  string `json:"deviceModel"`
	Type         string `json:"type"`
	// managementPoints is kept raw so each entry can be decoded with the
	// generic characteristic logic.
	ManagementPoints []json.RawMessage `json:"managementPoints"`
}

// rawCharacteristic mirrors the on-the-wire value-wrapper object.
type rawCharacteristic struct {
	Value     json.RawMessage `json:"value"`
	Settable  bool            `json:"settable"`
	MinValue  *float64        `json:"minValue"`
	MaxValue  *float64        `json:"maxValue"`
	StepValue *float64        `json:"stepValue"`
	Values    []string        `json:"values"`
}

// ParseDevices decodes the gateway-devices JSON array into a slice of
// [Device]. An error is returned only when the top-level JSON is not a
// well-formed array of objects; individual unknown or missing fields are
// tolerated.
func ParseDevices(raw json.RawMessage) ([]Device, error) {
	var rawDevices []json.RawMessage
	if err := json.Unmarshal(raw, &rawDevices); err != nil {
		return nil, fmt.Errorf("model: decode device array: %w", err)
	}

	devices := make([]Device, 0, len(rawDevices))
	for i, rd := range rawDevices {
		dev, err := parseDevice(rd)
		if err != nil {
			return nil, fmt.Errorf("model: device %d: %w", i, err)
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

func parseDevice(raw json.RawMessage) (Device, error) {
	var rd rawDevice
	if err := json.Unmarshal(raw, &rd); err != nil {
		return Device{}, fmt.Errorf("decode device: %w", err)
	}

	id := rd.ID
	if id == "" {
		id = rd.UnderscoreID
	}

	model := rd.DeviceModel
	if model == "" {
		model = rd.Type
	}

	mps := make([]ManagementPoint, 0, len(rd.ManagementPoints))
	for i, rmp := range rd.ManagementPoints {
		mp, err := parseManagementPoint(rmp)
		if err != nil {
			return Device{}, fmt.Errorf("management point %d: %w", i, err)
		}
		mps = append(mps, mp)
	}

	// Copy the raw bytes so callers cannot mutate our backing array.
	rawCopy := make(json.RawMessage, len(raw))
	copy(rawCopy, raw)

	return Device{
		ID:               id,
		Model:            model,
		ManagementPoints: mps,
		Raw:              rawCopy,
	}, nil
}

func parseManagementPoint(raw json.RawMessage) (ManagementPoint, error) {
	// Decode into a generic map so we can separate the descriptive keys
	// from the arbitrary set of characteristic keys.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ManagementPoint{}, fmt.Errorf("decode management point: %w", err)
	}

	mp := ManagementPoint{
		EmbeddedID:      decodeString(fields["embeddedId"]),
		Type:            decodeString(fields["managementPointType"]),
		Category:        decodeString(fields["managementPointCategory"]),
		Characteristics: make(map[string]Characteristic),
	}

	for name, fieldRaw := range fields {
		if _, ok := reserved[name]; ok {
			continue
		}
		c, ok := parseCharacteristic(fieldRaw)
		if !ok {
			// Not a value wrapper (for example managementPointSubType,
			// which is a bare string). Skip it rather than fail.
			continue
		}
		mp.Characteristics[name] = c
	}

	return mp, nil
}

// parseCharacteristic decodes a value-wrapper object. The second return
// value is false when the field is not an object containing a "value"
// key, in which case it is not a characteristic.
func parseCharacteristic(raw json.RawMessage) (Characteristic, bool) {
	var rc rawCharacteristic
	if err := json.Unmarshal(raw, &rc); err != nil {
		return Characteristic{}, false
	}
	// A value wrapper always carries a "value" field.
	if len(rc.Value) == 0 {
		return Characteristic{}, false
	}

	rawCopy := make(json.RawMessage, len(raw))
	copy(rawCopy, raw)

	return Characteristic{
		Value:     rc.Value,
		Settable:  rc.Settable,
		MinValue:  rc.MinValue,
		MaxValue:  rc.MaxValue,
		StepValue: rc.StepValue,
		Values:    rc.Values,
		Raw:       rawCopy,
	}, true
}

// decodeString best-effort decodes a JSON string, returning "" when the
// input is absent or not a string.
func decodeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// String returns the value as a string when it is a scalar JSON string.
// The boolean reports whether the conversion succeeded.
func (c Characteristic) String() (string, bool) {
	if len(c.Value) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(c.Value, &s); err != nil {
		return "", false
	}
	return s, true
}

// Float returns the value as a float64 when it is a scalar JSON number.
// The boolean reports whether the conversion succeeded.
func (c Characteristic) Float() (float64, bool) {
	if len(c.Value) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(c.Value, &f); err != nil {
		return 0, false
	}
	return f, true
}

// Bool returns the value as a bool when it is a scalar JSON boolean. The
// boolean reports whether the conversion succeeded.
func (c Characteristic) Bool() (value, ok bool) {
	if len(c.Value) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(c.Value, &b); err != nil {
		return false, false
	}
	return b, true
}

// IsObject reports whether the value is a JSON object or array rather
// than a scalar. Even when true, [Characteristic.Value] and
// [Characteristic.Raw] remain populated.
func (c Characteristic) IsObject() bool {
	for _, b := range c.Value {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// DataPoint is a flattened view of a single characteristic together with
// the device and management point it belongs to. It is convenient for
// catalog matching and linear iteration.
type DataPoint struct {
	DeviceID       string
	EmbeddedID     string
	MPType         string // managementPointType
	Name           string // characteristic name
	Characteristic Characteristic
}

// DataPoints flattens every characteristic of every management point of
// the device into a slice of [DataPoint].
func (d Device) DataPoints() []DataPoint {
	var dps []DataPoint
	for _, mp := range d.ManagementPoints {
		for name, c := range mp.Characteristics {
			dps = append(dps, DataPoint{
				DeviceID:       d.ID,
				EmbeddedID:     mp.EmbeddedID,
				MPType:         mp.Type,
				Name:           name,
				Characteristic: c,
			})
		}
	}
	return dps
}

// ErrNotFound is returned by lookup helpers when no matching element
// exists.
var ErrNotFound = errors.New("model: not found")
