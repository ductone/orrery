package core

import (
	"os"
	"testing"
)

// TestMain points the benchmark/temp-workspace memory skip at a directory no
// fixture uses, because test workspaces come from t.TempDir().
func TestMain(m *testing.M) {
	memoryTempDir = func() string { return "/nonexistent-orrery-memory-temp" }
	os.Exit(m.Run())
}
