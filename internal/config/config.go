// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package config holds the daemon's runtime settings.
//
// Values flow: YAML file → env overrides (DAIKIN_* prefix) → defaults →
// validation. The result is a single typed [Config] the rest of the
// daemon reads from.
//
// YAML keys are unprefixed (e.g. CLIENT_ID, MQTT_SERVER); the matching
// environment override prepends the DAIKIN_ prefix (DAIKIN_CLIENT_ID,
// DAIKIN_MQTT_SERVER). Keeping the YAML keys prefix-free avoids the
// awkward DAIKIN_DAIKIN_* doubling an in-YAML prefix would create.
package config

import (
	"time"
)

// Daemon-wide constants.
const (
	// MQTTClientID is the MQTT client identifier the bridge connects with.
	MQTTClientID = "daikin2mqtt"
	// TopicRoot is the default MQTT topic root (overridable via MQTT_TOPIC).
	TopicRoot = "daikin"
	// EnvPrefix is the environment-variable override prefix.
	EnvPrefix = "DAIKIN_"
	// AppDirName is the per-user config/data directory name under XDG.
	AppDirName = "daikin2mqtt"
	// ConfigFile is the default config file name searched by [Locate].
	ConfigFile = "config.yaml"
	// TokenStoreFile is the default token-store file name under the app dir.
	TokenStoreFile = "token-store.json"
)

// Config is the validated daemon configuration. Fields are flat to match
// the YAML keys 1:1 — grouping into sub-structs would force a custom
// unmarshaller for what is otherwise a trivial yaml.v3 decode.
//
// Interval fields are stored as plain seconds (matching the YAML) and
// surfaced as time.Duration through the helper methods below so callers
// never multiply by time.Second themselves.
type Config struct {
	// --- Daikin / ONECTA cloud (OAuth2) ---
	// ClientID / ClientSecret are the OAuth2 credentials issued by the
	// Daikin Developer Portal. Both are mandatory.
	ClientID     string `yaml:"CLIENT_ID"`
	ClientSecret string `yaml:"CLIENT_SECRET"`
	// RedirectURI must match a redirect URI registered for the client in
	// the Daikin Developer Portal. It points at the daemon's OAuth callback
	// endpoint (served by the web UI when enabled, otherwise by a temporary
	// callback server during first-time authentication).
	RedirectURI string `yaml:"REDIRECT_URI"`
	// OAuthCallbackBind is the listen address "host:port" for the temporary
	// callback server used when the web UI is disabled. Ignored once a
	// valid refresh token is present.
	OAuthCallbackBind string `yaml:"OAUTH_CALLBACK_BIND"`
	// TokenStorePath is the file the rotated refresh token is persisted to.
	// Empty selects the XDG default (see [Config.ResolveTokenStorePath]).
	TokenStorePath string `yaml:"TOKEN_STORE_PATH"`

	// --- Polling / rate limiting (seconds) ---
	// RefreshDayInterval / RefreshNightInterval are the poll intervals used
	// during day / night hours respectively (the cloud API is rate limited,
	// so polling is deliberately slow and time-of-day aware).
	RefreshDayInterval   int `yaml:"REFRESH_DAY_INTERVAL"`
	RefreshNightInterval int `yaml:"REFRESH_NIGHT_INTERVAL"`
	// DayStartHour / DayEndHour bound the "day" window [start, end) in local
	// hours (0..24). Outside the window the night interval applies.
	DayStartHour int `yaml:"DAY_START_HOUR"`
	DayEndHour   int `yaml:"DAY_END_HOUR"`
	// ScanIgnore is how long after a PATCH the daemon skips GETs, because
	// the cloud returns stale data for a short window after a write.
	ScanIgnore int `yaml:"SCAN_IGNORE"`

	// --- MQTT ---
	MQTTServer   string `yaml:"MQTT_SERVER"`
	MQTTPort     int    `yaml:"MQTT_PORT"`
	MQTTLogin    string `yaml:"MQTT_LOGIN"`
	MQTTPassword string `yaml:"MQTT_PASSWORD"`
	MQTTTopic    string `yaml:"MQTT_TOPIC"`

	// --- Home Assistant ---
	HASSEnable    bool   `yaml:"HASS_ENABLE"`
	HASSBaseTopic string `yaml:"HASS_BASE_TOPIC"`

	// --- Diagnostic web UI (optional) ---
	// WebEnable toggles the embedded HTTP server. Off by default so the
	// daemon stays a pure MQTT bridge unless an operator opts in. When on,
	// the same server also hosts the OAuth callback endpoint.
	WebEnable bool `yaml:"WEB_ENABLE"`
	// WebBind is the listen address "host:port". Defaults to
	// 127.0.0.1:8080 — localhost-only — so enabling the UI never exposes it
	// to the network by accident.
	WebBind string `yaml:"WEB_BIND"`
	// WebUser / WebPassword enable HTTP Basic auth when both are set. Leave
	// both empty to serve without authentication (e.g. behind HA ingress or
	// a reverse proxy).
	WebUser     string `yaml:"WEB_USER"`
	WebPassword string `yaml:"WEB_PASSWORD"`

	// --- Localisation ---
	// Language selects the UI / Home-Assistant display language: "en"
	// (default) or "de". It localises the web dashboard chrome and the
	// friendly names of HA entities; topics and entity_ids stay
	// language-independent.
	Language string `yaml:"LANGUAGE"`

	// --- Misc ---
	Debug bool `yaml:"DEBUG"`
}

// RefreshDayIntervalDuration returns RefreshDayInterval as a duration.
func (c *Config) RefreshDayIntervalDuration() time.Duration {
	return time.Duration(c.RefreshDayInterval) * time.Second
}

// RefreshNightIntervalDuration returns RefreshNightInterval as a duration.
func (c *Config) RefreshNightIntervalDuration() time.Duration {
	return time.Duration(c.RefreshNightInterval) * time.Second
}

// ScanIgnoreDuration returns ScanIgnore as a duration.
func (c *Config) ScanIgnoreDuration() time.Duration {
	return time.Duration(c.ScanIgnore) * time.Second
}

// PollInterval returns the refresh interval that applies at hour h
// (local 0..23): the day interval inside [DayStartHour, DayEndHour),
// otherwise the night interval. Windows that wrap past midnight
// (start > end) are handled too.
func (c *Config) PollInterval(hour int) time.Duration {
	day := c.DayStartHour <= hour && hour < c.DayEndHour
	if c.DayStartHour > c.DayEndHour { // window wraps midnight
		day = hour >= c.DayStartHour || hour < c.DayEndHour
	}
	if day {
		return c.RefreshDayIntervalDuration()
	}
	return c.RefreshNightIntervalDuration()
}
