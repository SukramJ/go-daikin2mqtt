// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ValidationError is returned by [Validate] when the loaded config fails
// one or more range or shape checks. The Issues slice contains
// human-readable problem descriptions in declaration order so the caller
// can log them all in one shot.
type ValidationError struct {
	Issues []string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		return "config: " + e.Issues[0]
	}
	return fmt.Sprintf("config: %d validation issue(s):\n  - %s",
		len(e.Issues), strings.Join(e.Issues, "\n  - "))
}

// allowedLanguages is the set of UI / HA display languages the daemon
// ships translations for. English is the canonical fallback.
var allowedLanguages = map[string]bool{
	"en": true,
	"de": true,
}

// Validate checks the post-defaults config and returns a
// [*ValidationError] aggregating every problem.
func Validate(c *Config) error {
	var issues []string
	add := func(format string, args ...any) {
		issues = append(issues, fmt.Sprintf(format, args...))
	}

	// --- Daikin cloud (OAuth2) ---
	if c.ClientID == "" {
		add("CLIENT_ID is required")
	}
	if c.ClientSecret == "" {
		add("CLIENT_SECRET is required")
	}
	if u, err := url.Parse(c.RedirectURI); err != nil || u.Scheme == "" || u.Host == "" {
		add("REDIRECT_URI must be an absolute URL, got %q", c.RedirectURI)
	}
	if err := validateHostPort(c.OAuthCallbackBind); err != nil {
		add("OAUTH_CALLBACK_BIND %v", err)
	}

	// --- Polling / rate limiting ---
	rangeCheck := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			add("%s must be %d..%d seconds, got %d", name, lo, hi, v)
		}
	}
	rangeCheck("REFRESH_DAY_INTERVAL", c.RefreshDayInterval, 30, 86400)
	rangeCheck("REFRESH_NIGHT_INTERVAL", c.RefreshNightInterval, 30, 86400)
	rangeCheck("SCAN_IGNORE", c.ScanIgnore, 0, 600)
	if c.DayStartHour < 0 || c.DayStartHour > 23 {
		add("DAY_START_HOUR must be 0..23, got %d", c.DayStartHour)
	}
	if c.DayEndHour < 1 || c.DayEndHour > 24 {
		add("DAY_END_HOUR must be 1..24, got %d", c.DayEndHour)
	}

	// --- MQTT ---
	if c.MQTTServer == "" {
		add("MQTT_SERVER is required")
	}
	if c.MQTTPort < 1 || c.MQTTPort > 65535 {
		add("MQTT_PORT must be 1..65535, got %d", c.MQTTPort)
	}
	if c.MQTTTopic == "" {
		add("MQTT_TOPIC is required")
	}

	// --- Diagnostic web UI ---
	// Only meaningful when the server is enabled; an unused bind address
	// shouldn't block startup of a pure-MQTT deployment.
	if c.WebEnable {
		if err := validateHostPort(c.WebBind); err != nil {
			add("WEB_BIND %v", err)
		}
		// Basic auth is all-or-nothing: a username without a password (or
		// vice versa) is almost certainly a config mistake.
		if (c.WebUser == "") != (c.WebPassword == "") {
			add("WEB_USER and WEB_PASSWORD must both be set or both be empty")
		}
	}

	// --- Localisation ---
	if !allowedLanguages[c.Language] {
		add("LANGUAGE must be one of [de en], got %q", c.Language)
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// validateHostPort checks that s is a "host:port" with a port in 1..65535.
// An empty host (":8080") is accepted and means "all interfaces".
func validateHostPort(s string) error {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("must be host:port, got %q: %w", s, err)
	}
	p, perr := strconv.Atoi(port)
	if perr != nil || p < 1 || p > 65535 {
		return fmt.Errorf("port must be 1..65535, got %q", port)
	}
	return nil
}
