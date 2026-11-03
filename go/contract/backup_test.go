package contract

import (
	"archive/zip"
	"crypto/sha256"
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
	"testing"

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
	if running.code == 0 || !strings.Contains(running.stderr+running.stdout, "Stop it first") {
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
	r := target.cli(nil, "restore", "--yes", file)
	if r.code != 0 {
		t.Fatalf("%s%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, fmt.Sprintf("already uses port %d", busy)) || !strings.Contains(r.stderr, "config apply") {
		t.Errorf("a busy port should be named: %s", r.stderr)
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
