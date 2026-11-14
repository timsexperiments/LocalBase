package system

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// Planted in the logs, catalog files, the feed URL, the OTLP headers and the
// service stderr capture: none may reach a bundle.
var diagnosticsSecrets = []string{"hf_plantedTokenValue123", "lb_plantedLocalBaseKey1234567890abcd", "sekritBearerValue42",
	"feedUserPassword", "feedSignature99", "otlpHeaderSecret77", "stderrCrashSecret", "secretPromptText",
	"authProfileHeaderSecret31", "signedUrlSignature62", "VoiceNameSecretAda", "goSettingSecret48", "runOutputSecret55"}

func diagnosticsLog() string {
	ev := func(i int, extra map[string]any) string {
		e := map[string]any{"schemaVersion": 2, "id": fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			"timestamp": fmt.Sprintf("2026-10-20T10:00:%02d.000Z", i), "severity": "info", "eventName": "gateway.request",
			"category": "http", "component": "gateway", "runtime": "gateway", "message": "Request served."}
		for k, v := range extra {
			e[k] = v
		}
		b, _ := json.Marshal(e)
		return string(b)
	}
	lines := []string{
		ev(1, nil),
		ev(2, map[string]any{"message": "Upstream said Authorization: Bearer sekritBearerValue42 and token=hf_plantedTokenValue123",
			"requestId": "req-2", "http": map[string]any{"method": "POST", "path": "/v1/chat/completions", "status": 200, "durationMs": 12.5}}),
		ev(3, map[string]any{"severity": "error", "message": "Backend failed.",
			"error": map[string]any{"type": "Error", "message": "spawn failed with key lb_plantedLocalBaseKey1234567890abcd", "code": "E1"},
			"attributes": map[string]any{"model_id": "qwen", "api_key": "lb_plantedLocalBaseKey1234567890abcd", "prompt_tokens": 12,
				"note": "https://user:feedUserPassword@host/x?token=feedSignature99", "Prompt": "secretPromptText", "ok": true}}),
		ev(4, map[string]any{"message": `{"messages": "secretPromptText"}`}),
		ev(5, map[string]any{"eventName": "token.refresh-hf_plantedTokenValue123", "component": "auth"}),
		ev(6, map[string]any{"trace": map[string]any{"traceId": strings.Repeat("ab", 16), "spanId": strings.Repeat("cd", 8)}}),
		`{"not":"an event"}`,
		"garbage line",
		ev(7, map[string]any{"severity": "warn", "runtime": "llm", "component": "llama-server", "message": "Slow start."}),
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestCLIDiagnostics makes a bundle of a root with logs, catalog patches and problems, a feed
// setting, OTLP environment and captured stderr: its manifest and redacted log are pinned (they
// were compared with TS's until step 7), and no planted secret reaches the archive.
func TestCLIDiagnostics(t *testing.T) {
	pinned(t)
	harness.PinValues(t)
	gr := cliRoot(t)
	initRoot(t, gr)
	r := gr
	fsguard.MkdirAll(filepath.Join(r, "logs"), 0o700)
	fsguard.WriteFile(filepath.Join(r, "logs", "events.jsonl"), []byte(diagnosticsLog()), 0o600)
	fsguard.WriteFile(filepath.Join(r, "logs", "service-stderr.log"), []byte("panic: stderrCrashSecret\n"), 0o600)
	fsguard.MkdirAll(filepath.Join(r, "catalog.d"), 0o700)
	fsguard.WriteFile(filepath.Join(r, "catalog.d", "patch.json"), []byte(`{"modelId": "whisper-base-q8_0", "notes": "Patched.",
		"repositoryRevision": "`+strings.Repeat("e", 40)+`"}`), 0o600)
	fsguard.WriteFile(filepath.Join(r, "catalog.d", "broken.json"), []byte(`{"modelId": "qwen3.5-4b-q4_k_m", "storageGb": "big"}`), 0o600)
	plantPrivateState(t, r)
	env := []string{"LOCALBASE_CATALOG_FEED_URL=https://user:feedUserPassword@feed.example.com/feed.json?sig=feedSignature99#frag",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318/base/", "OTEL_EXPORTER_OTLP_HEADERS=x-auth=otlpHeaderSecret77"}
	out := fsguard.ScratchDir(t)
	bundle := func(root, name string) (cliResult, map[string][]byte) {
		path := filepath.Join(out, name)
		res := runCLIEnv(t, "go", root, env, "--json", "diagnostics", "--output", path)
		if res.code != 0 {
			t.Fatalf("diagnostics: %d %s %s", res.code, res.stdout, res.stderr)
		}
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		files := map[string][]byte{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			files[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		raw, _ := os.ReadFile(path)
		for _, s := range diagnosticsSecrets {
			for name, b := range files {
				if strings.Contains(string(b), s) {
					t.Errorf("%s holds %q", name, s)
				}
			}
			if strings.Contains(string(raw), s) || strings.Contains(res.stdout+res.stderr, s) {
				t.Errorf("the archive or output holds %q", s)
			}
		}
		return res, files
	}
	_, gfiles := bundle(gr, "go.zip")
	if len(gfiles) != 2 {
		t.Fatalf("entries: %d", len(gfiles))
	}
	harness.PinJSON(t, "redacted log", strings.Split(scrub(string(gfiles["logs/events.jsonl"]), gr, nil, nil), "\n"))
	pinManifest(t, "manifest", gfiles["manifest.json"], gr)

	// A root that was never initialized: every root-backed section is unavailable.
	fresh := filepath.Join(filepath.Dir(gr), "none-go")
	_, gf := bundle(fresh, "go-none.zip")
	pinManifest(t, "manifest of a root never initialized", gf["manifest.json"], fresh)

	// Output rules.
	fsguard.WriteFile(filepath.Join(out, "exists.zip"), []byte("x"), 0o600)
	for _, args := range [][]string{
		{"--json", "diagnostics", "--output", filepath.Join(out, "exists.zip")},
		{"--json", "diagnostics", "--output", filepath.Join(out, "report.txt")},
		{"--json", "diagnostics", "--output", filepath.Join(out, "missing-dir", "x.zip")},
		{"--json", "diagnostics", "--output", ""},
	} {
		runCase(t, gr, args...)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "exists.zip")); string(b) != "x" {
		t.Error("an existing output was replaced")
	}
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left a temporary file: %s", e.Name())
		}
	}
}

// manifestMasks are a manifest's fields that depend on the machine and not on LocalBase, each
// named. A pinned manifest carries the masks it needed, so a masked field that disappears fails
// the comparison instead of passing unnoticed.
var manifestMasks = []valueMask{
	// The hardware section: memory.DetectSpecs' CPU, GPU and memory (not the lab host).
	{name: "manifest hardware", match: func(p string) bool {
		return slices.Contains([]string{".hardware.data.cpu", ".hardware.data.gpu", ".hardware.data.memoryGb", ".hardware.data.vramGb"}, p)
	}},
	// Each model's fit: internal/diagnostics takes it from DetectSpecs' GPU memory, as the
	// "diagnostics fit" mask says of the human lines.
	{name: "manifest fit", match: func(p string) bool {
		return strings.HasPrefix(p, ".models.data.models[") && (strings.HasSuffix(p, ".memory.systemMemoryGb") || strings.HasSuffix(p, ".memory.verdict"))
	}},
	// Whether the user's service manager (launchd, systemd --user) answers is the session's, and
	// so is the service state it reports: "unknown" where no manager answers (a container),
	// "not_installed" where one does.
	{name: "service manager session", match: func(p string) bool {
		return p == ".service.data.managerAvailable" || p == ".service.data.managerState" || p == ".service.data.state"
	}},
}

// pinManifest pins a bundle's manifest, normalized as the transcripts are (scrub), without when it
// was made and by which build, with manifestMasks applied, and checks it is
// JSON.stringify(manifest, null, 2) plus a newline.
func pinManifest(t *testing.T, name string, raw []byte, root string) {
	t.Helper()
	if string(raw[:2]) != "{\n" || !strings.HasSuffix(string(raw), "}\n") {
		t.Errorf("%s is not JSON.stringify(manifest, null, 2) + newline", name)
	}
	var v any
	if err := json.Unmarshal([]byte(scrub(string(raw), root, nil, nil)), &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	v = harness.PruneJSON(v, "", func(p string) bool {
		return p == ".generatedAt" || p == ".version" || p == ".logs.data.serviceStderr.modifiedAt" // written separately
	})
	masked, used := maskJSON(v, "", manifestMasks)
	harness.PinJSON(t, name, map[string]any{"masks": used, "manifest": masked})
}

// plantPrivateState writes secrets where M3.5 and M4 keep private state: a
// download auth profile, an imported model whose source is a signed URL, a
// custom voice whose display name is personal (logs carry its id only),
// and a Go-only setting.
func plantPrivateState(t *testing.T, root string) {
	t.Helper()
	// A run checkpoint: in-flight model output (DESIGN §21.6).
	fsguard.MkdirAll(filepath.Join(root, "runs"), 0o700)
	fsguard.WriteFile(filepath.Join(root, "runs", "run_0123.json"), []byte(`{"id":"run_0123","owner":"local","status":"in_progress","output":{"content":"runOutputSecret55"}}`), 0o600)
	fsguard.MkdirAll(filepath.Join(root, "credentials"), 0o700)
	fsguard.WriteFile(filepath.Join(root, "credentials", "auth-profiles.json"), []byte(`{"profiles":[{"id":"vendor","origins":["https://cdn.example.com"],`+
		`"headers":{"X-Vendor-Key":"authProfileHeaderSecret31"}}]}`), 0o600)
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := "2026-10-20T10:00:00.000Z"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO imported_models (id, definition, created_at, updated_at) VALUES (?, ?, ?, ?)`, []any{"signed-import",
			`{"id":"signed-import","task":"chat","artifacts":[{"filename":"m.gguf","role":"weights","required":true,` +
				`"source":{"kind":"url","url":"https://cdn.example.com/m.gguf?X-Amz-Signature=signedUrlSignature62"}}]}`, now, now}},
		{`INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
			VALUES ('voice_planted', 'VoiceNameSecretAda', 'voicenamesecretada', ?, 1000, 5000, ?, 'key_x', 'I consent.', ?)`, []any{strings.Repeat("a", 64), now, now}},
		{`INSERT INTO go_settings (key, value, updated_at) VALUES ('test.private', 'goSettingSecret48', ?)`, []any{now}},
	} {
		if _, err := st.DB.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The voice is referenced in the log by id, as the gateway logs it.
	f, _ := fsguard.Append(filepath.Join(root, "logs", "events.jsonl"))
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "id": "00000000-0000-4000-8000-000000000099", "timestamp": "2026-10-20T10:00:09.000Z",
		"severity": "info", "eventName": "voices.created", "category": "runtime", "component": "voices", "runtime": "tts",
		"message": "Custom voice created.", "attributes": map[string]any{"voice_id": "voice_planted"}})
	f.Write(append(b, '\n'))
	f.Close()
}
