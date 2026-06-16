// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"sync"
	"time"
)

// stateTTL is how long a pending OAuth authorization state (and its PKCE
// verifier) is held before it is considered expired. The user has this long
// to complete the consent screen and return to the callback.
const stateTTL = 10 * time.Minute

// pendingAuth holds the per-login data needed to complete the OAuth code
// exchange: the PKCE verifier matching the challenge sent to the IdP, and an
// expiry used for opportunistic cleanup.
type pendingAuth struct {
	verifier  string
	expiresAt time.Time
}

// stateStore keeps pending OAuth states in memory, keyed by the opaque state
// parameter. It is safe for concurrent use. Entries expire after stateTTL
// and are cleaned up opportunistically on access.
type stateStore struct {
	mu      sync.Mutex
	entries map[string]pendingAuth
	now     func() time.Time
}

// newStateStore builds an empty store using the wall clock.
func newStateStore() *stateStore {
	return &stateStore{
		entries: make(map[string]pendingAuth),
		now:     time.Now,
	}
}

// put records the PKCE verifier for a freshly generated state and prunes any
// expired entries while the lock is held.
func (s *stateStore) put(state, verifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.entries[state] = pendingAuth{
		verifier:  verifier,
		expiresAt: s.now().Add(stateTTL),
	}
}

// take returns the verifier for state and removes it (single use). The
// boolean is false when the state is unknown or expired, which the callback
// treats as a CSRF / timeout failure.
func (s *stateStore) take(state string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[state]
	if !ok {
		return "", false
	}
	delete(s.entries, state)
	if s.now().After(e.expiresAt) {
		return "", false
	}
	return e.verifier, true
}

// pruneLocked drops expired entries. Callers must hold s.mu.
func (s *stateStore) pruneLocked() {
	now := s.now()
	for k, v := range s.entries {
		if now.After(v.expiresAt) {
			delete(s.entries, k)
		}
	}
}
