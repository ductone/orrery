package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ductone/orrey/internal/catalog"
	"github.com/ductone/orrey/internal/config"
	"github.com/ductone/orrey/internal/model"
	"github.com/ductone/orrey/internal/store"
)

// captureStdout runs f with os.Stdout replaced by a pipe and returns what
// it wrote.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// modelsRuntime opens a store in dir and loads a config from path, the way
// readOnly does for `orrery models`.
func modelsRuntime(t *testing.T, dir, path string) *runtime {
	t.Helper()
	cfg, err := config.LoadUnresolved(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "models.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &runtime{cfg: cfg, configPath: path, store: s}
}

// writeConfig writes a config file and returns its path.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "orrery.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestModelsNoKeysPrintsBuiltins covers the no-provider case: the command
// resolves no secrets, needs no keys, and still prints the built-in
// catalog, sorted by route id, with no disabled section or warnings.
func TestModelsNoKeysPrintsBuiltins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORRERY_HOME", dir)
	rt := modelsRuntime(t, dir, filepath.Join(dir, "missing.yaml"))
	var code int
	out := captureStdout(t, func() { code = listModels(context.Background(), rt, nil) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rows := tableRows(out)
	// A builtin route, with its canonical model, tier, family, and price.
	grok, ok := rows["xai/grok-4.6"]
	if !ok {
		t.Fatalf("no xai/grok-4.6 row:\n%s", out)
	}
	want := []string{"xai/grok-4.6", "grok-4.6", "frontier", "xai", "$2.00", "$6.00", "500K", "builtin"}
	if len(grok) != len(want) {
		t.Fatalf("xai/grok-4.6 = %v, want %v", grok, want)
	}
	for i := range want {
		if grok[i] != want[i] {
			t.Fatalf("xai/grok-4.6 = %v, want %v", grok, want)
		}
	}
	// Sorted by route id.
	ids := tableIDs(out)
	if len(ids) < 2 {
		t.Fatalf("too few rows:\n%s", out)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("rows not sorted by id: %v", ids)
		}
	}
	if strings.Contains(out, "disabled by config") || strings.Contains(out, "warning:") {
		t.Fatalf("unexpected sections:\n%s", out)
	}
}

// tableRows parses a tabwriter table into rows keyed by their first cell.
// tabwriter pads cells with spaces, so cells are split on runs of two or
// more; no cell value contains a space.
func tableRows(out string) map[string][]string {
	rows := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] == "ID" || strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "warning") || strings.HasPrefix(l, "\n") {
			continue
		}
		rows[f[0]] = f
	}
	return rows
}

// tableIDs lists the route ids in table order.
func tableIDs(out string) []string {
	var ids []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] == "ID" || strings.HasPrefix(l, "  ") || !strings.Contains(l, "builtin") && !strings.Contains(l, "discovered") && !strings.Contains(l, "overridden") {
			continue
		}
		ids = append(ids, f[0])
	}
	return ids
}

// TestModelsShowsOverridesDisabledAndWarnings covers the config layers:
// an override marks its route, a disabled entry is listed separately and
// leaves the table, and an entry naming no served model is a warning.
func TestModelsShowsOverridesDisabledAndWarnings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORRERY_HOME", dir)
	path := writeConfig(t, dir, "models:\n"+
		"  - id: xai/grok-4.5\n"+
		"    disabled: true\n"+
		"  - id: grok-4.6\n"+
		"    tier: efficient\n"+
		"  - id: mystery-model\n"+
		"    tier: frontier\n")
	rt := modelsRuntime(t, dir, path)
	var code int
	out := captureStdout(t, func() { code = listModels(context.Background(), rt, nil) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rows := tableRows(out)
	// The disabled route left the table and is listed separately.
	if _, ok := rows["xai/grok-4.5"]; ok {
		t.Fatalf("disabled route still listed:\n%s", out)
	}
	if !strings.Contains(out, "1 disabled by config:\n  xai/grok-4.5") {
		t.Fatalf("no disabled section:\n%s", out)
	}
	// The overridden route is marked and carries the override's tier.
	grok, ok := rows["xai/grok-4.6"]
	if !ok {
		t.Fatalf("no xai/grok-4.6 row:\n%s", out)
	}
	want := []string{"xai/grok-4.6", "grok-4.6", "efficient", "xai", "$2.00", "$6.00", "500K", "overridden"}
	if len(grok) != len(want) {
		t.Fatalf("xai/grok-4.6 = %v, want %v", grok, want)
	}
	for i := range want {
		if grok[i] != want[i] {
			t.Fatalf("xai/grok-4.6 = %v, want %v", grok, want)
		}
	}
	// The entry naming no served model is a warning, not a row.
	if !strings.Contains(out, "warning: models: no route serves mystery-model") {
		t.Fatalf("no warning:\n%s", out)
	}
	if _, ok := rows["mystery-model"]; ok {
		t.Fatalf("warning became a row:\n%s", out)
	}
}

// TestModelsUsesDiscoveryCacheWhenListingFails covers a config with a
// provider key but no reachable listing: discovery fails fast against a
// closed local port, falls back to the on-disk cache, and the cached
// models appear as discovered rows. No real network is used.
func TestModelsUsesDiscoveryCacheWhenListingFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORRERY_HOME", dir)
	// Seed the discovery cache with one non-curated model.
	cached := catalog.Discovery{
		Provider: "ramp",
		Models: []model.ModelSpec{{
			ID: "ramp/testlist-model", Family: model.DeepSeek, Tier: model.Efficient,
			ContextWindow: 128000, MaxOutput: 32768,
			Pricing: model.Pricing{Input: 0.3, Output: 1.2}, Discovered: true,
		}},
	}
	b, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog", "catalog-ramp.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// A provider whose listing endpoint is a closed local port.
	path := writeConfig(t, dir, "providers:\n"+
		"  ramp:\n"+
		"    api_key: test-key\n"+
		"    base_url: 'http://127.0.0.1:1'\n")
	rt := modelsRuntime(t, dir, path)
	var code int
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = listModels(context.Background(), rt, nil) })
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	rows := tableRows(out)
	row, ok := rows["ramp/testlist-model"]
	if !ok {
		t.Fatalf("no discovered row:\n%s", out)
	}
	// id, model (dash: not curated), tier, family, prices, ctx, source.
	want := []string{"ramp/testlist-model", "-", "efficient", "deepseek", "$0.30", "$1.20", "128K", "discovered"}
	if len(row) != len(want) {
		t.Fatalf("ramp/testlist-model = %v, want %v", row, want)
	}
	for i := range want {
		if row[i] != want[i] {
			t.Fatalf("ramp/testlist-model = %v, want %v", row, want)
		}
	}
	if !strings.Contains(stderr, "models: discovery ramp:") {
		t.Fatalf("no discovery failure note: %q", stderr)
	}
}

