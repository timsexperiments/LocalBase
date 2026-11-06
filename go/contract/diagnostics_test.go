package contract

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
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

// TestCLIDiagnostics compares the bundles of both CLIs on twin roots with
// logs, catalog patches and problems, a feed setting, OTLP environment and
// captured stderr: the manifests and redacted logs match, and no planted
// secret reaches either archive.
func TestCLIDiagnostics(t *testing.T) {
	requireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	for _, r := range roots {
		if res := runCLI(t, "go", r, "--json", "init"); res.code != 0 {
			t.Fatalf("init: %s", res.stderr)
		}
		mustMkdirAll(filepath.Join(r, "logs"), 0o700)
		mustWriteFile(filepath.Join(r, "logs", "events.jsonl"), []byte(diagnosticsLog()), 0o600)
		mustWriteFile(filepath.Join(r, "logs", "service-stderr.log"), []byte("panic: stderrCrashSecret\n"), 0o600)
		mustMkdirAll(filepath.Join(r, "catalog.d"), 0o700)
		mustWriteFile(filepath.Join(r, "catalog.d", "patch.json"), []byte(`{"modelId": "whisper-base-q8_0", "notes": "Patched.",
			"repositoryRevision": "`+strings.Repeat("e", 40)+`"}`), 0o600)
		mustWriteFile(filepath.Join(r, "catalog.d", "broken.json"), []byte(`{"modelId": "qwen3.5-4b-q4_k_m", "storageGb": "big"}`), 0o600)
		plantPrivateState(t, r)
	}
	env := []string{"LOCALBASE_CATALOG_FEED_URL=https://user:feedUserPassword@feed.example.com/feed.json?sig=feedSignature99#frag",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318/base/", "OTEL_EXPORTER_OTLP_HEADERS=x-auth=otlpHeaderSecret77"}
	out := scratchDir(t)
	bundle := func(impl, root, name string) (cliResult, map[string][]byte) {
		path := filepath.Join(out, name)
		res := runCLIEnv(t, impl, root, env, "--json", "diagnostics", "--output", path)
		if res.code != 0 {
			t.Fatalf("%s diagnostics: %d %s %s", impl, res.code, res.stdout, res.stderr)
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
					t.Errorf("%s: %s holds %q", impl, name, s)
				}
			}
			if strings.Contains(string(raw), s) || strings.Contains(res.stdout+res.stderr, s) {
				t.Errorf("%s: the archive or output holds %q", impl, s)
			}
		}
		return res, files
	}
	tres, tfiles := bundle("ts", ts, "ts.zip")
	gres, gfiles := bundle("go", gr, "go.zip")
	if a, b := normalize(t, strings.ReplaceAll(tres.stdout, "ts.zip", "X.zip"), ts), normalize(t, strings.ReplaceAll(gres.stdout, "go.zip", "X.zip"), gr); true {
		skip := func(p string) bool { return p == ".data.archive.bytes" }
		if d := diffJSON("", a, b, skip); len(d) > 0 {
			t.Errorf("envelope differs:\n  %s", strings.Join(d, "\n  "))
		}
	}
	if strings.ReplaceAll(tres.stderr, ts, "<root>") != strings.ReplaceAll(strings.ReplaceAll(gres.stderr, gr, "<root>"), "go.zip", "ts.zip") {
		t.Errorf("human output:\n ts %s\n go %s", tres.stderr, gres.stderr)
	}
	if len(tfiles) != 2 || len(gfiles) != 2 {
		t.Fatalf("entries: ts %d, go %d", len(tfiles), len(gfiles))
	}
	if string(tfiles["logs/events.jsonl"]) != string(gfiles["logs/events.jsonl"]) {
		t.Errorf("logs differ:\n ts %s\n go %s", tfiles["logs/events.jsonl"], gfiles["logs/events.jsonl"])
	}
	compareManifests(t, tfiles["manifest.json"], gfiles["manifest.json"])

	// A root that was never initialized: every root-backed section is
	// unavailable on both.
	fresh := [2]string{filepath.Join(filepath.Dir(ts), "none-ts"), filepath.Join(filepath.Dir(gr), "none-go")}
	_, tf := bundle("ts", fresh[0], "ts-none.zip")
	_, gf := bundle("go", fresh[1], "go-none.zip")
	compareManifests(t, tf["manifest.json"], gf["manifest.json"])

	// Output rules.
	mustWriteFile(filepath.Join(out, "exists.zip"), []byte("x"), 0o600)
	for _, args := range [][]string{
		{"--json", "diagnostics", "--output", filepath.Join(out, "exists.zip")},
		{"--json", "diagnostics", "--output", filepath.Join(out, "report.txt")},
		{"--json", "diagnostics", "--output", filepath.Join(out, "missing-dir", "x.zip")},
		{"--json", "diagnostics", "--output", ""},
	} {
		compareCLI(t, roots, cliCase{args: args})
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

func compareManifests(t *testing.T, a, b []byte) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("ts manifest: %v", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("go manifest: %v", err)
	}
	skip := func(p string) bool {
		return p == ".generatedAt" || p == ".version" || p == ".logs.data.serviceStderr.modifiedAt" // written separately
	}
	if d := diffJSON("", x, y, skip); len(d) > 0 {
		t.Errorf("manifests differ:\n  %s\nts %s\ngo %s", strings.Join(d, "\n  "), a, b)
	}
	if string(a[:2]) != "{\n" || string(b[:2]) != "{\n" || !strings.HasSuffix(string(b), "}\n") {
		t.Error("the manifest is not JSON.stringify(manifest, null, 2) + newline")
	}
}

// plantPrivateState writes secrets where M3.5 and M4 keep private state: a
// download auth profile, an imported model whose source is a signed URL, a
// custom voice whose display name is personal (logs carry its id only),
// and a Go-only setting.
func plantPrivateState(t *testing.T, root string) {
	t.Helper()
	// A run checkpoint: in-flight model output (DESIGN §21.6).
	mustMkdirAll(filepath.Join(root, "runs"), 0o700)
	mustWriteFile(filepath.Join(root, "runs", "run_0123.json"), []byte(`{"id":"run_0123","owner":"local","status":"in_progress","output":{"content":"runOutputSecret55"}}`), 0o600)
	mustMkdirAll(filepath.Join(root, "credentials"), 0o700)
	mustWriteFile(filepath.Join(root, "credentials", "auth-profiles.json"), []byte(`{"profiles":[{"id":"vendor","origins":["https://cdn.example.com"],`+
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
	f, _ := mustAppend(filepath.Join(root, "logs", "events.jsonl"))
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "id": "00000000-0000-4000-8000-000000000099", "timestamp": "2026-10-20T10:00:09.000Z",
		"severity": "info", "eventName": "voices.created", "category": "runtime", "component": "voices", "runtime": "tts",
		"message": "Custom voice created.", "attributes": map[string]any{"voice_id": "voice_planted"}})
	f.Write(append(b, '\n'))
	f.Close()
}
