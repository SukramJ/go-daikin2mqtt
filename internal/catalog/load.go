// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package catalog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidationError is returned by [Load] / [LoadFile] when one or more
// catalog entries fail a shape check. The Issues slice contains
// human-readable problem descriptions so the caller can log them all in one
// shot (analogous to config.ValidationError).
type ValidationError struct {
	Issues []string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		return "catalog: " + e.Issues[0]
	}
	return fmt.Sprintf("catalog: %d validation issue(s):\n  - %s",
		len(e.Issues), strings.Join(e.Issues, "\n  - "))
}

// allowedPlatforms is the set of Home Assistant platforms an [Entry] may
// target.
var allowedPlatforms = map[string]bool{
	"sensor":        true,
	"binary_sensor": true,
	"switch":        true,
	"select":        true,
	"number":        true,
	"climate":       true,
	"button":        true,
}

// Load parses and validates a YAML catalog document from r. The document is
// a top-level list of entries (see characteristics.yaml). On any validation
// failure it returns a [*ValidationError] aggregating every problem and a
// nil catalog.
func Load(r io.Reader) (*Catalog, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("catalog: read: %w", err)
	}

	var entries []Entry
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(false) // unknown fields are ignored by design
	if err := dec.Decode(&entries); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}

	var issues []string
	add := func(format string, args ...any) {
		issues = append(issues, fmt.Sprintf(format, args...))
	}

	byTopic := make(map[string]int, len(entries))
	byMatch := make(map[Match]int, len(entries))
	for i := range entries {
		e := &entries[i]
		// A stable, human-friendly identifier for issue messages even when
		// required fields are missing.
		id := fmt.Sprintf("entry %d (%s/%s)", i,
			orPlaceholder(e.Match.ManagementPointType),
			orPlaceholder(e.Match.Characteristic))

		if e.Match.ManagementPointType == "" {
			add("%s: match.managementPointType is required", id)
		}
		if e.Match.Characteristic == "" {
			add("%s: match.characteristic is required", id)
		}
		if e.Topic == "" {
			add("%s: topic is required", id)
		}
		if e.Platform == "" {
			add("%s: platform is required", id)
		} else if !allowedPlatforms[e.Platform] {
			add("%s: platform %q is not one of [%s]", id, e.Platform, allowedPlatformList())
		}
		// A select with no options can never be operated; this is almost
		// always an authoring mistake. We surface it as an issue.
		if e.Platform == "select" && len(e.Values) == 0 {
			add("%s: platform=select requires at least one entry in values", id)
		}
		if e.Category != "" && e.Category != "diagnostic" && e.Category != "config" {
			add("%s: category %q must be empty, \"diagnostic\", or \"config\"", id, e.Category)
		}

		// Uniqueness is on the MQTT topic suffix (the wire identity). Several
		// entries may share a (mpType, characteristic) pair when they read
		// different nested sub-values, so the pair itself is not unique.
		if e.Topic != "" {
			if prev, dup := byTopic[e.Topic]; dup {
				add("%s: duplicate topic %q, already defined by entry %d", id, e.Topic, prev)
			} else {
				byTopic[e.Topic] = i
			}
		}
		// byMatch keeps the first entry per pair for the convenience Match lookup.
		if e.Match.ManagementPointType != "" && e.Match.Characteristic != "" {
			if _, ok := byMatch[e.Match]; !ok {
				byMatch[e.Match] = i
			}
		}
	}

	if len(issues) > 0 {
		sort.Strings(issues)
		return nil, &ValidationError{Issues: issues}
	}

	return &Catalog{entries: entries, byTopic: byTopic, byMatch: byMatch}, nil
}

// LoadFile reads and validates the catalog at path.
func LoadFile(path string) (*Catalog, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied catalog path
	if err != nil {
		return nil, fmt.Errorf("catalog: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return Load(f)
}

// orPlaceholder returns s, or "?" when s is empty, for readable issue ids.
func orPlaceholder(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// allowedPlatformList renders the platform whitelist in a stable order.
func allowedPlatformList() string {
	ps := make([]string, 0, len(allowedPlatforms))
	for p := range allowedPlatforms {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return strings.Join(ps, " ")
}
