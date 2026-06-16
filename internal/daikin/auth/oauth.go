// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package auth implements the OAuth2 Authorization Code flow (with PKCE)
// against the Daikin ONECTA identity provider, plus refresh-token
// persistence and a [TokenSource] that keeps a valid access token
// available to the rest of the daemon.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Scopes requests basic device integration plus offline_access, which is
// what yields a refresh token for unattended daemon operation.
const Scopes = "openid onecta:basic.integration offline_access"

// ONECTA OAuth2 endpoints. They are package variables (not constants) so
// tests can point them at a local httptest server.
var (
	// AuthorizeURL is the authorization endpoint the user's browser is sent to.
	AuthorizeURL = "https://idp.onecta.daikineurope.com/v1/oidc/authorize"
	// TokenURL is the token endpoint used for code exchange and refresh.
	TokenURL = "https://idp.onecta.daikineurope.com/v1/oidc/token" //nolint:gosec // public OAuth endpoint URL, not a credential
)

// ErrReauthRequired indicates the refresh token is no longer valid
// (e.g. revoked or expired); the user must run the authorization flow
// again. Callers should surface this clearly rather than crash-loop.
var ErrReauthRequired = errors.New("auth: re-authentication required")

// Config carries the OAuth2 client credentials and redirect URI.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// Token is a stored OAuth2 token set. ExpiresAt is an absolute time so a
// persisted token can be evaluated for freshness after a restart.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid reports whether the access token is non-empty and not expired at
// time now (callers typically pass a skew-adjusted now).
func (t *Token) Valid(now time.Time) bool {
	return t != nil && t.AccessToken != "" && now.Before(t.ExpiresAt)
}

// tokenResponse is the raw JSON returned by the token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// PKCE holds a generated code verifier and its derived S256 challenge.
type PKCE struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE creates a high-entropy code verifier and its S256
// challenge per RFC 7636.
func GeneratePKCE() (PKCE, error) {
	v, err := randomURLSafe(32)
	if err != nil {
		return PKCE{}, err
	}
	sum := sha256.Sum256([]byte(v))
	return PKCE{
		Verifier:  v,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// GenerateState returns a random opaque state parameter for CSRF
// protection of the redirect.
func GenerateState() (string, error) {
	return randomURLSafe(24)
}

// randomURLSafe returns n random bytes encoded as base64url (no padding).
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthorizeURL builds the full authorization URL the user must open in a
// browser. state and challenge come from [GenerateState] / [GeneratePKCE].
func (c Config) AuthorizeURL(state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("scope", Scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return AuthorizeURL + "?" + q.Encode()
}

// ExchangeCode trades an authorization code (plus the PKCE verifier) for a
// token set.
func (c Config) ExchangeCode(ctx context.Context, hc *http.Client, code, verifier string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURI)
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("code_verifier", verifier)
	return c.postToken(ctx, hc, form, "")
}

// Refresh exchanges a refresh token for a fresh token set. If the provider
// does not rotate the refresh token, the previous one is carried over.
func (c Config) Refresh(ctx context.Context, hc *http.Client, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	return c.postToken(ctx, hc, form, refreshToken)
}

// postToken performs the token-endpoint POST and maps the response. The
// prevRefresh fallback is used when the provider omits a rotated refresh
// token (common on refresh_token grants).
func (c Config) postToken(ctx context.Context, hc *http.Client, form url.Values, prevRefresh string) (*Token, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read token response: %w", err)
	}

	var tr tokenResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &tr); err != nil {
			return nil, fmt.Errorf("auth: decode token response (status %d): %w", resp.StatusCode, err)
		}
	}

	if resp.StatusCode != http.StatusOK {
		// invalid_grant on a refresh means the refresh token is dead.
		if tr.Error == "invalid_grant" {
			return nil, fmt.Errorf("%w: %s", ErrReauthRequired, tr.ErrorDesc)
		}
		if tr.Error != "" {
			return nil, fmt.Errorf("auth: token endpoint %d: %s: %s", resp.StatusCode, tr.Error, tr.ErrorDesc)
		}
		return nil, fmt.Errorf("auth: token endpoint returned status %d", resp.StatusCode)
	}

	refresh := tr.RefreshToken
	if refresh == "" {
		refresh = prevRefresh
	}
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: refresh,
		TokenType:    tr.TokenType,
		ExpiresAt:    nowFunc().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// nowFunc is overridable in tests; production uses time.Now.
var nowFunc = time.Now
