// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import "strings"

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

	// DefaultScheduleCatchup is one scheduling slot: a restart inside the slot
	// that just started still establishes its state, while anything older is
	// left alone because a manual change may have happened since.
	DefaultScheduleCatchup = 1800
)

// Normalize is [applyDefaults] under the name the rest of the repository uses
// for it.
//
// Exported so that the one boundary property another package has to be able to
// check can be checked against the REAL normalisation rather than against a
// second spelling of it: the discovery prefix and the MQTT topic root must come
// out of here in the one form both go-hamqtt and internal/hass read the same
// way. See TestTheTwoPlanesAgreeOnTheDiscoveryPrefix.
func (c *Config) Normalize() { applyDefaults(c) }

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
	// Both topic roots are trimmed of surrounding slashes BEFORE the empty
	// check, so "/" normalises to the default rather than to an empty root.
	//
	// This is F15 of the ADR 0070 phase 8 measurement, checked in both
	// directions this time. Empty: publisher.New substitutes
	// discovery.DefaultPrefix ("homeassistant") and internal/hass uses the
	// empty string verbatim, which would put the device documents and this
	// daemon's own discovery filter on different trees. Trailing slash: the
	// library's topicPrefix trims one and internal/layout does not, so
	// HASS_BASE_TOPIC="homeassistant/" would have the runtime publish
	// "homeassistant/device/…" while this package looked under
	// "homeassistant//…". An empty MQTT level is legal and DISTINCT, so the
	// two never meet — which is exactly how go-mtec2mqtt ended up subscribing
	// a birth topic Home Assistant never writes and losing every entity after
	// each HA restart. Normalising at the boundary means there is only one
	// spelling for both packages to agree on.
	c.MQTTTopic = strings.Trim(c.MQTTTopic, "/")
	c.HASSBaseTopic = strings.Trim(c.HASSBaseTopic, "/")
	if c.MQTTTopic == "" {
		c.MQTTTopic = TopicRoot
	}
	if c.MQTTClientID == "" {
		c.MQTTClientID = DefaultMQTTClientID
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
	if c.ScheduleCatchup == 0 {
		c.ScheduleCatchup = DefaultScheduleCatchup
	}
}
