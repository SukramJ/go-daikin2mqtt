// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{ClientID: "cid", ClientSecret: "csecret", RedirectURI: "http://127.0.0.1:0/callback"}
}

func TestGeneratePKCE(t *testing.T) {
	a, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GeneratePKCE()
	if a.Verifier == "" || a.Challenge == "" {
		t.Fatal("empty PKCE fields")
	}
	if a.Verifier == b.Verifier {
		t.Fatal("PKCE verifiers should differ")
	}
	if strings.ContainsAny(a.Challenge, "+/=") {
		t.Errorf("challenge must be base64url without padding: %q", a.Challenge)
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := testConfig()
	raw := c.AuthorizeURL("the-state", "the-challenge")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "cid",
		"state":                 "the-state",
		"code_challenge":        "the-challenge",
		"code_challenge_method": "S256",
		"scope":                 Scopes,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
}

// tokenServer spins up a fake token endpoint and points TokenURL at it for
// the duration of the test.
func tokenServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := TokenURL
	TokenURL = srv.URL
	t.Cleanup(func() { TokenURL = prev })
}

func TestExchangeCode(t *testing.T) {
	tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "auth-code" {
			t.Errorf("unexpected form: %v", r.Form)
		}
		if r.Form.Get("code_verifier") != "verif" {
			t.Errorf("missing code_verifier")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
	})

	tok, err := testConfig().ExchangeCode(context.Background(), nil, "auth-code", "verif")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Errorf("unexpected token %+v", tok)
	}
	if !tok.Valid(time.Now()) {
		t.Error("token should be valid immediately after exchange")
	}
}

func TestRefreshKeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No refresh_token in the response -> previous one must be kept.
		_, _ = w.Write([]byte(`{"access_token":"at2","token_type":"Bearer","expires_in":3600}`))
	})
	tok, err := testConfig().Refresh(context.Background(), nil, "old-rt")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "old-rt" {
		t.Errorf("refresh token = %q, want carried-over old-rt", tok.RefreshToken)
	}
}

func TestRefreshInvalidGrantMapsToReauth(t *testing.T) {
	tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
	})
	_, err := testConfig().Refresh(context.Background(), nil, "dead")
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
}

func TestStoreRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token.json")
	st := NewStore(path)

	if _, err := st.Load(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Load on missing = %v, want ErrNoToken", err)
	}

	want := &Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Round(time.Second)}
	if err := st.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round trip mismatch: %+v vs %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token store perms = %o, want 600", perm)
	}
}

func TestTokenSourceRefreshesExpired(t *testing.T) {
	tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"rt2","token_type":"Bearer","expires_in":3600}`))
	})
	st := NewStore(filepath.Join(t.TempDir(), "token.json"))
	ts := NewTokenSource(testConfig(), st, nil)
	// Seed an already-expired token.
	if err := ts.SetToken(&Token{AccessToken: "stale", RefreshToken: "rt1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	at, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if at != "fresh" {
		t.Errorf("access token = %q, want fresh", at)
	}
	// Rotated refresh token must be persisted.
	persisted, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RefreshToken != "rt2" {
		t.Errorf("persisted refresh = %q, want rt2", persisted.RefreshToken)
	}
}

func TestTokenSourceNoStoreNeedsReauth(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "missing.json"))
	ts := NewTokenSource(testConfig(), st, nil)
	if _, err := ts.Token(context.Background()); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
}

func TestAuthorizeFullFlow(t *testing.T) {
	tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
	})

	cfg := Config{ClientID: "cid", ClientSecret: "cs", RedirectURI: "http://127.0.0.1:45917/callback"}

	// The "browser" hits the callback URL once the server is up.
	open := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		state := u.Query().Get("state")
		go func() {
			cb := "http://127.0.0.1:45917/callback?code=the-code&state=" + state
			// Retry briefly until the listener accepts.
			for range 50 {
				resp, err := http.Get(cb) //nolint:noctx // test
				if err == nil {
					_ = resp.Body.Close()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := cfg.Authorize(ctx, nil, "127.0.0.1:45917", open)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" {
		t.Errorf("access token = %q, want at", tok.AccessToken)
	}
}
