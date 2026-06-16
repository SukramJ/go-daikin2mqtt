// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package catalog implements the curated ONECTA characteristic catalog and
// its internationalisation (i18n) helpers.
//
// The catalog is the single source of truth for which Daikin ONECTA
// management-point characteristics are published and how they are exposed
// (MQTT topic, Home Assistant platform, device class, units, enum options,
// …). Coverage is deterministic and curated: only characteristics with an
// [Entry] are mapped.
//
// This package is intentionally decoupled from the wire model
// (internal/daikin/model): matching happens purely over strings
// (management-point type + characteristic name) so the catalog can evolve
// independently of the JSON parsing layer.
//
// Localisation follows the project i18n pattern (see docs/konzept.md §8):
// English is the canonical fallback and German overrides are optional and
// applied per item. A missing German value never hides the English one.
package catalog

// Match identifies which Daikin management-point characteristic an [Entry]
// applies to. Both fields are required and the pair must be unique across a
// loaded [Catalog].
type Match struct {
	ManagementPointType string `yaml:"managementPointType"`
	Characteristic      string `yaml:"characteristic"`
}

// ValueLabel pairs a raw API enum value with its friendly labels. The raw
// value is language-independent and used on the wire / in PATCH requests;
// the labels are for display only.
type ValueLabel struct {
	Value   string `yaml:"value"`    // raw API value, e.g. "heating"
	Label   string `yaml:"label"`    // English friendly label
	LabelDE string `yaml:"label_de"` // optional German label
}

// Entry describes how a single characteristic is mapped to MQTT and Home
// Assistant. Topic, Name and the raw enum Values are language-independent;
// NameDE and the per-value German labels are optional overrides.
type Entry struct {
	Match       Match  `yaml:"match"`
	Topic       string `yaml:"topic"`        // MQTT suffix, language-independent, unique
	Name        string `yaml:"name"`         // English friendly name (canonical)
	NameDE      string `yaml:"name_de"`      // optional German name
	Platform    string `yaml:"platform"`     // sensor|binary_sensor|switch|select|number|climate
	DeviceClass string `yaml:"device_class"` //
	Unit        string `yaml:"unit"`         //
	StateClass  string `yaml:"state_class"`  //
	Settable    bool   `yaml:"settable"`     //
	// Category is the Home Assistant entity_category: "diagnostic" or "config"
	// (empty = a normal/primary entity). Diagnostics group hardware info,
	// connectivity and error states away from primary controls.
	Category string       `yaml:"category"`
	Values   []ValueLabel `yaml:"values"` // enum options (for select/sensor enum)
	// ValuePath is an optional slash-separated path into the characteristic's
	// nested value object to read the actual scalar (and its unit/min/max/step
	// wrapper). Empty means the characteristic value itself is the scalar.
	// The token "{mode}" is replaced at resolve time by the current
	// operationMode (e.g. "cooling"), enabling mode-scoped setpoints.
	// Example: "operationModes/{mode}/setpoints/roomTemperature".
	ValuePath string `yaml:"value_path"`
	// Path is the optional nested PATCH "path" body field for settable nested
	// values. It also supports the "{mode}" token. Often equal to ValuePath.
	Path      string  `yaml:"path"`
	Scale     float64 `yaml:"scale"`     // optional, 0 means none
	Precision int     `yaml:"precision"` //
	Enabled   *bool   `yaml:"enabled"`   // default true when nil

	// Kind selects special resolution. "energy" reads consumptionData arrays
	// and sums the period slice (see EnergyMode/EnergyPeriod/Consumption).
	// Empty means a plain scalar (default).
	Kind string `yaml:"kind"`
	// Consumption is "electrical" (default) or "gas" for Kind=="energy".
	Consumption string `yaml:"consumption"`
	// EnergyMode is the consumption operation mode, e.g. "cooling"/"heating".
	EnergyMode string `yaml:"energy_mode"`
	// EnergyPeriod is "daily", "weekly", or "monthly".
	EnergyPeriod string `yaml:"energy_period"`
}

// IsEnabled reports whether the entry should be published. Entries are
// enabled by default; only an explicit enabled:false disables them.
func (e *Entry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// Catalog is an immutable, validated set of [Entry] values. Topics are
// unique across the catalog and indexed for O(1) write-path lookup. A given
// (managementPointType, characteristic) pair may appear in multiple entries
// when they read different nested sub-values (distinct ValuePath/Topic).
type Catalog struct {
	entries []Entry
	byTopic map[string]int // topic -> index into entries
	byMatch map[Match]int  // first entry per match pair -> index into entries
}

// Match returns the first entry for the given management-point type and
// characteristic, or (nil, false) when none is mapped. When several entries
// share the pair (nested sub-values), prefer iterating [Catalog.Entries]
// or [Catalog.EntriesForType] instead.
func (c *Catalog) Match(mpType, characteristic string) (*Entry, bool) {
	i, ok := c.byMatch[Match{ManagementPointType: mpType, Characteristic: characteristic}]
	if !ok {
		return nil, false
	}
	return &c.entries[i], true
}

// ByTopic returns the entry with the given MQTT topic suffix, or
// (nil, false). Used by the write path to resolve an inbound /set topic.
func (c *Catalog) ByTopic(topic string) (*Entry, bool) {
	i, ok := c.byTopic[topic]
	if !ok {
		return nil, false
	}
	return &c.entries[i], true
}

// EntriesForType returns the entries whose match targets the given
// management-point type, in declaration order.
func (c *Catalog) EntriesForType(mpType string) []Entry {
	var out []Entry
	for i := range c.entries {
		e := &c.entries[i]
		if e.Match.ManagementPointType == mpType {
			out = append(out, *e)
		}
	}
	return out
}

// Entries returns a copy of the catalog entries in declaration order. The
// returned slice is safe for the caller to retain and mutate; it does not
// alias the catalog's internal storage.
func (c *Catalog) Entries() []Entry {
	out := make([]Entry, len(c.entries))
	copy(out, c.entries)
	return out
}
