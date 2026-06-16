// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package process resolves parsed ONECTA devices against the curated
// catalog into a flat list of publishable [Point]s. It handles the nested
// value shapes the cloud uses (sensoryData sub-values, mode-scoped
// temperatureControl setpoints) so the coordinator and HA discovery layers
// can work with simple scalar points.
package process

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// Point is a single resolved, publishable data point.
type Point struct {
	DeviceID   string
	EmbeddedID string
	MPType     string
	Topic      string
	Entry      catalog.Entry
	// Value is the decoded scalar: string, float64, bool, or the raw JSON
	// string when it could not be decoded as a scalar.
	Value any
	Unit  string // resolved unit (nested wrapper unit, else entry.Unit)
	// Min/Max/Step come from the live API wrapper when present (used by HA
	// number entities); nil when the API did not provide them.
	Min, Max, Step *float64
	Settable       bool
}

// wrapper is the ONECTA value-wrapper used for both top-level and nested
// characteristic values.
type wrapper struct {
	Value     json.RawMessage `json:"value"`
	Unit      string          `json:"unit"`
	MinValue  *float64        `json:"minValue"`
	MaxValue  *float64        `json:"maxValue"`
	StepValue *float64        `json:"stepValue"`
	Settable  bool            `json:"settable"`
}

// Resolve flattens every device's catalog-matched characteristics into
// points, using the current time for time-dependent values (energy).
func Resolve(devices []model.Device, cat *catalog.Catalog) []Point {
	return ResolveAt(devices, cat, time.Now())
}

// ResolveAt is like [Resolve] but uses now for time-dependent resolution
// (the monthly energy bucket index). Characteristics without a catalog
// entry, disabled entries, and values that cannot be resolved (e.g. a
// {mode} path with no current mode) are skipped.
func ResolveAt(devices []model.Device, cat *catalog.Catalog, now time.Time) []Point {
	var points []Point
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			mode := currentMode(mp)
			entries := cat.EntriesForType(mp.Type)
			for i := range entries {
				e := entries[i]
				if !e.IsEnabled() {
					continue
				}
				char, ok := mp.Characteristics[e.Match.Characteristic]
				if !ok {
					continue
				}
				p, ok := resolvePoint(d.ID, mp.EmbeddedID, mp.Type, e, char, mode, now)
				if !ok {
					continue
				}
				points = append(points, p)
			}
		}
	}
	return points
}

// currentMode returns the active operationMode value of a management point,
// used to substitute the "{mode}" token. Empty when not present.
func currentMode(mp model.ManagementPoint) string {
	if c, ok := mp.Characteristics["operationMode"]; ok {
		if s, ok := c.String(); ok {
			return s
		}
	}
	return ""
}

// resolvePoint builds a Point for one catalog entry + characteristic.
func resolvePoint(deviceID, embeddedID, mpType string, e catalog.Entry, char model.Characteristic, mode string, now time.Time) (Point, bool) {
	p := Point{
		DeviceID:   deviceID,
		EmbeddedID: embeddedID,
		MPType:     mpType,
		Topic:      e.Topic,
		Entry:      e,
		Unit:       e.Unit,
		Settable:   e.Settable,
	}

	if e.Kind == "energy" {
		return resolveEnergy(p, e, char, now)
	}

	if e.ValuePath == "" {
		// The characteristic value is the scalar itself.
		p.Value = decodeScalar(char.Value)
		p.Min, p.Max, p.Step = char.MinValue, char.MaxValue, char.StepValue
		return p, true
	}

	// Navigate into the nested value object along the (mode-substituted) path.
	path := strings.ReplaceAll(e.ValuePath, "{mode}", mode)
	if strings.Contains(path, "{mode}") || (mode == "" && strings.Contains(e.ValuePath, "{mode}")) {
		return Point{}, false // unresolved mode
	}
	w, ok := navigate(char.Value, splitPath(path))
	if !ok {
		return Point{}, false
	}
	p.Value = decodeScalar(w.Value)
	if w.Unit != "" {
		p.Unit = w.Unit
	}
	p.Min, p.Max, p.Step = w.MinValue, w.MaxValue, w.StepValue
	return p, true
}

