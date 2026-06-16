// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package catalog

import "strings"

// langDE is the language code for German overrides. Any other language code
// (including the canonical "en") falls back to the English fields.
const langDE = "de"

// LocalizedName returns the entry name in the requested language. German is
// returned only when lang is "de" and a German name is set; otherwise the
// canonical English name is returned (per-item fallback).
func (e *Entry) LocalizedName(lang string) string {
	if lang == langDE && e.NameDE != "" {
		return e.NameDE
	}
	return e.Name
}

// LocalizedLabel returns the friendly label for a raw API value in the
// requested language. It falls back, in order, to the English label and
// finally to the raw value itself, so an unmapped value still yields a
// usable string.
func (e *Entry) LocalizedLabel(value, lang string) string {
	for i := range e.Values {
		v := &e.Values[i]
		if v.Value != value {
			continue
		}
		if lang == langDE && v.LabelDE != "" {
			return v.LabelDE
		}
		if v.Label != "" {
			return v.Label
		}
		return v.Value
	}
	return value
}

// CodeForLabel performs a reverse lookup from a friendly label back to its
// raw API value. It accepts either the English or the German label (and the
// raw value itself), matching case-insensitively. The second result is
// false when no value maps to the given label.
func (e *Entry) CodeForLabel(label string) (string, bool) {
	want := strings.TrimSpace(label)
	if want == "" {
		return "", false
	}
	for i := range e.Values {
		v := &e.Values[i]
		if strings.EqualFold(v.Value, want) ||
			strings.EqualFold(v.Label, want) ||
			strings.EqualFold(v.LabelDE, want) {
			return v.Value, true
		}
	}
	return "", false
}
