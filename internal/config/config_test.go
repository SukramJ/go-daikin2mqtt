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
		{"MQTTClientID", cfg.MQTTClientID, DefaultMQTTClientID},
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

func TestLocalModeDefaultsAndFallbacks(t *testing.T) {
	y := minimalYAML + `
MQTT_LOGIN: ha
MQTT_PASSWORD: pw
LOCAL_MODE: true
LOCAL_FAIKIN_SERVER: 172.18.4.22
LOCAL_DEVICE_MAP:
  cfcbab3e: Klima GA
  1921496f: Klima SZ
`
	cfg, err := Load(strings.NewReader(y), mapEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LocalEnabled() {
		t.Error("LocalEnabled should be true")
	}
	if cfg.LocalFaikinPort != DefaultLocalFaikinPort || cfg.LocalFaikinPrefix != DefaultLocalFaikinPrefix {
		t.Errorf("defaults not applied: port=%d prefix=%q", cfg.LocalFaikinPort, cfg.LocalFaikinPrefix)
	}
	if got := cfg.FaikinBrokerAddress(); got != "172.18.4.22:1883" {
		t.Errorf("FaikinBrokerAddress = %q", got)
	}
	// Credentials fall back to the main MQTT creds when unset.
	if cfg.FaikinLogin() != "ha" || cfg.FaikinPassword() != "pw" {
		t.Errorf("creds fallback = %q/%q, want ha/pw", cfg.FaikinLogin(), cfg.FaikinPassword())
	}
	if h, ok := cfg.FaikinHost("cfcbab3e"); !ok || h != "Klima GA" {
		t.Errorf("FaikinHost(cfcbab3e) = %q,%v", h, ok)
	}
	if _, ok := cfg.FaikinHost("unmapped"); ok {
		t.Error("unmapped device should not resolve")
	}
}

func TestLocalModeEmptyMapRejected(t *testing.T) {
	_, err := Load(strings.NewReader(minimalYAML+"\nLOCAL_MODE: true\n"), mapEnv{})
	if err == nil || !strings.Contains(err.Error(), "LOCAL_DEVICE_MAP") {
		t.Fatalf("want LOCAL_DEVICE_MAP validation error, got %v", err)
	}
}

func TestFaikinBrokerDefaultsToMQTTServer(t *testing.T) {
	// LOCAL_FAIKIN_SERVER empty → host falls back to MQTT_SERVER.
	cfg := &Config{MQTTServer: "broker.local", LocalFaikinPort: 1883}
	if got := cfg.FaikinBrokerAddress(); got != "broker.local:1883" {
		t.Errorf("FaikinBrokerAddress = %q", got)
	}
}

func TestLocalDeviceMapStringForm(t *testing.T) {
	// Inline string form (as the add-on / env would deliver it).
	y := minimalYAML + `
LOCAL_MODE: true
LOCAL_FAIKIN_SERVER: 172.18.4.22
LOCAL_DEVICE_MAP: "id1=Klima GA, id2=Klima SZ"
`
	cfg, err := Load(strings.NewReader(y), mapEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := cfg.FaikinHost("id1"); !ok || h != "Klima GA" {
		t.Errorf("id1 -> %q,%v (host keeps internal space)", h, ok)
	}
	if h, _ := cfg.FaikinHost("id2"); h != "Klima SZ" {
		t.Errorf("id2 -> %q", h)
	}
}

func TestLocalDeviceMapFromEnv(t *testing.T) {
	// The HA add-on passes the map via DAIKIN_LOCAL_DEVICE_MAP (a scalar).
	env := mapEnv{
		"DAIKIN_LOCAL_MODE":          "true",
		"DAIKIN_LOCAL_FAIKIN_SERVER": "172.18.4.22",
		"DAIKIN_LOCAL_DEVICE_MAP":    "abc=Klima WZ",
	}
	cfg, err := Load(strings.NewReader(minimalYAML), env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LocalEnabled() {
		t.Fatal("LocalEnabled should be true")
	}
	if h, _ := cfg.FaikinHost("abc"); h != "Klima WZ" {
		t.Errorf("env map abc -> %q", h)
	}
}

// TestMQTTClientIDIsConfigurable pins F1 of the ADR 0070 phase 8
// measurement: the client identifier was a compile-time constant with no
// override, so two daemons on one broker presented the same id and the
// broker disconnected each in turn (MQTT 3.1.1 §3.1.3.2 / 5.0 §3.1.4).
//
// Three things have to hold at once for the fix to be safe: the key works
// from YAML, it works from the environment (the add-on supplies every
// setting that way), and saying nothing still yields the exact string the
// daemon used before the key existed.
func TestMQTTClientIDIsConfigurable(t *testing.T) {
	t.Parallel()

	t.Run("from yaml", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(strings.NewReader(minimalYAML+"MQTT_CLIENT_ID: daikin2mqtt-staging\n"), mapEnv{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MQTTClientID != "daikin2mqtt-staging" {
			t.Errorf("MQTTClientID = %q, want %q", cfg.MQTTClientID, "daikin2mqtt-staging")
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(strings.NewReader(minimalYAML), mapEnv{EnvPrefix + "MQTT_CLIENT_ID": "daikin2mqtt-b"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MQTTClientID != "daikin2mqtt-b" {
			t.Errorf("MQTTClientID = %q, want %q", cfg.MQTTClientID, "daikin2mqtt-b")
		}
	})

	t.Run("unset keeps the pre-fix session", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(strings.NewReader(minimalYAML), mapEnv{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// The literal, not the constant: an installation that says nothing
		// must present the byte-identical id it presented before this key
		// existed, or its first restart looks like a takeover to the broker.
		if cfg.MQTTClientID != "daikin2mqtt" {
			t.Errorf("default MQTTClientID = %q, want %q", cfg.MQTTClientID, "daikin2mqtt")
		}
		if cfg.MQTTClientID+FaikinClientIDSuffix != "daikin2mqtt-faikin" {
			t.Errorf("faikin client id = %q, want %q",
				cfg.MQTTClientID+FaikinClientIDSuffix, "daikin2mqtt-faikin")
		}
	})

	t.Run("blank and whitespace are refused", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{`" "`, `"a b"`, `"a\tb"`} {
			_, err := Load(strings.NewReader(minimalYAML+"MQTT_CLIENT_ID: "+bad+"\n"), mapEnv{})
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("MQTT_CLIENT_ID %s: got %v, want a ValidationError", bad, err)
				continue
			}
			if !slices.ContainsFunc(ve.Issues, func(s string) bool { return strings.Contains(s, "MQTT_CLIENT_ID") }) {
				t.Errorf("MQTT_CLIENT_ID %s: issues %v name no MQTT_CLIENT_ID problem", bad, ve.Issues)
			}
		}
	})

	t.Run("two instances can differ", func(t *testing.T) {
		t.Parallel()
		a, err := Load(strings.NewReader(minimalYAML+"MQTT_CLIENT_ID: daikin-a\n"), mapEnv{})
		if err != nil {
			t.Fatalf("Load a: %v", err)
		}
		b, err := Load(strings.NewReader(minimalYAML), mapEnv{EnvPrefix + "MQTT_CLIENT_ID": "daikin-b"})
		if err != nil {
			t.Fatalf("Load b: %v", err)
		}
		if a.MQTTClientID == b.MQTTClientID {
			t.Fatalf("two instances still share the client id %q — F1 is not fixed", a.MQTTClientID)
		}
	})
}