// resolveEnergy reads a consumptionData array and sums the relevant period
// slice, mirroring the Daikin ONECTA semantics:
//   - daily ("d", 24 two-hour buckets over two days): sum of today's half [12:]
//   - weekly ("w", 14 days over two weeks): sum of this week [7:]
//   - monthly ("m", 24 months over two years): the current month (11+month)
//
// The unit comes from the consumption block ("kWh"), defaulting to kWh.
func resolveEnergy(p Point, e catalog.Entry, char model.Characteristic, now time.Time) (Point, bool) {
	consumption := e.Consumption
	if consumption == "" {
		consumption = "electrical"
	}
	periodKey, ok := map[string]string{"daily": "d", "weekly": "w", "monthly": "m"}[e.EnergyPeriod]
	if !ok {
		return Point{}, false
	}

	// char.Value is the consumptionData value object: {electrical:{cooling:{d,w,m},heating:{…},unit},gas:{…}}.
	consObj, ok := objectField(char.Value, consumption)
	if !ok {
		return Point{}, false
	}
	modeObj, ok := objectField(consObj, e.EnergyMode)
	if !ok {
		return Point{}, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(modeObj, &top) != nil {
		return Point{}, false
	}
	arrRaw, ok := top[periodKey]
	if !ok {
		return Point{}, false
	}
	var arr []*float64
	if json.Unmarshal(arrRaw, &arr) != nil {
		return Point{}, false
	}

	sum, ok := sumEnergySlice(arr, e.EnergyPeriod, now)
	if !ok {
		return Point{}, false
	}
	p.Value = math.Round(sum*1000) / 1000
	if u := unitOf(consObj); u != "" {
		p.Unit = u
	} else if p.Unit == "" {
		p.Unit = "kWh"
	}
	return p, true
}

// sumEnergySlice sums the period-specific slice of a consumption array.
func sumEnergySlice(arr []*float64, period string, now time.Time) (float64, bool) {
	get := func(i int) float64 {
		if i < 0 || i >= len(arr) || arr[i] == nil {
			return 0
		}
		return *arr[i]
	}
	var start, end int
	switch period {
	case "weekly":
		start, end = 7, len(arr)
	case "monthly":
		start = 11 + int(now.Month())
		end = start + 1
	default: // daily
		start, end = 12, len(arr)
	}
	if start < 0 || start > len(arr) {
		return 0, false
	}
	var sum float64
	for i := start; i < end; i++ {
		sum += get(i)
	}
	return sum, true
}

// objectField returns the raw JSON of key within a JSON object, or false.
func objectField(raw json.RawMessage, key string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	v, ok := obj[key]
	return v, ok
}

// unitOf extracts a "unit" string field from a JSON object, or "".
func unitOf(raw json.RawMessage) string {
	u, ok := objectField(raw, "unit")
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(u, &s)
	return s
}

// navigate descends raw (a JSON object) along segs and parses the final
// node as a value wrapper.
func navigate(raw json.RawMessage, segs []string) (wrapper, bool) {
	cur := raw
	for _, seg := range segs {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return wrapper{}, false
		}
		next, ok := obj[seg]
		if !ok {
			return wrapper{}, false
		}
		cur = next
	}
	var w wrapper
	if err := json.Unmarshal(cur, &w); err != nil {
		return wrapper{}, false
	}
	return w, true
}

// splitPath splits a slash path, dropping empty segments (so a leading
// slash is tolerated).
func splitPath(p string) []string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// decodeScalar turns a raw JSON value into a Go scalar (string/float64/bool)
// or the trimmed raw text when it is not a scalar.
func decodeScalar(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	return string(raw)
}

// Format renders the point's value as an MQTT payload string. Floats honour
// the entry's precision; bools become "true"/"false"; strings pass through.
func (p Point) Format() string {
	switch v := p.Value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', p.Entry.Precision, 64)
	default:
		return ""
	}
}
