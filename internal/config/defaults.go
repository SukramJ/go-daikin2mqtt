// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

// Default values applied when the YAML omits a field. Mandatory fields
// (the cloud credentials and MQTT server) have no default and are caught
// by [Validate] when missing.
const (
	// DefaultRedirectURI / DefaultOAuthCallbackBind keep first-time auth
	// working out of the box on a host where the operator runs a browser
	// locally. Both must be consistent with the redirect URI registered in
	// the Daikin Developer Portal.
	DefaultRedirectURI       = "http://localhost:8080/callback"
	DefaultOAuthCallbackBind = "127.0.0.1:8080"

	// DefaultRefreshDayInterval / DefaultRefreshNightInterval are slow on
	// purpose: the ONECTA cloud API is rate limited per minute and per day.
	DefaultRefreshDayInterval   = 600  // 10 min
	DefaultRefreshNightInterval = 1800 // 30 min
	DefaultDayStartHour         = 7
	DefaultDayEndHour           = 22
	// DefaultScanIgnore skips GETs for a short window after a PATCH because
	// the cloud serves stale data immediately after a write.
	DefaultScanIgnore = 30

	DefaultMQTTPort     = 1883
	DefaultMQTTLogin    = ""
	DefaultMQTTPassword = ""

	DefaultHASSEnable    = false
	DefaultHASSBaseTopic = "homeassistant"

	// DefaultWebBind binds the optional UI to localhost only. Operators who
	// want LAN access set WEB_BIND: 0.0.0.0:8080 explicitly.
	DefaultWebBind = "127.0.0.1:8080"

	// DefaultLanguage is the fallback UI / HA display language.
	DefaultLanguage = "en"

	// DefaultLocalFaikinPort / DefaultLocalFaikinPrefix match the revk Faikin
	// firmware defaults: plain MQTT on 1883 and the "Faikout" app prefix on the
	// command topics.
	DefaultLocalFaikinPort   = 1883
	DefaultLocalFaikinPrefix = "Faikout"
)

// applyDefaults fills in any field whose YAML+env round left it at its
// zero value with the documented default. Connection parameters without a
// default are left at zero and caught by [Validate].
func applyDefaults(c *Config) {
	if c.RedirectURI == "" {
		c.RedirectURI = DefaultRedirectURI
	}
	if c.OAuthCallbackBind == "" {
		c.OAuthCallbackBind = DefaultOAuthCallbackBind
	}
	if c.RefreshDayInterval == 0 {
		c.RefreshDayInterval = DefaultRefreshDayInterval
	}
	if c.RefreshNightInterval == 0 {
		c.RefreshNightInterval = DefaultRefreshNightInterval
	}
	// DayStartHour/DayEndHour: 0 is a legitimate hour, so only default when
	// both are zero (i.e. the field was omitted entirely).
	if c.DayStartHour == 0 && c.DayEndHour == 0 {
		c.DayStartHour = DefaultDayStartHour
		c.DayEndHour = DefaultDayEndHour
	}
	if c.ScanIgnore == 0 {
		c.ScanIgnore = DefaultScanIgnore
	}
	if c.MQTTPort == 0 {
		c.MQTTPort = DefaultMQTTPort
	}
	if c.MQTTTopic == "" {
		c.MQTTTopic = TopicRoot
	}
	if c.HASSBaseTopic == "" {
		c.HASSBaseTopic = DefaultHASSBaseTopic
	}
	if c.WebBind == "" {
		c.WebBind = DefaultWebBind
	}
	if c.Language == "" {
		c.Language = DefaultLanguage
	}
	if c.LocalFaikinPort == 0 {
		c.LocalFaikinPort = DefaultLocalFaikinPort
	}
	if c.LocalFaikinPrefix == "" {
		c.LocalFaikinPrefix = DefaultLocalFaikinPrefix
	}
}
