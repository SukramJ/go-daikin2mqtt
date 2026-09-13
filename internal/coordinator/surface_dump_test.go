// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDumpSurface is scaffolding, not a pin.
//
// It writes every scenario's BUILDER OUTPUT to the file named by
// $DAIKIN_SURFACE_DUMP and skips when that is unset, so the same harness can be
// checked out at two revisions and the two dumps compared key by key:
//
//	git worktree add /tmp/base origin/main
//	cp internal/coordinator/surface_dump_test.go /tmp/base/internal/coordinator/
//	(cd /tmp/base && DAIKIN_SURFACE_DUMP=/tmp/base.json go test ./internal/coordinator -run TestDumpSurface)
//	DAIKIN_SURFACE_DUMP=/tmp/head.json go test ./internal/coordinator -run TestDumpSurface
//	# then diff /tmp/base.json against /tmp/head.json, key by key
//
// This is how every "what moved" table in ADR 0070 phase 8 is derived. It reads
// no testdata, deliberately: the committed fixtures are produced by the very
// code under examination, so a diff of THEM cannot distinguish "the builders
// changed" from "the fixtures were regenerated".
func TestDumpSurface(t *testing.T) {
	path := os.Getenv("DAIKIN_SURFACE_DUMP")
	if path == "" {
		t.Skip("set DAIKIN_SURFACE_DUMP to a file path to dump the built surface")
	}
	out := map[string][]recordedMsg{}
	for _, sc := range surfaceScenarios() {
		out[sc.name] = buildSurface(t, sc)
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
