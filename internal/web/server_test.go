// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
)

// --- fakes -------------------------------------------------------------

// fakeClient is a deterministic cloudClient for handler tests.
type fakeClient struct {
	devices    json.RawMessage
	devicesErr error
	patchErr   error
	rl         client.RateLimit

	lastDevice string
	lastChar   string
	lastValue  any
	lastPath   string
}

func (f *fakeClient) GetDevices(_ context.Context) (json.RawMessage, error) {
	return f.devices, f.devicesErr
}

func (f *fakeClient) Patch(_ context.Context, deviceID, _, characteristic string, value any, path string) error {
	f.lastDevice, f.lastChar, f.lastValue, f.lastPath = deviceID, characteristic, value, path
	return f.patchErr
}

func (f *fakeClient) RateLimit() client.RateLimit { return f.rl }

// fakeTokens is a deterministic tokenSource for handler tests.
type fakeTokens struct {
	tokErr error
	saved  *auth.Token
}

func (f *fakeTokens) Token(_ context.Context) (string, error) {
	if f.tokErr != nil {
		return "", f.tokErr
	}
	return "access-token", nil
}

func (f *fakeTokens) SetToken(t *auth.Token) error {
	f.saved = t
	return nil
}

func newTestServer(d Deps) *httptest.Server {
	return httptest.NewServer(New(d).Handler())
}

func baseDeps() Deps {
	return Deps{
		Cfg:    &config.Config{Language: "de", WebBind: "127.0.0.1:8080"},
		Client: &fakeClient{},
		Tokens: &fakeTokens{},
	}
}

// --- tests -------------------------------------------------------------

func TestConfigReportsLanguage(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var cv configView
	if err := json.NewDecoder(res.Body).Decode(&cv); err != nil {
		t.Fatal(err)
	}
	if cv.Language != "de" {
		t.Errorf("language = %q, want de", cv.Language)
	}
	if cv.Web.Language != "de" {
		t.Errorf("web.language = %q, want de", cv.Web.Language)
	}
}

func TestConfigDefaultsLanguageToEnglish(t *testing.T) {
	d := baseDeps()
	d.Cfg = &config.Config{}
	ts := newTestServer(d)
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/config")
	defer func() { _ = res.Body.Close() }()
	var cv configView
	_ = json.NewDecoder(res.Body).Decode(&cv)
	if cv.Language != "en" {
		t.Errorf("language = %q, want en", cv.Language)
	}
}

func TestServesSPA(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", res.StatusCode)
	}
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), "daikin2mqtt") {
		t.Error("index.html not served at /")
	}

	for _, asset := range []string{"/static/app.js", "/static/style.css", "/i18n/en.json", "/i18n/de.json"} {
		r, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatalf("GET %s: %v", asset, err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("asset %s status = %d", asset, r.StatusCode)
		}
	}
}

func TestI18nBundlesAreValidJSON(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	for _, lang := range []string{"en", "de"} {
		res, err := http.Get(ts.URL + "/i18n/" + lang + ".json")
		if err != nil {
			t.Fatalf("GET %s: %v", lang, err)
		}
		var m map[string]string
		err = json.NewDecoder(res.Body).Decode(&m)
		_ = res.Body.Close()
		if err != nil {
			t.Fatalf("%s.json not valid JSON: %v", lang, err)
		}
		if _, ok := m["nav.auth"]; !ok {
			t.Errorf("%s.json missing key nav.auth", lang)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	d := baseDeps()
	d.Cfg = &config.Config{WebUser: "admin", WebPassword: "s3cret"}
	ts := newTestServer(d)
	defer ts.Close()

	// No credentials → 401.
	res, _ := http.Get(ts.URL + "/api/config")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", res.StatusCode)
	}

	// Wrong credentials → 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/config", http.NoBody)
	req.SetBasicAuth("admin", "wrong")
	res2, _ := http.DefaultClient.Do(req)
	_ = res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-auth status = %d, want 401", res2.StatusCode)
	}

	// Correct credentials → 200.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/config", http.NoBody)
	req2.SetBasicAuth("admin", "s3cret")
	res3, _ := http.DefaultClient.Do(req2)
	_ = res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("good-auth status = %d, want 200", res3.StatusCode)
	}
}

func TestAuthStatusAuthenticated(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/auth/status")
	defer func() { _ = res.Body.Close() }()
	var st authStatusView
	_ = json.NewDecoder(res.Body).Decode(&st)
	if !st.Authenticated {
		t.Errorf("authenticated = false, want true")
	}
}

func TestAuthStatusReauthRequired(t *testing.T) {
	d := baseDeps()
	d.Tokens = &fakeTokens{tokErr: auth.ErrReauthRequired}
	ts := newTestServer(d)
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/auth/status")
	defer func() { _ = res.Body.Close() }()
	var st authStatusView
	_ = json.NewDecoder(res.Body).Decode(&st)
	if st.Authenticated {
		t.Errorf("authenticated = true, want false on ErrReauthRequired")
	}
}

