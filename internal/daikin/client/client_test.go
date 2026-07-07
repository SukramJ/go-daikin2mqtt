// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubTokens is a TokenProvider returning a fixed token, or an error.
type stubTokens struct {
	token string
	err   error
}

func (s stubTokens) Token(_ context.Context) (string, error) {
	return s.token, s.err
}

// fakeClock provides a controllable monotonic-ish time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// withFakeSleep replaces the package sleep var for the duration of a test.
func withFakeSleep(t *testing.T) {
	t.Helper()
	prev := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = prev })
}

func TestGetDevicesOKAndRateLimitHeaders(t *testing.T) {
	const wantBody = `[{"id":"dev-1"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/gateway-devices" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding = %q", got)
		}
		w.Header().Set("X-RateLimit-Limit-minute", "60")
		w.Header().Set("X-RateLimit-Remaining-minute", "59")
		w.Header().Set("X-RateLimit-Limit-day", "1000")
		w.Header().Set("X-RateLimit-Remaining-day", "900")
		w.Header().Set("ratelimit-reset", "30")
		_, _ = io.WriteString(w, wantBody)
	}))
	defer srv.Close()

	clk := newFakeClock()
	c := New(Options{BaseURL: srv.URL, Tokens: stubTokens{token: "tok"}, Clock: clk.now})

	body, err := c.GetDevices(context.Background())
	if err != nil {
		t.Fatalf("GetDevices: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}

	rl := c.RateLimit()
	if rl.LimitMinute != 60 || rl.RemainingMinute != 59 || rl.LimitDay != 1000 || rl.RemainingDay != 900 {
		t.Errorf("rate limit = %+v", rl)
	}
	if want := clk.now().Add(30 * time.Second); !rl.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", rl.ResetAt, want)
	}
	if rl.Updated.IsZero() {
		t.Error("Updated not set")
	}
}

func TestGetDevicesRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Tokens: stubTokens{token: "tok"}})

	_, err := c.GetDevices(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if rl := c.RateLimit(); rl.RetryAfter != 42 {
		t.Errorf("RetryAfter = %d, want 42", rl.RetryAfter)
	}
}

func TestScanIgnore(t *testing.T) {
	var patchCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			atomic.AddInt32(&patchCalls, 1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()

	clk := newFakeClock()
	c := New(Options{
		BaseURL:    srv.URL,
		Tokens:     stubTokens{token: "tok"},
		Clock:      clk.now,
		ScanIgnore: 30 * time.Second,
	})

	if err := c.Patch(context.Background(), "dev", "emb", "onOffMode", "on", ""); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	// Within window: GET must be blocked.
	if _, err := c.GetDevices(context.Background()); !errors.Is(err, ErrScanIgnore) {
		t.Fatalf("GetDevices within window err = %v, want ErrScanIgnore", err)
	}

	// Still within window at boundary - 1ns.
	clk.advance(30*time.Second - time.Nanosecond)
	if _, err := c.GetDevices(context.Background()); !errors.Is(err, ErrScanIgnore) {
		t.Fatalf("GetDevices at boundary err = %v, want ErrScanIgnore", err)
	}

	// Past window: GET allowed.
	clk.advance(2 * time.Nanosecond)
	if _, err := c.GetDevices(context.Background()); err != nil {
		t.Fatalf("GetDevices after window: %v", err)
	}
}

func TestPatchSendsCorrectPathAndBody(t *testing.T) {
	tests := []struct {
		name           string
		value          any
		path           string
		wantBodyFields map[string]any
	}{
		{
			name:           "no path",
			value:          "cooling",
			path:           "",
			wantBodyFields: map[string]any{"value": "cooling"},
		},
		{
			name:           "with path",
			value:          21.5,
			path:           "/temperatureControl/operationModes/cooling/setpoints/roomTemperature",
			wantBodyFields: map[string]any{"value": 21.5, "path": "/temperatureControl/operationModes/cooling/setpoints/roomTemperature"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
				}
				b, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(b, &gotBody); err != nil {
					t.Errorf("unmarshal body: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			c := New(Options{BaseURL: srv.URL, Tokens: stubTokens{token: "tok"}})
			err := c.Patch(context.Background(), "DEV1", "climateControl", "temperatureControl", tc.value, tc.path)
			if err != nil {
				t.Fatalf("Patch: %v", err)
			}

			wantPath := "/v1/gateway-devices/DEV1/management-points/climateControl/characteristics/temperatureControl"
			if gotPath != wantPath {
				t.Errorf("path = %q, want %q", gotPath, wantPath)
			}
			if len(gotBody) != len(tc.wantBodyFields) {
				t.Errorf("body = %v, want fields %v", gotBody, tc.wantBodyFields)
			}
			for k, want := range tc.wantBodyFields {
				if got := gotBody[k]; got != want {
					t.Errorf("body[%q] = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// ID segments come from MQTT topics / the web API; they must be path-escaped
// so they cannot inject path segments or query/fragment parts into the URL.
func TestPatchEscapesPathSegments(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Tokens: stubTokens{token: "tok"}})
	if err := c.Patch(context.Background(), "dev/../../admin", "emb?x=1", "char#frag", "on", ""); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	want := "/v1/gateway-devices/dev%2F..%2F..%2Fadmin/management-points/emb%3Fx=1/characteristics/char%23frag"
	if gotURI != want {
		t.Errorf("request URI = %q, want %q", gotURI, want)
	}
}

func TestGetDevicesRetryThenSuccess(t *testing.T) {
	withFakeSleep(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `[{"ok":true}]`)
	}))
	defer srv.Close()

	c := New(Options{
		BaseURL:        srv.URL,
		Tokens:         stubTokens{token: "tok"},
		RetryBaseDelay: time.Nanosecond,
		RetryAttempts:  3,
	})

	body, err := c.GetDevices(context.Background())
	if err != nil {
		t.Fatalf("GetDevices: %v", err)
	}
	if string(body) != `[{"ok":true}]` {
		t.Errorf("body = %q", body)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want 2", got)
	}
}

func TestGetDevicesRetryExhausted(t *testing.T) {
	withFakeSleep(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(Options{
		BaseURL:        srv.URL,
		Tokens:         stubTokens{token: "tok"},
		RetryBaseDelay: time.Nanosecond,
		RetryAttempts:  3,
	})

	if _, err := c.GetDevices(context.Background()); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func TestCircuitBreakerOpensAfterFiveFailures(t *testing.T) {
	withFakeSleep(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	clk := newFakeClock()
	c := New(Options{
		BaseURL:        srv.URL,
		Tokens:         stubTokens{token: "tok"},
		Clock:          clk.now,
		RetryBaseDelay: time.Nanosecond,
		RetryAttempts:  1, // one failure per GetDevices call
	})

	// 5 failing GETs trip the breaker.
	for i := range cbFailureThreshold {
		if _, err := c.GetDevices(context.Background()); err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}

	// Next call should be short-circuited.
	if _, err := c.GetDevices(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}

	// After recovery timeout, breaker goes half-open and tries again.
	clk.advance(cbRecoveryTimeout)
	if _, err := c.GetDevices(context.Background()); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("breaker should have transitioned to half-open and attempted request")
	}
}

func TestCircuitBreakerIgnores429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Tokens: stubTokens{token: "tok"}})

	// Many 429s must not trip the breaker.
	for i := range cbFailureThreshold + 2 {
		if _, err := c.GetDevices(context.Background()); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("call %d err = %v, want ErrRateLimited", i, err)
		}
	}
}

func TestTokenErrorPropagated(t *testing.T) {
	sentinel := errors.New("auth: re-authentication required")
	c := New(Options{BaseURL: "http://example.invalid", Tokens: stubTokens{err: sentinel}})

	if _, err := c.GetDevices(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("GetDevices token err = %v, want %v", err, sentinel)
	}
	if err := c.Patch(context.Background(), "d", "e", "ch", 1, ""); !errors.Is(err, sentinel) {
		t.Errorf("Patch token err = %v, want %v", err, sentinel)
	}
}

func TestDefaultsApplied(t *testing.T) {
	c := New(Options{Tokens: stubTokens{token: "tok"}})
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q", c.baseURL)
	}
	if c.scanIgnore != 30*time.Second {
		t.Errorf("scanIgnore = %v", c.scanIgnore)
	}
	if c.retryBase != time.Second {
		t.Errorf("retryBase = %v", c.retryBase)
	}
	if c.retryMax != 3 {
		t.Errorf("retryMax = %d", c.retryMax)
	}
	if c.clock == nil || c.httpClient == nil || c.logger == nil {
		t.Error("defaults not fully applied")
	}
}
