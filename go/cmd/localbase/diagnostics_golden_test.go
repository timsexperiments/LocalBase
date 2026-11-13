package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The bundle as the command makes it, recorded before its collection moved to internal/diagnostics
// (5-7b), on a root with a voice, a catalog file that is skipped and a model's memory peak: the
// manifest, how many log lines, and what the command prints. What depends on the machine or the
// moment is masked: the time, the version, the platform and hardware, the service, the memory verdict
// and the system memory it was judged against, and the log window's times and size.
// LOCALBASE_UPDATE_DIAGNOSTICS_GOLDEN=1 rewrites testdata/diagnostics.golden.
func TestDiagnosticsGolden(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	for _, k := range slices.Concat(obs.OtelEnvironmentNames, []string{"LOCALBASE_CATALOG_FEED_URL", "LOCALBASE_TEST_CATALOG_D"}) {
		t.Setenv(k, "")
	}
	roomyHost(t) // the defaults depend on the memory: these are 64 GiB's
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	if err := os.MkdirAll(filepath.Join(root, "catalog.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "catalog.d", "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
		VALUES ('voice_a', 'Name a', 'name a', ?, 1000, 5000, '2026-01-01T00:00:00.000Z', 'cli', 'I consent.', '2026-01-01T00:00:00.000Z')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	// A peak seen for the active LLM, at the shape it plans.
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadRoot(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := cat.ByID(cfg.Active["llm"])
	if spec == nil || spec.Primary().SHA256 == "" {
		t.Fatalf("active LLM %q has no artifact hash", cfg.Active["llm"])
	}
	if err := st.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: spec.ModelID, ArtifactSHA256: spec.Primary().SHA256,
		LaunchShape: runtime.FitFor(spec, 0, nil, nil, cat.PlannableFeedPeaks(spec)).LaunchShape, PeakBytes: 3 << 30,
		Method: "footprint", ObservedAt: "2026-01-01T00:00:00.000Z"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	bundle := filepath.Join(t.TempDir(), "bundle.zip")
	var out, errb bytes.Buffer
	a := app()
	a.Stdout, a.Stderr = &out, &errb
	if code := a.Run([]string{"--root", root, "diagnostics", "--output", bundle}); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	files := map[string][]byte{}
	var names []string
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = b
		names = append(names, f.Name)
	}
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	const masked = "<masked>"
	for _, k := range []string{"generatedAt", "version", "platform", "hardware", "service"} {
		if _, ok := manifest[k]; ok {
			manifest[k] = masked
		}
	}
	if logs, ok := manifest["logs"].(map[string]any); ok {
		if data, ok := logs["data"].(map[string]any); ok {
			data["window"], data["bytes"] = masked, masked
		}
	}
	if models, ok := manifest["models"].(map[string]any); ok {
		if data, ok := models["data"].(map[string]any); ok {
			list, _ := data["models"].([]any)
			for _, m := range list {
				mem := m.(map[string]any)["memory"].(map[string]any)
				mem["verdict"], mem["systemMemoryGb"] = masked, masked
			}
		}
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	logLines := strings.Count(string(files["logs/events.jsonl"]), "\n")
	text := out.String() + "--- stderr\n" + errb.String()
	text = strings.ReplaceAll(text, bundle, "<bundle>")
	text = regexp.MustCompile(`(Memory for [^:]+: [0-9.]+ GB \([^)]*\)), .*`).ReplaceAllString(text, "$1, <masked>")
	got := "entries: " + strings.Join(names, ", ") + "\nlog lines: " + itoa(logLines) + "\n--- manifest.json\n" + string(b) + "\n--- stdout\n" + text
	golden := filepath.Join("testdata", "diagnostics.golden")
	if os.Getenv("LOCALBASE_UPDATE_DIAGNOSTICS_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the bundle differs from %s:\n%s", golden, got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
