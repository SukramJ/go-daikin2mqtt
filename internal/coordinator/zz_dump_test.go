package coordinator

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDumpSurface is scaffolding, not a pin: it writes every scenario's
// BUILDER OUTPUT to $DAIKIN_SURFACE_DUMP so the same harness can be run at two
// revisions and the outputs compared key by key. It reads no testdata.
func TestDumpSurface(t *testing.T) {
	path := os.Getenv("DAIKIN_SURFACE_DUMP")
	if path == "" {
		t.Skip("set DAIKIN_SURFACE_DUMP")
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