func TestSourceFor(t *testing.T) {
	m := model.ModelSpec{Discovered: true}
	if got := sourceFor(m, false); got != "discovered" {
		t.Errorf("discovered = %q", got)
	}
	if got := sourceFor(m, true); got != "discovered,overridden" {
		t.Errorf("discovered+overridden = %q", got)
	}
	if got := sourceFor(model.ModelSpec{}, false); got != "builtin" {
		t.Errorf("builtin = %q", got)
	}
}

func TestModelsFormatting(t *testing.T) {
	if got := shortCtx(1_048_576); got != "1.0M" {
		t.Errorf("shortCtx(1048576) = %q", got)
	}
	if got := shortCtx(256_000); got != "256K" {
		t.Errorf("shortCtx(256000) = %q", got)
	}
	if got := usdPerM(0); got != "-" {
		t.Errorf("usdPerM(0) = %q", got)
	}
	if got := usdPerM(3.756); got != "$3.76" {
		t.Errorf("usdPerM(3.756) = %q", got)
	}
	if got := dash(""); got != "-" {
		t.Errorf("dash(\"\") = %q", got)
	}
	if got := seconds(2.34); got != "2.3s" {
		t.Errorf("seconds(2.34) = %q", got)
	}
	if got := seconds(0); got != "-" {
		t.Errorf("seconds(0) = %q", got)
	}
	if got := tokps(47.6); got != "48" {
		t.Errorf("tokps(47.6) = %q", got)
	}
	if got := pct(1, 0); got != "-" {
		t.Errorf("pct(1,0) = %q", got)
	}
	if got := pct(1, 3); got != "33" {
		t.Errorf("pct(1,3) = %q", got)
	}
	if got := pct(2, 3); got != "67" {
		t.Errorf("pct(2,3) = %q", got)
	}
	if got := when(time.Time{}); got != "-" {
		t.Errorf("when(zero) = %q", got)
	}
}

// TestDisabledAndOverriddenIDs covers the id matching the config layers
// use: by route id, by canonical model without a provider prefix, and the
// deduplication of a disabled entry naming several routes.
func TestDisabledAndOverriddenIDs(t *testing.T) {
	models := []model.ModelSpec{
		{ID: "xai/grok-4.6", Model: "grok-4.6"},
		{ID: "ramp/grok-4.6", Model: "grok-4.6", Discovered: true},
		{ID: "ramp/other-model"},
	}
	overridden := overriddenIDs([]config.ModelConfig{{ID: "grok-4.6"}}, models)
	if len(overridden) != 2 || !overridden["xai/grok-4.6"] || !overridden["ramp/grok-4.6"] {
		t.Fatalf("overriddenIDs = %v", overridden)
	}
	res := catalog.Result{Models: models, Discoveries: []catalog.Discovery{{Models: models}}}
	disabled := disabledIDs([]config.ModelConfig{{ID: "grok-4.6", Disabled: ptr(true)}}, res)
	if len(disabled) != 2 || disabled[0] != "xai/grok-4.6" || disabled[1] != "ramp/grok-4.6" {
		t.Fatalf("disabledIDs = %v", disabled)
	}
}

func ptr(b bool) *bool { return &b }

func TestModelsReadOnlyWithoutProviderKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORRERY_HOME", dir)
	path := writeConfig(t, dir, "database: stats.db\nproviders:\n  openai:\n    api_key: !env ORRERY_TEST_MISSING_MODEL_KEY\n")
	t.Setenv("ORRERY_TEST_MISSING_MODEL_KEY", "")
	var code int
	out := captureStdout(t, func() { code = readOnly(context.Background(), "models", path, []string{"--stats"}) })
	if code != 0 || !strings.Contains(out, "xai/grok-4.6") || !strings.Contains(out, "CALLS") {
		t.Fatalf("exit=%d output=%s", code, out)
	}
}

func TestModelsStatsColumns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORRERY_HOME", dir)
	rt := modelsRuntime(t, dir, filepath.Join(dir, "missing.yaml"))
	ctx := context.Background()
	if err := rt.store.RecordModelCall(ctx, "xai/grok-4.6", 200*time.Second, 1000, true); err != nil {
		t.Fatal(err)
	}
	if err := rt.store.RecordModelFailure(ctx, "xai/grok-4.6", "empty"); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { code = listModels(ctx, rt, []string{"--stats"}) })
	row := tableRows(out)["xai/grok-4.6"]
	if code != 0 || len(row) != 14 || strings.Join(row[8:13], " ") != "1 200.0s 5 100 1" || row[13] == "-" {
		t.Fatalf("exit=%d row=%v output=%s", code, row, out)
	}
}
