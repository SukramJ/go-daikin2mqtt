// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// refreshSkew is how long before the real expiry a token is treated as
// expired, so a refresh happens before requests start failing.
const refreshSkew = 60 * time.Second

// TokenSource hands out a valid access token, refreshing proactively and
// persisting rotated refresh tokens. It is safe for concurrent use.
type TokenSource struct {
	cfg   Config
	store *Store
	hc    *http.Client
	clock func() time.Time

	mu      sync.Mutex
	current *Token
}

// NewTokenSource builds a TokenSource. hc may be nil (a client with a 60s
// timeout — the refresh runs under ts.mu, so it must never hang forever).
// The initial token is loaded lazily on the first [TokenSource.Token] call
// if not already present.
func NewTokenSource(cfg Config, store *Store, hc *http.Client) *TokenSource {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &TokenSource{cfg: cfg, store: store, hc: hc, clock: time.Now}
}

// Invalidate forces the next [TokenSource.Token] call to refresh, even if
// the cached token has not expired yet. Callers use this when the cloud
// rejects a token with HTTP 401 (Daikin invalidates older access tokens
// when the refresh token rotates).
func (ts *TokenSource) Invalidate() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.current == nil {
		if t, err := ts.store.Load(); err == nil {
			ts.current = t
		}
	}
	if ts.current != nil {
		// Mark expired so Token() refreshes via the refresh token.
		ts.current.ExpiresAt = ts.clock().Add(-time.Hour)
	}
}

// SetToken seeds the in-memory token (e.g. right after the interactive
// authorization flow) and persists it.
func (ts *TokenSource) SetToken(t *Token) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.current = t
	return ts.store.Save(t)
}

// Token returns a currently valid access token, refreshing if necessary.
// It returns [ErrReauthRequired] when the refresh token is no longer
// accepted by the provider.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.current == nil {
		t, err := ts.store.Load()
		if err != nil {
			if errors.Is(err, ErrNoToken) {
				return "", errors.Join(ErrReauthRequired, err)
			}
			return "", err
		}
		ts.current = t
	}

	now := ts.clock().Add(refreshSkew)
	if ts.current.Valid(now) {
		return ts.current.AccessToken, nil
	}

	if ts.current.RefreshToken == "" {
		return "", fmt.Errorf("%w: no refresh token", ErrReauthRequired)
	}

	refreshed, err := ts.cfg.Refresh(ctx, ts.hc, ts.current.RefreshToken)
	if err != nil {
		return "", err // already wraps ErrReauthRequired on invalid_grant
	}
	ts.current = refreshed
	if err := ts.store.Save(refreshed); err != nil {
		// A persistence failure is non-fatal for this request — the token
		// works in memory — but the caller should know.
		return refreshed.AccessToken, fmt.Errorf("auth: token refreshed but not persisted: %w", err)
	}
	return refreshed.AccessToken, nil
}
