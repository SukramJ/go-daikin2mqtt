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
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DeviceMap maps an ONECTA device ID to the Faikin host that controls it. It
// unmarshals from either a YAML mapping or an "id1=host1,id2=host2" string, so
// the same field works in config.yaml and via the DAIKIN_LOCAL_DEVICE_MAP env
// var / the HA add-on (both of which can only pass scalar strings).
type DeviceMap map[string]string

// UnmarshalYAML accepts a mapping node or a scalar "id=host,..." string.
func (m *DeviceMap) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.MappingNode:
		raw := map[string]string{}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		*m = raw
	case yaml.ScalarNode:
		*m = parseDeviceMapString(node.Value)
	default:
		return fmt.Errorf("LOCAL_DEVICE_MAP must be a mapping or an \"id=host,...\" string")
	}
	return nil
}

// parseDeviceMapString parses "id1=host one,id2=host two" into a DeviceMap.
// Entries are comma- or semicolon-separated; keys are trimmed and host names
// keep their internal spaces (e.g. "Klima GA").
func parseDeviceMapString(s string) DeviceMap {
	m := DeviceMap{}
	for _, pair := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

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
	// ClientID / ClientSecret are the OAuth2 credentials issued by the Daikin
	// Developer Portal. They are required for any cloud access, but the daemon
	// still starts without them (a fresh add-on install has them empty) so the
	// operator can configure them via the UI rather than hitting a crash-loop;
	// see [Config.CredentialsConfigured].
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

	// --- Local-first control (Faikin / Faikout ESP32 modules) ---
	// LocalMode routes reads and writes through the indoor units' local Faikin
	// MQTT interface (revk/ESP32-Faikout) instead of the ONECTA cloud. It is a
	// global switch; the cloud path still serves what Faikin does not expose.
	// See [Config.LocalEnabled].
	LocalMode bool `yaml:"LOCAL_MODE"`
	// LocalFaikinServer / LocalFaikinPort address the MQTT broker the Faikin
	// modules publish to. An empty server defaults to MQTTServer (same broker).
	LocalFaikinServer string `yaml:"LOCAL_FAIKIN_SERVER"`
	LocalFaikinPort   int    `yaml:"LOCAL_FAIKIN_PORT"`
	// LocalFaikinLogin / LocalFaikinPassword authenticate to that broker. Empty
	// values fall back to MQTTLogin / MQTTPassword — the common case, where the
	// Faikin modules share the daemon's broker credentials.
	LocalFaikinLogin    string `yaml:"LOCAL_FAIKIN_LOGIN"`
	LocalFaikinPassword string `yaml:"LOCAL_FAIKIN_PASSWORD"`
	// LocalFaikinPrefix is the Faikin firmware "app" name that prefixes the
	// command topics (`<prefix>/<host>/command/control`), e.g. "Faikout".
	LocalFaikinPrefix string `yaml:"LOCAL_FAIKIN_PREFIX"`
	// LocalDeviceMap maps an ONECTA device ID to the Faikin host name that
	// controls it (e.g. "cfcbab3e-…": "Klima GA"). Only mapped devices can be
	// driven locally; unmapped devices stay cloud-only even in local mode.
	LocalDeviceMap DeviceMap `yaml:"LOCAL_DEVICE_MAP"`

	// --- Multi-split outdoor-unit constraints ---
	// These govern settings that ONECTA exposes per indoor unit but that are
	// physically shared across all indoor units on one outdoor unit. All three
	// default to ON (nil → true); set false to disable. See the getter methods.
	//
	// MultiSplitModeSync propagates a heat/cool operationMode change to the
	// other indoor units of the same outdoor unit, since a standard multi-split
	// cannot cool and heat simultaneously (conflicting units go to standby).
	MultiSplitModeSync *bool `yaml:"MULTISPLIT_MODE_SYNC"`
	// MultiSplitOutdoorAggregate surfaces outdoor-shared settings (outdoor
	// silent, demand control) as a single entity per outdoor unit and fans
	// writes out to all member indoor units.
	MultiSplitOutdoorAggregate *bool `yaml:"MULTISPLIT_OUTDOOR_AGGREGATE"`
	// EnforceMutualExclusive clears the mutually-exclusive partner of a setting
	// (powerful ⇄ econo) when one is switched on.
	EnforceMutualExclusive *bool `yaml:"ENFORCE_MUTUAL_EXCLUSIVE"`

	// --- Misc ---
	Debug bool `yaml:"DEBUG"`
}

// LocalEnabled reports whether local-first control is active and usable: the
// switch is on and at least one device is mapped to a Faikin host.
func (c *Config) LocalEnabled() bool {
	return c.LocalMode && len(c.LocalDeviceMap) > 0
}

// FaikinBrokerAddress returns the "host:port" of the Faikin MQTT broker,
// defaulting the host to MQTTServer when LOCAL_FAIKIN_SERVER is empty.
func (c *Config) FaikinBrokerAddress() string {
	host := c.LocalFaikinServer
	if host == "" {
		host = c.MQTTServer
	}
	return net.JoinHostPort(host, strconv.Itoa(c.LocalFaikinPort))
}

// FaikinLogin returns the effective Faikin broker username, falling back to the
// main MQTT username when LOCAL_FAIKIN_LOGIN is unset.
func (c *Config) FaikinLogin() string {
	if c.LocalFaikinLogin != "" {
		return c.LocalFaikinLogin
	}
	return c.MQTTLogin
}

// FaikinPassword returns the effective Faikin broker password, falling back to
// the main MQTT password when LOCAL_FAIKIN_PASSWORD is unset.
func (c *Config) FaikinPassword() string {
	if c.LocalFaikinPassword != "" {
		return c.LocalFaikinPassword
	}
	return c.MQTTPassword
}

// FaikinHost returns the Faikin host name that controls the given ONECTA device
// ID, or ("", false) when the device is not mapped for local control.
func (c *Config) FaikinHost(deviceID string) (string, bool) {
	h, ok := c.LocalDeviceMap[deviceID]
	return h, ok
}

// ModeSyncEnabled reports whether heat/cool mode changes propagate across an
// outdoor unit's indoor units. Defaults to true when unset.
func (c *Config) ModeSyncEnabled() bool {
	return c.MultiSplitModeSync == nil || *c.MultiSplitModeSync
}

// OutdoorAggregateEnabled reports whether outdoor-shared settings are surfaced
// once per outdoor unit with fan-out writes. Defaults to true when unset.
func (c *Config) OutdoorAggregateEnabled() bool {
	return c.MultiSplitOutdoorAggregate == nil || *c.MultiSplitOutdoorAggregate
}

// MutualExclusiveEnforced reports whether switching one of a mutually-exclusive
// pair (powerful ⇄ econo) on clears the other. Defaults to true when unset.
func (c *Config) MutualExclusiveEnforced() bool {
	return c.EnforceMutualExclusive == nil || *c.EnforceMutualExclusive
}

// FaikinSharesMainBroker reports whether the Faikin broker address equals the
// main MQTT broker, so the existing connection can be reused instead of opening
// a second one (the common case: the modules publish to the same broker).
func (c *Config) FaikinSharesMainBroker() bool {
	return c.FaikinBrokerAddress() == net.JoinHostPort(c.MQTTServer, strconv.Itoa(c.MQTTPort))
}

// CredentialsConfigured reports whether both OAuth client credentials are set.
// The daemon starts without them so a fresh install does not crash-loop, but
// no cloud access (auth, polling, writes) is possible until both are present.
func (c *Config) CredentialsConfigured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
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
