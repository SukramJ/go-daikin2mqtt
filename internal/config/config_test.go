// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// mapEnv is a hermetic [Env] backed by a map.
type mapEnv map[string]string

func (m mapEnv) LookupEnv(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m mapEnv) Environ() []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

const minimalYAML = `
CLIENT_ID: abc
CLIENT_SECRET: secret
MQTT_SERVER: broker.local
`

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Load(strings.NewReader(minimalYAML), mapEnv{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"MQTTPort", cfg.MQTTPort, DefaultMQTTPort},
		{"MQTTTopic", cfg.MQTTTopic, TopicRoot},
		{"RedirectURI", cfg.RedirectURI, DefaultRedirectURI},
		{"OAuthCallbackBind", cfg.OAuthCallbackBind, DefaultOAuthCallbackBind},
		{"RefreshDayInterval", cfg.RefreshDayInterval, DefaultRefreshDayInterval},
		{"RefreshNightInterval", cfg.RefreshNightInterval, DefaultRefreshNightInterval},
		{"DayStartHour", cfg.DayStartHour, DefaultDayStartHour},
		{"DayEndHour", cfg.DayEndHour, DefaultDayEndHour},
		{"ScanIgnore", cfg.ScanIgnore, DefaultScanIgnore},
		{"HASSBaseTopic", cfg.HASSBaseTopic, DefaultHASSBaseTopic},
		{"WebBind", cfg.WebBind, DefaultWebBind},
		{"Language", cfg.Language, DefaultLanguage},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadEnvOverride(t *testing.T) {
	env := mapEnv{
		"DAIKIN_MQTT_SERVER": "env.broker", // overrides file
		"DAIKIN_MQTT_PORT":   "8883",       // int coercion
		"DAIKIN_HASS_ENABLE": "false",      // bool coercion
		"DAIKIN_LANGUAGE":    "de",
		"IGNORED_KEY":        "x", // no DAIKIN_ prefix -> ignored
	}
	cfg, err := Load(strings.NewReader(minimalYAML+"\nHASS_ENABLE: true\n"), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MQTTServer != "env.broker" {
		t.Errorf("MQTTServer = %q, want env.broker", cfg.MQTTServer)
	}
	if cfg.MQTTPort != 8883 {
		t.Errorf("MQTTPort = %d, want 8883", cfg.MQTTPort)
	}
	if cfg.HASSEnable {
		t.Errorf("HASSEnable = true, want false (env override)")
	}
	if cfg.Language != "de" {
		t.Errorf("Language = %q, want de", cfg.Language)
	}
}

func TestLoadValidationAggregates(t *testing.T) {
	// Empty config still fails on the genuinely-required MQTT_SERVER.
	_, err := Load(strings.NewReader(""), mapEnv{})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if !slices.ContainsFunc(ve.Issues, func(s string) bool { return strings.Contains(s, "MQTT_SERVER") }) {
		t.Errorf("missing issue mentioning MQTT_SERVER in %v", ve.Issues)
	}
	// CLIENT_ID / CLIENT_SECRET are intentionally NOT fatal so a fresh add-on
	// install starts unconfigured instead of crash-looping.
	for _, iss := range ve.Issues {
		if strings.Contains(iss, "CLIENT_ID") || strings.Contains(iss, "CLIENT_SECRET") {
			t.Errorf("credentials should not be a fatal validation issue, got %q", iss)
		}
	}
}

func TestValidateRejectsUnknownLanguage(t *testing.T) {
	_, err := Load(strings.NewReader(minimalYAML+"\nLANGUAGE: fr\n"), mapEnv{})
	if err == nil || !strings.Contains(err.Error(), "LANGUAGE") {
		t.Fatalf("want LANGUAGE validation error, got %v", err)
	}
}

func TestValidateWebBasicAuthAllOrNothing(t *testing.T) {
	y := minimalYAML + "\nWEB_ENABLE: true\nWEB_USER: admin\n" // password missing
	_, err := Load(strings.NewReader(y), mapEnv{})
	if err == nil || !strings.Contains(err.Error(), "WEB_USER and WEB_PASSWORD") {
		t.Fatalf("want basic-auth validation error, got %v", err)
	}
}

func TestPollInterval(t *testing.T) {
	cfg := &Config{
		RefreshDayInterval:   600,
		RefreshNightInterval: 1800,
		DayStartHour:         7,
		DayEndHour:           22,
	}
	cases := map[int]int{6: 1800, 7: 600, 12: 600, 21: 600, 22: 1800, 23: 1800, 0: 1800}
	for hour, wantSec := range cases {
		if got := cfg.PollInterval(hour); int(got.Seconds()) != wantSec {
			t.Errorf("PollInterval(%d) = %v, want %ds", hour, got, wantSec)
		}
	}

	// Window wrapping past midnight: day = [22, 6).
	wrap := &Config{RefreshDayInterval: 600, RefreshNightInterval: 1800, DayStartHour: 22, DayEndHour: 6}
	for hour, wantSec := range map[int]int{23: 600, 0: 600, 5: 600, 6: 1800, 12: 1800, 21: 1800} {
		if got := wrap.PollInterval(hour); int(got.Seconds()) != wantSec {
			t.Errorf("wrap PollInterval(%d) = %v, want %ds", hour, got, wantSec)
		}
	}
}

func TestResolveTokenStorePathExplicit(t *testing.T) {
	cfg := &Config{TokenStorePath: "/custom/token.json"}
	if got := cfg.ResolveTokenStorePath(mapEnv{}); got != "/custom/token.json" {
		t.Errorf("ResolveTokenStorePath = %q, want /custom/token.json", got)
	}
}

func TestResolveTokenStorePathXDG(t *testing.T) {
	// configCandidates uses APPDATA on Windows and XDG_CONFIG_HOME elsewhere;
	// drive each platform with its own base dir and build the expected path
	// with filepath.Join so the separators match too.
	envKey, base := "XDG_CONFIG_HOME", "/xdg"
	if runtime.GOOS == "windows" {
		envKey, base = "APPDATA", `C:\xdg`
	}
	cfg := &Config{}
	got := cfg.ResolveTokenStorePath(mapEnv{envKey: base})
	want := filepath.Join(base, AppDirName, TokenStoreFile)
	if got != want {
		t.Errorf("ResolveTokenStorePath = %q, want %q", got, want)
	}
}
