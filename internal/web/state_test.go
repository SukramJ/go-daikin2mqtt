// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"fmt"
	"testing"
	"time"
)

// The store is hard-capped: at capacity the entries closest to expiry are
// evicted, so an unauthenticated login flood cannot grow it without bound.
func TestStateStorePutEvictsAtCap(t *testing.T) {
	s := newStateStore()
	base := time.Unix(1_700_000_000, 0)
	current := base
	s.now = func() time.Time { return current }

	for i := range maxPendingAuth + 10 {
		current = base.Add(time.Duration(i) * time.Second)
		s.put(fmt.Sprintf("state-%d", i), "verifier", "redirect")
		if n := len(s.entries); n > maxPendingAuth {
			t.Fatalf("store grew to %d entries, want <= %d", n, maxPendingAuth)
		}
	}

	// The newest entry survives; the earliest-expiring ones were evicted.
	if _, _, ok := s.take(fmt.Sprintf("state-%d", maxPendingAuth+9)); !ok {
		t.Error("newest state should be retained")
	}
	if _, _, ok := s.take("state-0"); ok {
		t.Error("oldest state should have been evicted at capacity")
	}
}
