// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
)

// writeTestCA writes a freshly generated self-signed CA certificate as PEM
// and returns its path.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadTestConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader("CLIENT_ID: abc\nCLIENT_SECRET: secret\n"+yaml), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestMainBrokerConfigPlain(t *testing.T) {
	cfg := loadTestConfig(t, "MQTT_SERVER: broker.lan\n")
	got, err := mainBrokerConfig(cfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("mainBrokerConfig: %v", err)
	}
	if got.BrokerURL != "tcp://broker.lan:1883" {
		t.Errorf("BrokerURL = %q, want tcp://broker.lan:1883", got.BrokerURL)
	}
	if got.TLSConfig != nil {
		t.Errorf("TLSConfig = %v, want nil while MQTT_TLS is false", got.TLSConfig)
	}
}

func TestMainBrokerConfigTLSWithCA(t *testing.T) {
	ca := writeTestCA(t)
	cfg := loadTestConfig(t, "MQTT_SERVER: broker.lan\nMQTT_TLS: true\nMQTT_TLS_CA: "+ca+"\n")
	got, err := mainBrokerConfig(cfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("mainBrokerConfig: %v", err)
	}
	if got.BrokerURL != "tls://broker.lan:8883" {
		t.Errorf("BrokerURL = %q, want tls://broker.lan:8883", got.BrokerURL)
	}
	tc := got.TLSConfig
	if tc == nil {
		t.Fatal("TLSConfig = nil, want one while MQTT_TLS is true")
	}
	if tc.ServerName != "broker.lan" {
		t.Errorf("ServerName = %q, want the broker hostname", tc.ServerName)
	}
	if tc.RootCAs == nil {
		t.Error("RootCAs = nil, want the MQTT_TLS_CA bundle")
	}
	if tc.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false")
	}
}

func TestMainBrokerConfigTLSInsecure(t *testing.T) {
	cfg := loadTestConfig(t, "MQTT_SERVER: broker.lan\nMQTT_TLS: true\nMQTT_TLS_INSECURE: true\n")
	got, err := mainBrokerConfig(cfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("mainBrokerConfig: %v", err)
	}
	if got.TLSConfig == nil || !got.TLSConfig.InsecureSkipVerify {
		t.Errorf("TLSConfig = %v, want InsecureSkipVerify", got.TLSConfig)
	}
	if got.TLSConfig != nil && got.TLSConfig.RootCAs != nil {
		t.Error("RootCAs set without MQTT_TLS_CA, want the system roots")
	}
}

func TestMainBrokerConfigBadCAStopsStartup(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.crt")
	cfg := loadTestConfig(t, "MQTT_SERVER: broker.lan\nMQTT_TLS: true\nMQTT_TLS_CA: "+missing+"\n")
	_, err := mainBrokerConfig(cfg, nil, slog.Default())
	if err == nil {
		t.Fatal("mainBrokerConfig accepted a missing CA file")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the CA file", err)
	}
}
