package contract

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

const backupModel = "backup-tiny"

// backupRoot is a scratch root with a fake backend and a tiny model, that serves chat through
// nothing but its own settings, so a restored copy can be asked to do the same.
func backupRoot(t *testing.T, tmp, name string) *e2e {
	t.Helper()
	fake := filepath.Join(tmp, "fakebackend")
	if _, err := os.Stat(fake); err != nil {
		if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
			t.Fatalf("build fakebackend: %v %s", err, out)
		}
	}
	e := &e2e{t: t, root: filepath.Join(tmp, name), outDir: tmp}
	if r := e.cli(nil, "init"); r.code != 0 {
		t.Fatalf("init: %s%s", r.stdout, r.stderr)
	}
	def, _ := json.Marshal(map[string]any{"schema": "localbase.backend/v1", "id": "backup-fake", "name": "Backup fake", "engine": "fake-engine",
		"adapter": "process-openai/v1", "tasks": []string{"chat"}, "formats": []string{"gguf"}, "capabilities": []string{},
		"architectures": map[string]any{"supported": []string{"backuptest"}},
		"platforms":     []any{map[string]any{"os": goruntime.GOOS, "arch": map[string]string{"arm64": "arm64", "amd64": "x64"}[goruntime.GOARCH], "accelerator": "cpu"}},
		"launch": map[string]any{"command": fake, "args": []string{"--host", "{host}", "--port", "{port}", "-m", "{model}"}, "healthPath": "/health",
			"memory": map[string]any{"weightsFactor": 1, "overheadGb": 0, "assumption": "a fake backend holds next to nothing"}}})
	defPath := filepath.Join(tmp, name+"-backend.json")
	if err := mustWriteFile(defPath, def, 0o600); err != nil {
		t.Fatal(err)
	}
	e.json("backends", "add", "--file", defPath)
	models := filepath.Join(e.root, "models", "llm")
	if err := mustMkdirAll(models, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(models, "backup-tiny.gguf")
	if err := testutil.GGUF(file, map[string]any{"general.architecture": "backuptest", "general.file_type": 15, "backuptest.context_length": 4096}, 1<<16); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	entry, _ := json.Marshal(map[string]any{"modelId": backupModel, "displayName": "Backup tiny", "kind": "llm", "provider": "LocalBase tests", "family": "Backup",
		"version": "test", "size": "tiny", "quant": "Q4_K_M", "storageGb": 0.01, "source": "https://huggingface.co/localbase/backup-tiny", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts":       []any{map[string]any{"sourcePath": "backup-tiny.gguf", "filename": "backup-tiny.gguf", "expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"text"}, "outputModalities": []string{"text"}, "contextWindowTokens": 4096, "capabilities": []string{"text-generation"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture."})
	if err := mustMkdirAll(filepath.Join(e.root, "catalog.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(e.root, "catalog.d", backupModel+".json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	show := e.cli(nil, "config", "show")
	if show.code != 0 {
		t.Fatal(show.stderr)
	}
	replace := map[string]string{"selectedLlmModels": `["` + backupModel + `"]`, "selectedSttModels": "[]", "selectedImageModels": "[]", "selectedTtsModels": "[]", "selectedVideoModels": "[]",
		"activeLlmModel": `"` + backupModel + `"`, "activeSttModel": `""`, "activeImageModel": `""`, "activeTtsModel": `""`, "activeVideoModel": `""`,
		"ctxSize": "2048", "ctxMode": `"explicit"`, "parallel": "1", "percent": "1", "minimumGb": "0"}
	lines := strings.Split(show.stdout, "\n")
	for i, line := range lines {
		if key, _, ok := strings.Cut(line, " = "); ok {
			if v, known := replace[key]; known {
				lines[i] = key + " = " + v
			}
		}
	}
	cfg := filepath.Join(tmp, name+"-config.toml")
	if err := mustWriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "config", "apply", "--file", cfg, "--restart", "never"); r.code != 0 {
		t.Fatalf("config apply: %s%s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "models", "backend", backupModel, "--set", "backup-fake"); r.code != 0 {
		t.Fatalf("set backend: %s%s", r.stdout, r.stderr)
	}
	e.key = e.json("keys", "create", "--name", "backup", "--scopes", "inference:chat,models:read")["secret"].(string)
	return e
}

func backupGoOnly(t *testing.T) {
	t.Helper()
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("Go-only feature")
	}
}

// The operator's test: a backup from a scratch root restores into a fresh root, which passes
// root.Open's owner checks, and serves a chat request with the fake backend.
func TestBackupRestoresIntoFreshRootAndServes(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	one := backupRoot(t, tmp, "root-one")
	one.serve()
	if r := one.chat(backupModel); r.Status != 200 {
		t.Fatalf("the source doesn't serve: %d %s", r.Status, r.Body)
	}

	// While it serves: a backup does not need the gateway stopped.
	file := filepath.Join(tmp, "out", "b"+backup.Extension)
	if err := mustMkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	made := one.cli(nil, "backup", "--output", file)
	if made.code != 0 {
		t.Fatalf("backup: %s%s", made.stdout, made.stderr)
	}
	if !strings.Contains(made.stderr, "not encrypted") {
		t.Errorf("the warning: %q", made.stderr)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the backup is %o", info.Mode().Perm())
	}
	running := one.cli(nil, "restore", "--replace", "--yes", file)
	if running.code == 0 || !strings.Contains(running.stderr+running.stdout, "Stop it where it is running") {
		t.Errorf("a restore over a running gateway: %d %s%s", running.code, running.stdout, running.stderr)
	}
	one.stop()

	two := &e2e{t: t, root: filepath.Join(tmp, "root-two"), outDir: tmp, key: one.key}
	dry := two.cli(nil, "restore", "--dry-run", file)
	if dry.code != 0 || !strings.Contains(dry.stdout, backup.SignOutLine) {
		t.Fatalf("dry run: %d %s%s", dry.code, dry.stdout, dry.stderr)
	}
	if _, err := os.Stat(two.root); !os.IsNotExist(err) {
		t.Errorf("a dry run made the root: %v", err)
	}
	done := two.cli(nil, "restore", "--yes", file)
	if done.code != 0 {
		t.Fatalf("restore: %s%s", done.stdout, done.stderr)
	}
	for _, want := range []string{backup.SignOutLine, "Nothing was started", "start", "backup-fake", filepath.Join(tmp, "fakebackend")} {
		if !strings.Contains(done.stdout, want) {
			t.Errorf("the output should say %q:\n%s", want, done.stdout)
		}
	}
	if _, err := root.Open(two.root); err != nil {
		t.Fatalf("the restored root doesn't pass the checks a gateway gets: %v", err)
	}
	// Models are not in a backup, and it says so, so this one is not installed yet.
	two.serve()
	defer two.stop()
	if r := two.chat(backupModel); r.Status != 409 || !strings.Contains(string(r.Body), "models install") {
		t.Errorf("before its model is installed: %d %s", r.Status, r.Body)
	}
	if !strings.Contains(done.stdout, backupModel) {
		t.Errorf("the output should name the model to install:\n%s", done.stdout)
	}
	// Put it where an install would. A real one would download it.
	weights, err := os.ReadFile(filepath.Join(one.root, "models", "llm", "backup-tiny.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mustMkdirAll(filepath.Join(two.root, "models", "llm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(two.root, "models", "llm", "backup-tiny.gguf"), weights, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := two.chat(backupModel); r.Status != 200 {
		t.Fatalf("the restored root doesn't serve chat: %d %s", r.Status, r.Body)
	}
	if !strings.Contains(string(two.do("GET", "/v1/models", "").Body), backupModel) {
		t.Error("the restored catalog entry isn't listed")
	}
	stale := do(t, "POST", two.base+"/v1/chat/completions", bearer("lb_notAKeyThisRootEverHad0000000000"), `{"model":"`+backupModel+`","messages":[]}`)
	if stale.Status != 401 {
		t.Errorf("a key it never had: %d", stale.Status)
	}
}

// A secret left blank would make a validator refuse the file, and that stops serve. So a backup
// without secrets must start, and must hold none of them.
func TestRestoreWithoutSecretsStarts(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	one := backupRoot(t, tmp, "root-one")
	const oidc, smtp, header = "PLANTED_OIDC_SECRET_77", "PLANTED_SMTP_SECRET_77", "PLANTED_HEADER_SECRET_77"
	plants := map[string]string{
		"ui-access.json": `{"provider":{"kind":"direct","registrations":[
			{"kind":"oidc","id":"google","name":"Google","issuer":"https://accounts.example.com","clientId":"cid","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"` + oidc + `"}},
			{"kind":"magic-link","id":"mail","name":"Mail"}]},"origin":"https://localbase.example.com","permissions":["inference:chat"]}`,
		"email-delivery.json":                              `{"host":"smtp.example.com","port":587,"security":"starttls","authentication":{"kind":"password","username":"u","password":"` + smtp + `"},"from":"a@example.com"}`,
		filepath.Join("credentials", "auth-profiles.json"): `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"X-Api-Key":"` + header + `"}}]}`,
	}
	for name, body := range plants {
		p := filepath.Join(one.root, name)
		if err := mustMkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := mustWriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(tmp, "b"+backup.Extension)
	made := one.cli(nil, "backup", "--exclude-secrets", "--output", file)
	if made.code != 0 {
		t.Fatalf("%s%s", made.stdout, made.stderr)
	}
	if !strings.Contains(made.stderr, "not anonymised") || !strings.Contains(made.stderr, "not encrypted") {
		t.Errorf("the warning: %q", made.stderr)
	}
	for _, want := range []string{"OIDC client secret (google)", "access oidc add --id google", "SMTP password", "download credential (vendor)"} {
		if !strings.Contains(made.stdout, want) {
			t.Errorf("the output should name %q:\n%s", want, made.stdout)
		}
	}
	two := &e2e{t: t, root: filepath.Join(tmp, "root-two"), outDir: tmp, key: one.key}
	done := two.cli(nil, "restore", "--yes", file)
	if done.code != 0 {
		t.Fatalf("%s%s", done.stdout, done.stderr)
	}
	if !strings.Contains(done.stdout, "Put back OIDC client secret (google)") {
		t.Errorf("restore should say what to put back:\n%s", done.stdout)
	}
	for _, secret := range []string{oidc, smtp, header} {
		if strings.Contains(done.stdout+done.stderr+made.stdout+made.stderr, secret) {
			t.Errorf("the output shows %s", secret)
		}
		filepath.WalkDir(two.root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), secret) {
				t.Errorf("%s holds %s", p, secret)
			}
			return nil
		})
	}
	weights, err := os.ReadFile(filepath.Join(one.root, "models", "llm", "backup-tiny.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mustMkdirAll(filepath.Join(two.root, "models", "llm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(two.root, "models", "llm", "backup-tiny.gguf"), weights, 0o600); err != nil {
		t.Fatal(err)
	}
	two.serve()
	defer two.stop()
	if r := two.chat(backupModel); r.Status != 200 {
		t.Fatalf("a root restored without secrets doesn't serve: %d %s", r.Status, r.Body)
	}
}

func TestBackupClassifiesEveryRootEntry(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	e.serve()
	defer e.stop()
	if r := e.chat(backupModel); r.Status != 200 {
		t.Fatalf("chat: %d %s", r.Status, r.Body)
	}
	if r := e.do("GET", "/health", ""); r.Status != 200 {
		t.Fatal(r.Status)
	}
	raw := e.cli(nil, "--json", "backup", "--dry-run")
	if raw.code != 0 {
		t.Fatalf("%s%s", raw.stdout, raw.stderr)
	}
	var out struct {
		Data backup.Result `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Warning == "" || out.Data.Manifest == nil {
		t.Fatalf("output: %s", raw.stdout)
	}
	for _, n := range out.Data.Manifest.NotIncluded {
		if n.Unknown {
			t.Errorf("a root that ran a gateway holds %q, which neither table in §34.1 names", n.What)
		}
		if n.Why == "" {
			t.Errorf("%q is left out with no reason", n.What)
		}
	}
	if !strings.Contains(raw.stderr, "not encrypted") {
		t.Errorf("with --json the warning still goes to stderr: %q", raw.stderr)
	}
}

func TestBackupRestoreCLI(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "backup", "--output", file); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "never overwrites") {
		t.Errorf("a second backup to one name: %d %s%s", r.code, r.stdout, r.stderr)
	}

	// A root that fails the checks a gateway gets isn't backed up, and says why.
	access := filepath.Join(e.root, "email-delivery.json")
	if err := mustWriteFile(access, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mustChmod(access, 0o666); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "backup", "--dry-run"); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "other users can change it") {
		t.Errorf("an unsafe root: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if err := mustChmod(access, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mustRemoveAll(access); err != nil {
		t.Fatal(err)
	}

	target := &e2e{t: t, root: filepath.Join(tmp, "new"), outDir: tmp}
	// Nothing to confirm with, so it refuses, having said what matters first.
	asked := target.cli(nil, "--non-interactive", "restore", file)
	if asked.code == 0 || !strings.Contains(asked.stderr+asked.stdout, "--yes") {
		t.Errorf("no confirmation: %d %s%s", asked.code, asked.stdout, asked.stderr)
	}
	if i := strings.Index(asked.stdout, backup.SignOutLine); i < 0 {
		t.Errorf("the sign-out line should come before it asks:\n%s", asked.stdout)
	}
	if _, err := os.Stat(target.root); !os.IsNotExist(err) {
		t.Errorf("it refused, and made the root: %v", err)
	}
	if r := target.cli(nil, "--json", "restore", file); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "--yes") {
		t.Errorf("--json without --yes: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if r := target.cli(nil, "restore"); r.code == 0 {
		t.Error("restore with no file")
	}
	if r := e.cli(nil, "restore", "--yes", file); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "--replace") {
		t.Errorf("a root with data, no --replace: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if r := e.cli(nil, "restore", "--undo"); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "no unfinished restore") {
		t.Errorf("--undo with nothing to undo: %d %s%s", r.code, r.stdout, r.stderr)
	}

	// Replacing keeps the old files, and the last line says how to delete them.
	replaced := e.cli(nil, "restore", "--replace", "--yes", file)
	if replaced.code != 0 {
		t.Fatalf("%s%s", replaced.stdout, replaced.stderr)
	}
	last := strings.Split(strings.TrimSpace(replaced.stdout), "\n")
	line := last[len(last)-1]
	if !strings.Contains(line, ".before-restore-") || !strings.Contains(line, "hold secrets") || !strings.Contains(line, "rm -rf") {
		t.Errorf("the last line should name the old files:\n%s", replaced.stdout)
	}
	matches, _ := filepath.Glob(filepath.Join(e.root, ".before-restore-*"))
	if len(matches) != 1 {
		t.Fatalf("old files: %v", matches)
	}
	if info, err := os.Stat(matches[0]); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the old files' directory: %v %v", info, err)
	}
}

func TestRestoreWarnsAboutBusyPorts(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port
	show := e.cli(nil, "config", "show")
	cfg := filepath.Join(tmp, "ports.toml")
	var out []string
	section := ""
	for _, line := range strings.Split(show.stdout, "\n") {
		if strings.HasPrefix(line, "[") {
			section = line
		}
		if section == "[gateway]" && strings.HasPrefix(line, "port = ") {
			line = fmt.Sprintf("port = %d", busy)
		}
		if section == "[runtime]" && strings.HasPrefix(line, "port = ") {
			line = fmt.Sprintf("port = %d", freePort())
		}
		if section == "[runtime]" && strings.HasPrefix(line, "sttPort = ") {
			line = fmt.Sprintf("sttPort = %d", freePort())
		}
		out = append(out, line)
	}
	if err := mustWriteFile(cfg, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "config", "apply", "--file", cfg, "--restart", "never"); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	target := &e2e{t: t, root: filepath.Join(tmp, "new"), outDir: tmp}
	connections := func() int {
		n := 0
		tcp := l.(*net.TCPListener)
		for {
			tcp.SetDeadline(time.Now().Add(300 * time.Millisecond))
			c, err := tcp.Accept()
			if err != nil {
				return n
			}
			c.Close()
			n++
		}
	}

	// With the flag, what listens on that port sees nothing at all.
	quiet := target.cli(nil, "restore", "--dry-run", "--no-port-probe", file)
	if quiet.code != 0 {
		t.Fatalf("%s%s", quiet.stdout, quiet.stderr)
	}
	if n := connections(); n != 0 {
		t.Errorf("--no-port-probe made %d connections", n)
	}
	if strings.Contains(quiet.stderr, "already uses port") || !strings.Contains(quiet.stderr, "Ports were not checked") || !strings.Contains(quiet.stderr, fmt.Sprint(busy)) {
		t.Errorf("it should say it did not check, and which ports: %s", quiet.stderr)
	}

	r := target.cli(nil, "restore", "--yes", file)
	if r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, fmt.Sprintf("already uses port %d", busy)) || !strings.Contains(r.stderr, "config apply") {
		t.Errorf("a busy port should be named: %s", r.stderr)
	}
	if n := connections(); n != 1 {
		t.Errorf("the check made %d connections, want 1", n)
	}
	// It starts nothing, so the port it warned about is still only the test's.
	if st, _, _ := mustOpenRoot(t, target.root).OwnerState(); st != "missing" {
		t.Errorf("a gateway was started: %s", st)
	}
}

func mustOpenRoot(t *testing.T, dir string) *root.Root {
	t.Helper()
	r, err := root.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBeforeRestoreIsNeverRead(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	const planted = "PLANTED_OLD_SECRET_FROM_BEFORE_RESTORE"
	old := filepath.Join(e.root, ".before-restore-20260101-000000")
	if err := mustMkdirAll(filepath.Join(old, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ui-access.json", "email-delivery.json", filepath.Join("credentials", "auth-profiles.json"), "local-base.db"} {
		if err := mustWriteFile(filepath.Join(old, name), []byte(planted), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A gateway starts with it there, and nothing it answers holds it.
	e.serve()
	for _, path := range []string{"/health", "/v1/models", "/openapi.json"} {
		if r := e.do("GET", path, ""); strings.Contains(string(r.Body), planted) {
			t.Errorf("%s answers with it", path)
		}
	}
	if r := e.chat(backupModel); r.Status != 200 {
		t.Errorf("chat: %d %s", r.Status, r.Body)
	}
	e.stop()

	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), planted) {
		t.Error("the backup file holds it")
	}
	listed := e.cli(nil, "backup", "--dry-run")
	if !strings.Contains(listed.stdout, ".before-restore-20260101-000000") {
		t.Errorf("it should be listed as not included:\n%s", listed.stdout)
	}

	report := filepath.Join(tmp, "diagnostics.zip")
	if r := e.cli(nil, "diagnostics", "--output", report); r.code != 0 {
		t.Fatalf("diagnostics: %s%s", r.stdout, r.stderr)
	}
	zr, err := zip.OpenReader(report)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(b), planted) {
			t.Errorf("diagnostics entry %s holds it", f.Name)
		}
	}
}

// A real gateway started with serve, on the default root of a scratch home. Restore doesn't send
// someone to `stop` for it, because stop really does refuse it, which is checked here too. `stop` refuses
// before it asks the service manager anything, so nothing outside the scratch home is touched.
func TestRestoreOverAForegroundGatewaySaysWhatStopsIt(t *testing.T) {
	backupGoOnly(t)
	home, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, home, filepath.Join(".local", "share", "local-base"))
	e.env = append(e.env, "HOME="+home)
	file := filepath.Join(home, "out", "b"+backup.Extension)
	if err := mustMkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("backup: %s%s", r.stdout, r.stderr)
	}
	e.serve()
	defer e.stop()
	r := e.cli(nil, "restore", "--replace", "--yes", file)
	said := r.stdout + r.stderr
	if r.code == 0 {
		t.Fatalf("it restored over a running gateway: %s", said)
	}
	for _, want := range []string{"started with local-base serve", "This is the default root, the one your installed LocalBase uses.", "Stop it where it is running"} {
		if !strings.Contains(said, want) {
			t.Errorf("no %q in: %s", want, said)
		}
	}
	if strings.Contains(said, " stop") || strings.Contains(said, "--root") {
		t.Errorf("it names a command that can't stop this gateway: %s", said)
	}
	stop := e.cli(nil, "stop")
	if stop.code == 0 || !strings.Contains(stop.stdout+stop.stderr, "a foreground gateway owns the root") {
		t.Errorf("stop on a gateway started with serve: %d %s%s", stop.code, stop.stdout, stop.stderr)
	}
}

func TestUninstallNamesBackups(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	if r := e.cli(nil, "backup"); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	dir := backup.DefaultDir(e.root)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups: %v %v", entries, err)
	}
	r := e.cli(nil, "uninstall", "--yes")
	if r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, dir) || !strings.Contains(r.stdout, "hold secrets") || !strings.Contains(r.stdout, "rm -rf") {
		t.Errorf("it should say what it left:\n%s", r.stdout)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the backups should stay: %v", err)
	}
	if _, err := os.Stat(e.root); !os.IsNotExist(err) {
		t.Errorf("the root should be gone: %v", err)
	}
	// With no backups, it says nothing about them.
	other := backupRoot(t, tmp, "other")
	if r := other.cli(nil, "uninstall", "--yes"); r.code != 0 || strings.Contains(r.stdout, "hold secrets") {
		t.Errorf("no backups: %d %s%s", r.code, r.stdout, r.stderr)
	}
}

// bigBackupRoot has a recording large enough that a run is still working when a signal arrives.
func bigBackupRoot(t *testing.T, tmp string) *e2e {
	t.Helper()
	e := backupRoot(t, tmp, "root")
	clip := make([]byte, 96<<20)
	if _, err := rand.Read(clip); err != nil {
		t.Fatal(err)
	}
	if err := mustMkdirAll(filepath.Join(e.root, "voices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(e.root, "voices", "voice-aaaaaaaaaaaa.wav"), clip, 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// startUntilWorking runs a command until its working directory appears, and returns it.
func startUntilWorking(t *testing.T, e *e2e, place string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := e.command(append([]string{"--root", e.root}, args...)...)
	cmd.Env = cleanEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if found, _ := filepath.Glob(filepath.Join(place, ".local-base-temporary-*")); len(found) > 0 {
			return cmd, found[0]
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	t.Fatal("no working directory appeared")
	return nil, ""
}

func noWorkingFiles(t *testing.T, place, when string) {
	t.Helper()
	if found, _ := filepath.Glob(filepath.Join(place, ".local-base-temporary-*")); len(found) > 0 {
		t.Errorf("%s left %v", when, found)
	}
}

// The operator's condition on a dry run's working directory: it is private, gone on every way out
// that can clean up, and named by doctor and the next run after one that can't.
func TestInterruptedRunsLeaveNothing(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := bigBackupRoot(t, tmp)
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--exclude-secrets", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}

	for name, sig := range map[string]syscall.Signal{"an interrupt": syscall.SIGINT, "a termination": syscall.SIGTERM} {
		t.Run(name+" during a dry run", func(t *testing.T) {
			cmd, working := startUntilWorking(t, e, tmp, "backup", "--dry-run", "--exclude-secrets")
			info, err := os.Stat(working)
			if err == nil && info.Mode().Perm() != 0o700 {
				t.Errorf("the working directory is %o", info.Mode().Perm())
			}
			cmd.Process.Signal(sig)
			if err := cmd.Wait(); err == nil {
				t.Error("it should not report success")
			}
			noWorkingFiles(t, tmp, name)
		})
		t.Run(name+" during a restore", func(t *testing.T) {
			target := &e2e{t: t, root: filepath.Join(tmp, "new"), outDir: tmp}
			cmd, _ := startUntilWorking(t, target, tmp, "restore", "--dry-run", "--no-port-probe", file)
			cmd.Process.Signal(sig)
			if err := cmd.Wait(); err == nil {
				t.Error("it should not report success")
			}
			noWorkingFiles(t, tmp, name)
			if _, err := os.Stat(target.root); !os.IsNotExist(err) {
				t.Errorf("it made the root: %v", err)
			}
		})
	}

	t.Run("a kill, which can't clean up", func(t *testing.T) {
		cmd, working := startUntilWorking(t, e, tmp, "backup", "--dry-run", "--exclude-secrets")
		cmd.Process.Kill()
		cmd.Wait()
		if _, err := os.Stat(working); err != nil {
			t.Skipf("it finished before the kill: %v", err)
		}
		r := e.cli(nil, "doctor")
		if !strings.Contains(r.stdout, working) || !strings.Contains(r.stdout, "may hold secrets") || !strings.Contains(r.stdout, "rm -rf") {
			t.Errorf("doctor should name it:\n%s", r.stdout)
		}
		if _, err := os.Stat(working); err != nil {
			t.Errorf("doctor should not remove it: %v", err)
		}
		next := e.cli(nil, "backup", "--dry-run", "--exclude-secrets")
		if next.code != 0 {
			t.Fatalf("%s%s", next.stdout, next.stderr)
		}
		if !strings.Contains(next.stderr, working) || !strings.Contains(next.stderr, "Removed") {
			t.Errorf("the next run should say what it removed: %s", next.stderr)
		}
		noWorkingFiles(t, tmp, "the next run")
	})
}

// A backend has no authentication. Its stored host can say 0.0.0.0, as the live root's does, and a
// restored root must still listen only on this computer, with what the report says it will.
func TestRestoredBackendStaysOnLoopback(t *testing.T) {
	backupGoOnly(t)
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		t.Skip("needs lsof")
	}
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	show := e.cli(nil, "config", "show")
	lines := strings.Split(show.stdout, "\n")
	section := ""
	for i, line := range lines {
		if strings.HasPrefix(line, "[") {
			section = line
		}
		if section == "[runtime]" && (strings.HasPrefix(line, "host = ") || strings.HasPrefix(line, "sttHost = ")) {
			key, _, _ := strings.Cut(line, " = ")
			lines[i] = key + ` = "0.0.0.0"`
		}
	}
	cfg := filepath.Join(tmp, "open-hosts.toml")
	if err := mustWriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "config", "apply", "--file", cfg, "--restart", "never"); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	two := &e2e{t: t, root: filepath.Join(tmp, "two"), outDir: tmp, key: e.key}
	done := two.cli(nil, "restore", "--yes", "--no-port-probe", file)
	if done.code != 0 {
		t.Fatalf("%s%s", done.stdout, done.stderr)
	}
	if strings.Contains(done.stdout, "backend, reachable from this computer only: 0.0.0.0") || !strings.Contains(done.stdout, "chat backend, reachable from this computer only: 127.0.0.1:") {
		t.Errorf("the report should say where it listens:\n%s", done.stdout)
	}
	if show := two.cli(nil, "config", "show"); !strings.Contains(show.stdout, `host = "0.0.0.0"`) {
		t.Fatalf("this test needs the stored host to still be open:\n%s", show.stdout)
	}
	weights, err := os.ReadFile(filepath.Join(e.root, "models", "llm", "backup-tiny.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mustMkdirAll(filepath.Join(two.root, "models", "llm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(two.root, "models", "llm", "backup-tiny.gguf"), weights, 0o600); err != nil {
		t.Fatal(err)
	}
	two.serve()
	defer two.stop()
	if r := two.chat(backupModel); r.Status != 200 {
		t.Fatalf("chat: %d %s", r.Status, r.Body)
	}
	out, _ := exec.Command(lsof, "-nP", "-a", "-c", "fakebackend", "-iTCP", "-sTCP:LISTEN").CombinedOutput()
	seen := 0
	for _, line := range strings.Split(string(out), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		seen++
		if address := fields[8]; !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "[::1]:") {
			t.Errorf("a backend listens on %s", address)
		}
	}
	if seen == 0 {
		t.Fatalf("no backend found listening:\n%s", out)
	}
}

// crashDatabase leaves a database as a process killed during a large write would: pages changed on
// disk, and a journal that undoes them. It takes a copy's files while its transaction is open.
// crashDatabase leaves a database as a version before write-ahead logging would, killed during a write.
// A root that crashed before it was upgraded has this, and nothing has opened it for writing since.
func crashDatabase(t *testing.T, tmp, dbPath string) {
	t.Helper()
	rollback, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rollback.SetMaxOpenConns(1)
	var mode string
	err = rollback.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&mode)
	_ = rollback.Close() // only the mode was changed, and the query's error says if that failed
	if err != nil || mode != "delete" {
		t.Fatalf("journal mode %q: %v", mode, err)
	}
	work := filepath.Join(tmp, "crash-copy.db")
	body, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(work, body, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+work+"?_pragma=cache_size(8)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE crash_mid_write (x)`); err != nil {
		t.Fatal(err)
	}
	for range 64 {
		if _, err := tx.Exec(`INSERT INTO crash_mid_write VALUES (randomblob(65536))`); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range map[string]string{work: dbPath, work + "-journal": dbPath + "-journal"} {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := mustWriteFile(to, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A gateway killed with a write only in the -wal, in write-ahead logging. SQLite recovers that by itself,
// and what it reads is consistent, so nothing is refused: the write is in the backup, a restore over it
// keeps it, and an ordinary command clears the -wal without saying anything.
func TestAKilledGatewaysWriteIsKeptAndNothingIsRefused(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	created := e.json("keys", "create", "--name", "leaked", "--scopes", "models:read")
	leaked := created["secret"].(string)
	id := created["key"].(map[string]any)["id"].(string)
	older := filepath.Join(tmp, "older"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", older); r.code != 0 {
		t.Fatalf("backup: %s%s", r.stdout, r.stderr)
	}
	e.serve()
	// The gateway holds the database open, so nothing checkpoints this: it stays in the -wal.
	if r := e.cli(nil, "keys", "revoke", id); r.code != 0 {
		t.Fatalf("revoke: %s%s", r.stdout, r.stderr)
	}
	e.kill()
	db := filepath.Join(e.root, "local-base.db")
	if info, err := os.Stat(db + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("no -wal after killing the gateway, so this tests nothing: %v", err)
	}

	for _, args := range [][]string{{"backup", "--dry-run"}, {"restore", "--dry-run", "--replace", "--no-port-probe", older}} {
		r := e.cli(nil, args...)
		if said := r.stdout + r.stderr; r.code != 0 || strings.Contains(said, "never finished") {
			t.Errorf("%v after a kill: %d %s", args, r.code, said)
		}
	}
	file := filepath.Join(tmp, "after"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("backup after a kill: %s%s", r.stdout, r.stderr)
	}

	// The backup holds the revocation: the key is refused on a root restored from it.
	two := &e2e{t: t, root: filepath.Join(tmp, "root-two"), outDir: tmp, key: leaked}
	if r := two.cli(nil, "restore", "--yes", "--no-port-probe", file); r.code != 0 {
		t.Fatalf("restore: %s%s", r.stdout, r.stderr)
	}
	two.serve()
	defer two.stop()
	if r := two.do("GET", "/v1/models", ""); r.Status != 401 {
		t.Errorf("a key revoked just before the kill gets %d from a root restored from the backup", r.Status)
	}

	if r := e.cli(nil, "status"); r.code != 0 || strings.Contains(r.stdout+r.stderr, "never finished") {
		t.Errorf("status: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if info, err := os.Stat(db + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("status left %d bytes in the -wal", info.Size())
	}
}

// After a crash, backup and restore refuse and name a command. That command has to work.
func TestCrashedRootIsRefusedUntilStatusRecoversIt(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	crashDatabase(t, tmp, filepath.Join(e.root, "local-base.db"))
	commands := [][]string{{"backup", "--dry-run"}, {"restore", "--dry-run", "--replace", file}}
	for _, args := range commands {
		r := e.cli(nil, args...)
		if said := r.stdout + r.stderr; r.code == 0 || !strings.Contains(said, "never finished") || !strings.Contains(said, " status") {
			t.Errorf("%v on a crashed root: %d %s", args, r.code, said)
		}
	}
	if r := e.cli(nil, "status"); r.code != 0 {
		t.Fatalf("status: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Lstat(filepath.Join(e.root, "local-base.db-journal")); err == nil {
		t.Error("status left the journal")
	}
	for _, args := range commands {
		if r := e.cli(nil, args...); r.code != 0 {
			t.Errorf("%v after status: %d %s%s", args, r.code, r.stdout, r.stderr)
		}
	}
}

// Every command on a crashed root either refuses and names the one command to run, or opens the
// database for writing, which undoes the write, and shows the real settings. None shows
// defaults as if they were real. A root's key and its settings are how to tell.
func TestCrashedRootEveryCommand(t *testing.T) {
	backupGoOnly(t)
	crashed := func(t *testing.T) (*e2e, string) {
		t.Helper()
		tmp, _ := filepath.EvalSymlinks(scratchDir(t))
		e := backupRoot(t, tmp, "root")
		crashDatabase(t, tmp, filepath.Join(e.root, "local-base.db"))
		return e, tmp
	}
	journal := func(e *e2e) bool {
		_, err := os.Lstat(filepath.Join(e.root, "local-base.db-journal"))
		return err == nil
	}
	for _, args := range [][]string{{"doctor"}, {"--json", "doctor"}, {"config", "show"},
		{"models", "show", "qwen3.5-4b-q4_k_m"}, {"models", "catalog"}, {"models", "recommend"}} {
		t.Run("refuses "+strings.Join(args, " "), func(t *testing.T) {
			e, _ := crashed(t)
			r := e.cli(nil, args...)
			if said := r.stdout + r.stderr; r.code == 0 || !strings.Contains(said, "never finished") || !strings.Contains(said, " status") {
				t.Errorf("%d %.300s", r.code, said)
			}
			if !journal(e) {
				t.Error("a read-only command undid the write")
			}
		})
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status"}, "Service:"},
		{[]string{"keys", "list"}, "backup"},
		{[]string{"models", "list"}, "backup-tiny"},
		{[]string{"access", "show"}, "not configured"},
		{[]string{"backends", "list"}, "built-in"},
		{[]string{"voices", "list"}, "No custom voices"},
	} {
		t.Run("recovers "+strings.Join(c.args, " "), func(t *testing.T) {
			e, _ := crashed(t)
			r := e.cli(nil, c.args...)
			if said := r.stdout + r.stderr; r.code != 0 || !strings.Contains(said, c.want) {
				t.Errorf("%d %.300s", r.code, said)
			}
			if journal(e) {
				t.Error("the journal is still there, so what this printed came from a database it could not trust")
			}
		})
	}
	t.Run("config apply keeps working", func(t *testing.T) {
		e, tmp := crashed(t)
		shown := e.cli(nil, "status")
		if shown.code != 0 {
			t.Fatalf("%s%s", shown.stdout, shown.stderr)
		}
		doc := e.cli(nil, "config", "show")
		if doc.code != 0 {
			t.Fatalf("%s%s", doc.stdout, doc.stderr)
		}
		crashDatabase(t, tmp, filepath.Join(e.root, "local-base.db"))
		file := filepath.Join(tmp, "config.toml")
		mustWriteFile(file, []byte(doc.stdout), 0o600)
		if r := e.cli(nil, "config", "plan", "--file", file); r.code == 0 || !strings.Contains(r.stdout+r.stderr, "never finished") {
			t.Errorf("plan: %d %s%s", r.code, r.stdout, r.stderr)
		}
		if r := e.cli(nil, "config", "apply", "--file", file, "--restart", "never"); r.code != 0 {
			t.Errorf("%d %s%s", r.code, r.stdout, r.stderr)
		}
		if after := e.cli(nil, "config", "show"); after.stdout != doc.stdout {
			t.Errorf("the settings changed:\n%s\nthen:\n%s", doc.stdout, after.stdout)
		}
	})
	t.Run("diagnostics still makes a bundle and says so", func(t *testing.T) {
		e, tmp := crashed(t)
		out := filepath.Join(tmp, "d.zip")
		r := e.cli(nil, "diagnostics", "--output", out)
		if said := r.stdout + r.stderr; r.code != 0 || !strings.Contains(said, "never finished") {
			t.Errorf("%d %.300s", r.code, said)
		}
		zr, err := zip.OpenReader(out)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		manifest, err := zr.Open("manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		defer manifest.Close()
		body, _ := io.ReadAll(manifest)
		if !strings.Contains(string(body), "unfinished_write") {
			t.Errorf("the manifest does not say so: %.300s", body)
		}
		if !journal(e) {
			t.Error("diagnostics undid the write")
		}
	})
}
