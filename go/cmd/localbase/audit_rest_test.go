package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/timsexperiments/LocalBase/go/internal/audit"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

// changedConfig is this root's configuration as a file to apply, with one setting changed.
func changedConfig(t *testing.T, root, dir string) string {
	t.Helper()
	code, out := runApp(t, "--root", root, "config", "show")
	if code != 0 {
		t.Fatal(out)
	}
	lines := strings.Split(out, "\n")
	changed := false
	for i, line := range lines {
		if key, _, ok := strings.Cut(line, " = "); ok && key == "ctxSize" {
			lines[i], changed = "ctxSize = 2049", true
		}
	}
	if !changed {
		t.Fatalf("no ctxSize in %s", out)
	}
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// voiceClip is a recording a voice can be made from: five seconds of quiet speech-rate noise.
func voiceClip() []byte {
	const rate = 24000
	data := make([]byte, 5*rate*2)
	for i := 0; i+1 < len(data); i += 2 {
		binary.LittleEndian.PutUint16(data[i:], uint16(int16(i*7919%20000-10000)))
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// The commands that change settings, models and backends write one line each, in the words
// their routes use, with nothing a person would mind in a log.
func TestOtherCommandsAreAudited(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const header = "PLANTED_auth_profile_header_value_77c1"
	t.Setenv("AUDIT_TEST_HEADER", header)
	e := &auditEnv{t: t, root: root, secrets: map[string]string{"an auth profile's header": header}}
	write := func(name string, b []byte) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fake := write("fake-backend", []byte("#!/bin/sh\nexit 1\n"))
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[goruntime.GOARCH]
	definition, _ := json.Marshal(map[string]any{"schema": "localbase.backend/v1", "id": "audit-fake", "name": "Audit fake", "engine": "fake",
		"adapter": "process-openai/v1", "tasks": []string{"chat"}, "formats": []string{"gguf"}, "capabilities": []string{},
		"architectures": map[string]any{"supported": []string{"audittest"}},
		"platforms":     []any{map[string]any{"os": goruntime.GOOS, "arch": arch, "accelerator": "cpu"}},
		"launch": map[string]any{"command": fake, "args": []string{"--port", "{port}", "-m", "{model}"}, "healthPath": "/health",
			"memory": map[string]any{"weightsFactor": 1, "overheadGb": 0, "assumption": "a test"}}})
	weights := filepath.Join(tmp, "audit-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, weights, 1<<20)
	const model = "qwen3.5-4b-q4_k_m"

	var exported, voiceID string
	shown := map[string]bool{}
	for _, s := range []struct {
		command                 string
		args                    []string
		event, action, kind, id string
	}{
		{"configure", []string{"configure", "--defaults", "--no-create-key"}, "config", "configure", "configuration", ""},
		{"speech marking", []string{"speech", "marking", "off"}, "speech", "off", "setting", "speech-marking"},
		{"usage retention", []string{"usage", "retention", "7"}, "usage", "set-retention", "usage", ""},
		{"usage recording", []string{"usage", "recording", "off"}, "usage", "set-recording", "usage", ""},
		{"usage clear", []string{"usage", "clear", "--yes"}, "usage", "clear", "usage", ""},
		{"models feed use", []string{"models", "feed", "use", "off"}, "feed", "use", "feed", ""},
		{"backends add", []string{"backends", "add", "--file", write("backend.json", definition)}, "audit.backend-registered", "add", "backend", "audit-fake"},
		{"models import", []string{"models", "import", weights, "--task", "chat", "--id", "audit-import", "--name", "Audit"}, "imports", "import", "model", "audit-import"},
		{"models backend", []string{"models", "backend", "audit-import", "--set", "audit-fake"}, "audit.model-backend-changed", "set", "model", "audit-import"},
		{"models backend", []string{"models", "backend", "audit-import", "--auto"}, "audit.model-backend-changed", "reset", "model", "audit-import"},
		{"models delete", []string{"models", "delete", "audit-import", "--yes"}, "imports", "delete", "model", "audit-import"},
		{"models add-definition", nil, "imports", "add-definition", "model", "audit-import"},
		{"backends remove", []string{"backends", "remove", "audit-fake"}, "audit.backend-removed", "remove", "backend", "audit-fake"},
		{"auth-profiles add", []string{"auth-profiles", "add", "--id", "audit-profile", "--origin", "https://models.example", "--header-env", "X-Planted=AUDIT_TEST_HEADER"}, "auth-profiles", "add", "auth-profile", "audit-profile"},
		{"auth-profiles remove", []string{"auth-profiles", "remove", "audit-profile"}, "auth-profiles", "remove", "auth-profile", "audit-profile"},
		{"voices add", []string{"voices", "add", write("voice.wav", voiceClip()), "--name", "PLANTED voice name", "--consent"}, "voices", "create", "voice", "*"},
		{"voices remove", nil, "voices", "delete", "voice", "*"},
		{"models placement", []string{"models", "placement", model, "--reset"}, "placement", "clear", "model", model},
		{"models forget-peaks", []string{"models", "forget-peaks", model}, "models", "forget-peaks", "model", model},
		{"config apply", nil, "config", "apply", "configuration", ""},
		{"reset", []string{"reset", "--yes"}, "config", "reset", "configuration", ""},
	} {
		t.Run(s.command, func(t *testing.T) {
			shown[s.command] = true
			if auditClasses[s.command] != "audited" {
				t.Fatalf("%q is classed %q", s.command, auditClasses[s.command])
			}
			before := len(auditLines(t, root))
			args := s.args
			if s.command == "models add-definition" {
				args = []string{"models", "add-definition", exported}
			}
			if s.command == "voices remove" {
				args = []string{"voices", "remove", voiceID}
			}
			if s.command == "models forget-peaks" { // a peak to forget, as a load would have recorded it
				st, err := store.Open(filepath.Join(root, "local-base.db"))
				if err != nil {
					t.Fatal(err)
				}
				err = st.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: model, ArtifactSHA256: strings.Repeat("a", 64),
					LaunchShape: "default", PeakBytes: 1 << 30, Method: "footprint"})
				_ = st.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if s.command == "config apply" {
				args = []string{"config", "apply", "--file", changedConfig(t, root, tmp), "--restart", "never"}
			}
			data := e.run(args...)
			if s.command == "models import" {
				_, out := runApp(t, "--root", root, "models", "export", "audit-import")
				exported = write("definition.json", []byte(out))
			}
			if _, ok := data["auditWritten"]; ok {
				t.Errorf("auditWritten is only for a failure: %v", data)
			}
			lines := auditLines(t, root)
			if len(lines) != before+1 {
				t.Fatalf("%d new audit lines, want 1: %+v", len(lines)-before, lines[before:])
			}
			got := lines[before]
			if s.command == "voices add" {
				voiceID = got.TargetID
			}
			want := "audit." + s.event + "-changed"
			if strings.HasPrefix(s.event, "audit.") { // the backend events DESIGN §31.4 names
				want = s.event
			}
			if got.Event != want || got.Action != s.action || got.ActorKind != "cli" || got.TargetKind != s.kind {
				t.Errorf("%+v", got)
			}
			if s.id == "*" && got.TargetID == "" || s.id != "*" && got.TargetID != s.id {
				t.Errorf("target %q, want %q", got.TargetID, s.id)
			}
		})
	}

	// A first key is a key made, and says what for.
	t.Run("the key configure makes", func(t *testing.T) {
		before := len(auditLines(t, root))
		data := e.run("configure", "--create-key")
		e.secrets["configure's key"], _ = data["createdKey"].(map[string]any)["secret"].(string)
		lines := auditLines(t, root)
		if len(lines) != before+2 || lines[before].Event != "audit.config-changed" && lines[before+1].Event != "audit.config-changed" {
			t.Errorf("%+v", lines[before:])
		}
		found := false
		for _, l := range lines[before:] {
			found = found || l.Event == "audit.api-key-changed" && l.Action == "create" && l.Purpose == "initial" && l.TargetID != ""
		}
		if !found {
			t.Errorf("no key line: %+v", lines[before:])
		}
	})

	t.Run("upgrade's temporary key", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		defer ts.Close()
		c := &cli.Ctx{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
		env, err := openEnv(&cli.Ctx{Global: cli.Global{Root: root}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
		if err != nil {
			t.Fatal(err)
		}
		defer env.Close()
		before := len(auditLines(t, root))
		_ = waitGatewayIdle(c, env, ts.URL, 0, func(time.Duration) {})
		lines := auditLines(t, root)[before:]
		if len(lines) != 2 || lines[0].Action != "create" || lines[1].Action != "delete" || lines[0].TargetID == "" || lines[0].TargetID != lines[1].TargetID ||
			lines[0].Purpose != "upgrade" || lines[1].Purpose != "upgrade" {
			t.Errorf("%+v", lines)
		}
	})

	// Everything classified as audited is shown, here or for keys and access.
	for command, class := range auditClasses {
		if class != "audited" || strings.HasPrefix(command, "keys") || strings.HasPrefix(command, "access") {
			continue
		}
		if !shown[command] {
			t.Errorf("%s is classified as audited, and nothing here shows it", command)
		}
	}

	e.secrets["a voice's name"] = "PLANTED voice name"
	entries, err := os.ReadDir(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(root, "logs", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for what, value := range e.secrets {
			if strings.Contains(string(b), value) {
				t.Errorf("%s holds %s", entry.Name(), what)
			}
		}
	}
}

func TestUpgradeOutcome(t *testing.T) {
	for want, got := range map[string]string{
		"applied":      ApplyOutcome(upgrade.Result{}, nil),
		"rolled-back":  ApplyOutcome(upgrade.Result{RolledBack: true}, errors.New("PLANTED")),
		"smoke-failed": ApplyOutcome(upgrade.Result{}, &upgrade.SmokeFailed{Err: errors.New("PLANTED")}),
		"failed":       ApplyOutcome(upgrade.Result{}, errors.New("PLANTED")),
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// A target from a file someone else wrote is only logged in the shape of an id.
func TestAnOddTargetIsNotLogged(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := audit.New(r, &bytes.Buffer{})
	a.Change("imports", "add-definition", "model", "x\n{\"eventName\":\"audit.forged-changed\"}")
	a.Change("imports", "add-definition", "model", "ok.id-1")
	lines := auditLines(t, dir)
	if len(lines) != 2 || lines[0].TargetID != "" || lines[1].TargetID != "ok.id-1" || lines[0].Event != "audit.imports-changed" {
		t.Errorf("%+v", lines)
	}
}

func TestVersionForAudit(t *testing.T) {
	for in, want := range map[string]string{"0.2.0": "0.2.0", "9.9.9": "9.9.9", "not a version": "unknown", "": "unknown",
		"0.2.0\nPLANTED": "unknown", strings.Repeat("9", 100000): "unknown"} {
		if got := VersionForAudit(in); got != want {
			t.Errorf("%.40q: %.40q, want %q", in, got, want)
		}
	}
}

// A test can't reach the installed service, whatever it asks, and what happened is on the record.
func TestAServiceCommandOnTheDefaultRootIsRefusedInATest(t *testing.T) {
	home := t.TempDir() // so even a guard that failed would only find a scratch root
	t.Setenv("HOME", home)
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, c := range []struct {
		args       []string
		said, line string
	}{
		{[]string{"stop"}, "This stops the installed LocalBase service.", "audit.service-stopped"},
		{[]string{"restart"}, "This restarts the installed LocalBase service.", "audit.service-restarted"},
		{[]string{"start"}, "", "audit.service-started"},
	} {
		code, out := runApp(t, c.args...)
		if code == 0 || !strings.Contains(out, "A test tried to manage the service on the default root") {
			t.Errorf("%v: %d %.300q", c.args, code, out)
		}
		if c.said != "" && !strings.Contains(out, c.said) {
			t.Errorf("%v doesn't say %q: %.300q", c.args, c.said, out)
		}
		b, err := os.ReadFile(filepath.Join(root.Default(), "logs", "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var last string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.Contains(line, c.line) {
				last = line
			}
		}
		for _, want := range []string{`"result":"failed"`, `"actor_kind":"cli"`, `"service_id":"`, `"root":"`} {
			if !strings.Contains(last, want) {
				t.Errorf("%v: %q lacks %s", c.args, last, want)
			}
		}
	}
}

// Nothing to say on a root that isn't the installed one, and uninstall says its record goes too.
func TestScratchRootsGetNoInstalledNotice(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	code, out := runApp(t, "--root", dir, "uninstall", "--yes")
	if code != 0 || strings.Contains(out, "installed LocalBase service") || !strings.Contains(out, "Its audit log goes with it.") {
		t.Errorf("%d %.400q", code, out)
	}
}

// An env from readEnv has its root's audit log, as openEnv's does, so a command that opens the
// root with readEnv and makes a change writes its record.
func TestReadEnvHasTheRootsAuditLog(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	before := len(auditLines(t, dir))
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.audit.Change("models", "forget-peaks", "model", "m")
	lines := auditLines(t, dir)
	if len(lines) != before+1 || lines[len(lines)-1].Event != "audit.models-changed" || lines[len(lines)-1].TargetID != "m" {
		t.Errorf("after one record from readEnv's env: %+v", lines[before:])
	}
}

// models feed use can save the setting in a root it can't open (its marker unreadable), and then has
// no audit log: the one place a command has none. It succeeds, and its result says nothing about
// a record, as before; it doesn't hand WithResult a nil audit.Log.
func TestFeedUseWithARootThatCannotBeOpenedHasNoAuditFields(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".localbase-root.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Open(dir); err == nil {
		t.Fatal("the root should not open with a broken marker")
	}
	code, out := runApp(t, "--root", dir, "--json", "models", "feed", "use", "default")
	if code != 0 || !strings.Contains(out, `"feed"`) || strings.Contains(out, "auditWritten") || strings.Contains(out, "auditError") {
		t.Errorf("models feed use in a root that can't be opened: exit %d\n%s", code, out)
	}
}
