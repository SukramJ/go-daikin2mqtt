// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestMQTTTLSFromYAML(t *testing.T) {
	cfg, err := Load(strings.NewReader(minimalYAML+"MQTT_TLS: true\nMQTT_TLS_CA: /ssl/ca.crt\n"), mapEnv{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MQTTTLS || cfg.MQTTTLSInsecure || cfg.MQTTTLSCA != "/ssl/ca.crt" {
		t.Errorf("TLS = %v, insecure = %v, CA = %q; want true, false, /ssl/ca.crt",
			cfg.MQTTTLS, cfg.MQTTTLSInsecure, cfg.MQTTTLSCA)
	}
	if cfg.MQTTPort != DefaultMQTTTLSPort {
		t.Errorf("MQTTPort = %d, want the TLS default %d", cfg.MQTTPort, DefaultMQTTTLSPort)
	}
	if got, want := cfg.MQTTBrokerURL(), "tls://broker.local:8883"; got != want {
		t.Errorf("MQTTBrokerURL = %q, want %q", got, want)
	}
}

func TestMQTTTLSFromEnv(t *testing.T) {
	cfg, err := Load(strings.NewReader(minimalYAML), mapEnv{
		"DAIKIN_MQTT_TLS":          "true",
		"DAIKIN_MQTT_TLS_INSECURE": "true",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MQTTTLS || !cfg.MQTTTLSInsecure || cfg.MQTTTLSCA != "" {
		t.Errorf("TLS = %v, insecure = %v, CA = %q; want true, true, empty",
			cfg.MQTTTLS, cfg.MQTTTLSInsecure, cfg.MQTTTLSCA)
	}
	if cfg.MQTTPort != DefaultMQTTTLSPort {
		t.Errorf("MQTTPort = %d, want the TLS default %d", cfg.MQTTPort, DefaultMQTTTLSPort)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "MQTT_TLS_INSECURE is on") {
		t.Errorf("Warnings = %q, want the insecure warning", w)
	}
}

func TestMQTTTLSExplicitPortWins(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		env        mapEnv
		want       int
	}{
		{"yaml port with TLS", "MQTT_TLS: true\nMQTT_PORT: 1883\n", mapEnv{}, 1883},
		{"env port with TLS", "MQTT_TLS: true\n", mapEnv{"DAIKIN_MQTT_PORT": "18883"}, 18883},
		{"no TLS, no port", "", mapEnv{}, DefaultMQTTPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(strings.NewReader(minimalYAML+tc.yaml), tc.env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.MQTTPort != tc.want {
				t.Errorf("MQTTPort = %d, want %d", cfg.MQTTPort, tc.want)
			}
		})
	}
}

func TestMQTTTLSCAWithInsecureRejected(t *testing.T) {
	_, err := Load(strings.NewReader(minimalYAML+
		"MQTT_TLS: true\nMQTT_TLS_INSECURE: true\nMQTT_TLS_CA: /ssl/ca.crt\n"), mapEnv{})
	verr, ok := errors.AsType[*ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	if !slices.ContainsFunc(verr.Issues, func(s string) bool {
		return strings.Contains(s, "MQTT_TLS_CA and MQTT_TLS_INSECURE")
	}) {
		t.Errorf("issues %q do not name the CA/insecure conflict", verr.Issues)
	}
}

func TestMQTTTLSOptionsWithoutTLSWarn(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  mapEnv
		want string
	}{
		{"insecure", mapEnv{"DAIKIN_MQTT_TLS_INSECURE": "true"}, "MQTT_TLS_INSECURE has no effect while MQTT_TLS is false"},
		{"ca", mapEnv{"DAIKIN_MQTT_TLS_CA": "/ssl/ca.crt"}, "MQTT_TLS_CA has no effect while MQTT_TLS is false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(strings.NewReader(minimalYAML), tc.env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if w := cfg.Warnings(); !slices.Equal(w, []string{tc.want}) {
				t.Errorf("Warnings = %q, want [%q]", w, tc.want)
			}
			if got, want := cfg.MQTTBrokerURL(), "tcp://broker.local:1883"; got != want {
				t.Errorf("MQTTBrokerURL = %q, want %q", got, want)
			}
		})
	}
}

const localYAML = `
LOCAL_MODE: true
LOCAL_DEVICE_MAP:
  cfcbab3e: Klima GA
`

// TestFaikinFallbackSharesTheTLSConnection pins the safe default for the
// Faikin broker under MQTT_TLS: with LOCAL_FAIKIN_SERVER and
// LOCAL_FAIKIN_PORT both unset the Faikin broker is the main broker, so the
// TLS connection is reused — never a second, plaintext tcp:// connection to the
// main broker's host (on 1883, carrying the main credentials in the clear, or
// against its TLS port).
func TestFaikinFallbackSharesTheTLSConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		port int
	}{
		{"default TLS port", "MQTT_TLS: true\n", 8883},
		{"explicit TLS port", "MQTT_TLS: true\nMQTT_PORT: 8884\n", 8884},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(strings.NewReader(minimalYAML+localYAML+tc.yaml), mapEnv{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LocalFaikinPort != tc.port {
				t.Errorf("LocalFaikinPort = %d, want %d (follows MQTT_PORT)", cfg.LocalFaikinPort, tc.port)
			}
			if !cfg.FaikinSharesMainBroker() {
				t.Errorf("FaikinSharesMainBroker = false (%s vs %s), want the TLS connection reused",
					cfg.FaikinBrokerAddress(), cfg.MQTTBrokerURL())
			}
			if w := cfg.Warnings(); len(w) != 0 {
				t.Errorf("Warnings = %q, want none", w)
			}
		})
	}
}

// TestFaikinWithoutTLSIsUnchanged pins that MQTT_TLS off leaves the Faikin
// defaults exactly as before: port 1883 regardless of MQTT_PORT.
func TestFaikinWithoutTLSIsUnchanged(t *testing.T) {
	cfg, err := Load(strings.NewReader(minimalYAML+localYAML+"MQTT_PORT: 1884\n"), mapEnv{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalFaikinPort != DefaultLocalFaikinPort {
		t.Errorf("LocalFaikinPort = %d, want %d", cfg.LocalFaikinPort, DefaultLocalFaikinPort)
	}
	if got, want := cfg.FaikinBrokerURL(), "tcp://broker.local:1883"; got != want {
		t.Errorf("FaikinBrokerURL = %q, want %q", got, want)
	}
}

// TestFaikinExplicitBrokerStaysPlainUnderTLS pins that an explicitly
// configured Faikin broker keeps its plain tcp:// connection while the main
// link is TLS, and that the daemon says so.
func TestFaikinExplicitBrokerStaysPlainUnderTLS(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, url string
	}{
		{"own server", "LOCAL_FAIKIN_SERVER: 172.18.4.22\n", "tcp://172.18.4.22:1883"},
		{"main host, own port", "LOCAL_FAIKIN_PORT: 1883\n", "tcp://broker.local:1883"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(strings.NewReader(minimalYAML+localYAML+"MQTT_TLS: true\n"+tc.yaml), mapEnv{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.FaikinSharesMainBroker() {
				t.Fatal("FaikinSharesMainBroker = true, want a separate connection")
			}
			if got := cfg.FaikinBrokerURL(); got != tc.url {
				t.Errorf("FaikinBrokerURL = %q, want %q", got, tc.url)
			}
			if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "Faikin broker connection") {
				t.Errorf("Warnings = %q, want the plaintext Faikin warning", w)
			}
		})
	}
}
