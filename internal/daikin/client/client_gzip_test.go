// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubTokens is a minimal TokenProvider for tests.
type gzipStubTokens struct{}

func (gzipStubTokens) Token(context.Context) (string, error) { return "tok", nil }

// TestGetDevicesDecompressesGzip is a regression test: the cloud returns
// gzip-compressed JSON. The client must NOT set Accept-Encoding manually,
// so Go's transport transparently decompresses the body. If a manual
// Accept-Encoding header is reintroduced, this test fails because the raw
// gzip bytes (starting with 0x1f 0x8b) leak through.
func TestGetDevicesDecompressesGzip(t *testing.T) {
	want := `[{"id":"dev-1"}]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write([]byte(want))
		_ = gz.Close()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Tokens: gzipStubTokens{}})
	got, err := c.GetDevices(context.Background())
	if err != nil {
		t.Fatalf("GetDevices: %v", err)
	}
	if string(got) != want {
		t.Fatalf("body = %q, want decompressed %q", string(got), want)
	}
}