func TestAuthLoginRedirectsAndStoresState(t *testing.T) {
	d := baseDeps()
	d.Auth = auth.Config{ClientID: "cid", RedirectURI: "https://example/callback"}
	srv := New(d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login", http.NoBody)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "state=") || !strings.Contains(loc, "code_challenge=") {
		t.Errorf("redirect missing state/challenge: %q", loc)
	}
	if len(srv.states.entries) != 1 {
		t.Errorf("expected one pending state, got %d", len(srv.states.entries))
	}
}

func TestCallbackInvalidStateRejected(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/callback?code=abc&state=bogus")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown state", res.StatusCode)
	}
}

func TestPatchForwardsToClient(t *testing.T) {
	fc := &fakeClient{}
	d := baseDeps()
	d.Client = fc
	ts := newTestServer(d)
	defer ts.Close()

	body := `{"deviceId":"dev1","embeddedId":"climate","characteristic":"onOffMode","value":"on","path":"/p"}`
	res, err := http.Post(ts.URL+"/api/patch", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	if fc.lastDevice != "dev1" || fc.lastChar != "onOffMode" || fc.lastValue != "on" || fc.lastPath != "/p" {
		t.Errorf("client got device=%q char=%q value=%v path=%q",
			fc.lastDevice, fc.lastChar, fc.lastValue, fc.lastPath)
	}
}

func TestPatchMissingFields(t *testing.T) {
	ts := newTestServer(baseDeps())
	defer ts.Close()

	res, _ := http.Post(ts.URL+"/api/patch", "application/json",
		strings.NewReader(`{"value":1}`))
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestDevicesEnrichedWithCatalog(t *testing.T) {
	fc := &fakeClient{
		devices: json.RawMessage(`[
			{"id":"dev1","deviceModel":"Altherma",
			 "managementPoints":[
				{"embeddedId":"climateControl","managementPointType":"climateControl",
				 "onOffMode":{"value":"on","settable":true},
				 "roomTemperature":{"value":21.5}}
			 ]}
		]`),
	}
	d := baseDeps()
	d.Client = fc
	ts := newTestServer(d)
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/devices")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var out []deviceView
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "dev1" {
		t.Fatalf("unexpected devices: %+v", out)
	}
	mp := out[0].ManagementPoints[0]
	if len(mp.Characteristics) != 2 {
		t.Fatalf("characteristics = %d, want 2", len(mp.Characteristics))
	}
	// All characteristics emitted even without a catalog (matched=false).
	for _, c := range mp.Characteristics {
		if c.Matched {
			t.Errorf("characteristic %q matched without a catalog", c.Name)
		}
	}
}

func TestRateLimitEndpoint(t *testing.T) {
	fc := &fakeClient{rl: client.RateLimit{LimitMinute: 200, RemainingMinute: 199}}
	d := baseDeps()
	d.Client = fc
	ts := newTestServer(d)
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/api/ratelimit")
	defer func() { _ = res.Body.Close() }()
	var rl client.RateLimit
	if err := json.NewDecoder(res.Body).Decode(&rl); err != nil {
		t.Fatal(err)
	}
	if rl.LimitMinute != 200 || rl.RemainingMinute != 199 {
		t.Errorf("ratelimit = %+v", rl)
	}
}

func TestEffectiveRedirectURI(t *testing.T) {
	// Explicit config always wins, regardless of request headers.
	d := baseDeps()
	d.Auth = auth.Config{RedirectURI: "https://configured/callback"}
	cfgSrv := New(d)
	req := httptest.NewRequest(http.MethodGet, "http://ha.local/api/auth/login", http.NoBody)
	req.Header.Set("X-Ingress-Path", "/api/hassio_ingress/abc")
	if got := cfgSrv.effectiveRedirectURI(req); got != "https://configured/callback" {
		t.Errorf("configured redirect = %q, want the explicit value", got)
	}

	// Derived cases use an empty-RedirectURI server (baseDeps sets none).
	srv := New(baseDeps())
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		want    string
	}{
		{
			name:    "ingress implies https and prefix",
			host:    "ha.example.com",
			headers: map[string]string{"X-Ingress-Path": "/api/hassio_ingress/abc"},
			want:    "https://ha.example.com/api/hassio_ingress/abc/callback",
		},
		{
			name:    "forwarded proto and host win",
			host:    "internal:8080",
			headers: map[string]string{"X-Forwarded-Proto": "https, http", "X-Forwarded-Host": "proxy.example.com"},
			want:    "https://proxy.example.com/callback",
		},
		{
			name: "plain http fallback",
			host: "localhost:8080",
			want: "http://localhost:8080/callback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/auth/login", http.NoBody)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := srv.effectiveRedirectURI(r); got != tc.want {
				t.Errorf("effectiveRedirectURI = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRootRedirectIngressAware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/callback", http.NoBody)
	if got := rootRedirect(req); got != "./" {
		t.Errorf("rootRedirect without ingress = %q, want ./", got)
	}
	req.Header.Set("X-Ingress-Path", "/api/hassio_ingress/abc")
	if got := rootRedirect(req); got != "/api/hassio_ingress/abc/" {
		t.Errorf("rootRedirect with ingress = %q", got)
	}
}
