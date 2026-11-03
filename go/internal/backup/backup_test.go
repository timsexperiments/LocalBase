package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

const (
	secretHF     = "hf_PLANTED_TOKEN_0001"
	secretOTLP   = "x-planted=PLANTED_OTLP_0002"
	secretOIDC   = "PLANTED_OIDC_0003"
	secretSMTP   = "PLANTED_SMTP_0004"
	secretHeader = "PLANTED_HEADER_0005"
	voiceID      = "voice-0123456789ab"
)

var allSecrets = []string{secretHF, "PLANTED_OTLP_0002", secretOIDC, secretSMTP, secretHeader}

func dir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newRoot makes a root the way a person would have one: settings, a key, a backend, a catalog
// entry, a voice, sign-in, email, credentials, and things that must never travel.
func newRoot(t *testing.T, parent string) (*root.Root, string) {
	t.Helper()
	r, err := root.Init(filepath.Join(parent, "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cat, err := catalog.LoadRoot(r.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.Default(r.Dir, 64, cat)
	cfg.HFToken, cfg.OtelHeaders, cfg.OtelEndpoint = secretHF, secretOTLP, "https://otel.example.com"
	cfg.GatewayHost, cfg.GatewayPort, cfg.Port, cfg.SttPort = "0.0.0.0", 28300, 28301, 28302
	if err := st.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateAPIKey(ctx, "kept", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-1', 'active', 'x', 'x')`,
		`INSERT INTO local_sessions (id, token_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at) VALUES ('s', 'PLANTED_SESSION', 1, 1, 1, 1)`,
		`INSERT INTO local_login_links (token_hash, created_at, expires_at) VALUES ('PLANTED_LINK', 1, 1)`,
		`INSERT INTO model_observed_memory (model_id, artifact_sha256, launch_shape, peak_bytes, method, observed_at) VALUES ('m', 'PLANTED_MEASURE', 's', 1, 'm', 'x')`,
	} {
		if _, err := st.DB.ExecContext(ctx, stmt); err != nil {
			t.Logf("%s: %v", stmt[:40], err)
		}
	}
	clip := []byte("RIFF a planted voice recording")
	sum := sha256.Sum256(clip)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
		VALUES (?, 'Mine', 'mine', ?, ?, 1000, 'x', 'x', 'x', 'x')`, voiceID, hex.EncodeToString(sum[:]), len(clip)); err != nil {
		t.Fatal(err)
	}
	p, _ := voices.Path(r.Dir, voiceID)
	write(t, p, string(clip))
	write(t, filepath.Join(r.Dir, "ui-access.json"), `{"provider":{"kind":"direct","registrations":[
		{"kind":"oidc","id":"google","name":"Google","issuer":"https://accounts.example.com","clientId":"cid","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"`+secretOIDC+`"}},
		{"kind":"oidc","id":"public","name":"Public","issuer":"https://public.example.com","clientId":"cid2","clientAuthentication":{"kind":"none"}}
	]},"origin":"https://localbase.example.com","permissions":["inference:chat"]}`)
	write(t, filepath.Join(r.Dir, "email-delivery.json"), `{"host":"smtp.example.com","port":587,"security":"starttls","authentication":{"kind":"password","username":"u","password":"`+secretSMTP+`"},"from":"a@example.com"}`)
	write(t, filepath.Join(r.Dir, "credentials", "auth-profiles.json"), `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"X-Api-Key":"`+secretHeader+`"}}]}`)
	write(t, filepath.Join(r.Dir, "backends.d", "custom.json"), `{"schema":"localbase.backend/v1","id":"custom","name":"Custom","engine":"fake-engine","adapter":"process-openai/v1","tasks":["chat"],"capabilities":[],"formats":["gguf"],"architectures":{"supported":["x"]},"platforms":[{"os":"darwin","arch":"arm64","accelerator":"cpu"}],"launch":{"command":"/bin/sh","args":["-c","run --port {port}"],"healthPath":"/health","memory":{"weightsFactor":1,"overheadGb":0,"assumption":"none"}}}`)
	entry, _ := json.Marshal(map[string]any{"modelId": "my-model", "displayName": "Mine", "kind": "llm", "provider": "Me", "family": "Mine",
		"version": "1", "size": "1", "quant": "Q4_K_M", "storageGb": 0.01, "source": "https://huggingface.co/me/mine", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts":       []any{map[string]any{"sourcePath": "m.gguf", "filename": "m.gguf", "expectedSizeBytes": 10, "sha256": strings.Repeat("b", 64), "role": "primary"}},
		"inputModalities": []string{"text"}, "outputModalities": []string{"text"}, "contextWindowTokens": 4096, "capabilities": []string{"text-generation"},
		"commercialStatus": "open", "catch": "Test.", "notes": "Test."})
	write(t, filepath.Join(r.Dir, "catalog.d", "my-model.json"), string(entry))
	write(t, filepath.Join(r.Dir, "catalog.d", ".former-built-ins"), "an-old-model\nanother-old-model\n")
	write(t, filepath.Join(r.Dir, "runs", "run_1.json"), `{"reply":"PLANTED_REPLY"}`)
	write(t, filepath.Join(r.Dir, "logs", "events.jsonl"), `{"PLANTED_LOG":1}`+"\n")
	write(t, filepath.Join(r.Dir, "models", "llm", "a.gguf"), "PLANTED_MODEL")
	write(t, filepath.Join(r.Dir, "runtime", "ui-csrf.key"), "PLANTED_CSRF")
	write(t, filepath.Join(r.Dir, "service", "manifest.json"), `{"token":"PLANTED_SERVICE"}`)
	return r, secret
}

func create(t *testing.T, r *root.Root, exclude bool) *Result {
	t.Helper()
	res, err := Create(context.Background(), Options{Root: r, Output: filepath.Join(dir(t), "b"+Extension), ExcludeSecrets: exclude, Version: "9.9.9", Now: time.Unix(1_800_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// entry is one tar entry of a backup, for reading one and for crafting a bad one.
type entry struct {
	header *tar.Header
	body   []byte
}

func read(t *testing.T, path string) []entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{h, b})
	}
}

func pack(t *testing.T, path string, entries []entry) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := *e.header
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func manifestOf(t *testing.T, entries []entry) *Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(entries[0].body, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func setManifest(t *testing.T, entries []entry, m *Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].body = b
}

// reseal makes a manifest match its entries again, as anyone crafting a file would.
func reseal(t *testing.T, entries []entry) {
	t.Helper()
	m := manifestOf(t, entries)
	m.Files = nil
	for _, e := range entries[1:] {
		if e.header.Typeflag != tar.TypeReg || !strings.HasPrefix(e.header.Name, filesPrefix) {
			continue
		}
		sum := sha256.Sum256(e.body)
		m.Files = append(m.Files, File{Path: strings.TrimPrefix(e.header.Name, filesPrefix), Size: int64(len(e.body)), SHA256: hex.EncodeToString(sum[:])})
	}
	setManifest(t, entries, m)
}

// editDB runs statements on the backup's database, and reseals it.
func editDB(t *testing.T, entries []entry, statements ...string) {
	t.Helper()
	p := filepath.Join(dir(t), "db")
	for i := range entries {
		if entries[i].header.Name != filesPrefix+"local-base.db" {
			continue
		}
		if err := os.WriteFile(p, entries[i].body, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", sqliteDSN(p, ""))
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		db.Close()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		entries[i].body = b
		reseal(t, entries)
		return
	}
	t.Fatal("no database in the backup")
}

// tree describes every path under a directory, by mode and content. A refusal must leave it the same.
func tree(t *testing.T, base string, skip ...string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(base, p)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+string(filepath.Separator)) {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := rel + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func noLeftovers(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), temporaryPrefix) {
			t.Errorf("a working directory was left: %s", e.Name())
		}
	}
}

func restore(t *testing.T, file, target string, replace bool) (*Report, *Done) {
	t.Helper()
	s, err := Stage(context.Background(), file, target, replace)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rep := s.Report
	done, err := s.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rep, done
}

// --- what a backup holds --------------------------------------------------------------------

func TestNoNetworkImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "net" || path == "net/http" || path == "net/rpc" || path == "net/smtp" {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

func TestBackupHoldsSecretsOrNot(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	raw := func(res *Result) []byte {
		var out []byte
		for _, e := range read(t, res.Output) {
			out = append(out, e.body...)
		}
		return out
	}
	full := create(t, r, false)
	body := raw(full)
	// This proves the scan can find them, so the next one means something.
	for _, s := range allSecrets {
		if !bytes.Contains(body, []byte(s)) {
			t.Errorf("a full backup should hold %s", s)
		}
	}
	if full.Manifest.Secrets != "included" || !strings.Contains(full.Warning, "not encrypted") || !strings.Contains(full.Warning, "Hugging Face token") {
		t.Errorf("full: %s %q", full.Manifest.Secrets, full.Warning)
	}

	bare := create(t, r, true)
	body = raw(bare)
	for _, s := range allSecrets {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("--exclude-secrets still holds %s", s)
		}
	}
	if bare.Manifest.Secrets != "excluded" || len(bare.Manifest.SecretNames) != 0 {
		t.Errorf("manifest: %+v", bare.Manifest)
	}
	if !strings.Contains(bare.Warning, "not encrypted") || !strings.Contains(bare.Warning, "not anonymised") {
		t.Errorf("warning: %q", bare.Warning)
	}
	names := map[string]string{}
	for _, s := range bare.Manifest.SecretsRemoved {
		names[s.Name] = s.Restore
	}
	for name, command := range map[string]string{
		"Hugging Face token":           "configure --hf-token",
		"OTLP headers":                 "configure --otel-headers",
		"OIDC client secret (google)":  "access oidc add --id google",
		"SMTP password":                "access email configure",
		"download credential (vendor)": "auth-profiles add --id vendor --origin https://vendor.example.com --header-env X-Api-Key=",
	} {
		if !strings.Contains(names[name], command) {
			t.Errorf("%s: %q, want %q", name, names[name], command)
		}
	}
	// The public registration has no secret, so it stays.
	access := map[string][]byte{}
	for _, e := range read(t, bare.Output) {
		access[e.header.Name] = e.body
	}
	if !bytes.Contains(access[filesPrefix+"ui-access.json"], []byte(`"public"`)) || bytes.Contains(access[filesPrefix+"ui-access.json"], []byte(`"google"`)) {
		t.Errorf("ui-access: %s", access[filesPrefix+"ui-access.json"])
	}
	for _, gone := range []string{"email-delivery.json", "credentials/auth-profiles.json"} {
		if _, there := access[filesPrefix+gone]; there {
			t.Errorf("%s should be left out", gone)
		}
	}
}

func TestRestoreWithoutSecretsPassesEveryCheck(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, true)
	target := filepath.Join(dir(t), "new")
	rep, _ := restore(t, res.Output, target, false)
	if len(rep.Manifest.SecretsRemoved) == 0 {
		t.Error("the report should list what to put back")
	}
	if _, err := root.Open(target); err != nil {
		t.Error(err)
	}
	// With only secret-bearing registrations, the file goes whole, and the root still opens.
	r2, _ := newRoot(t, dir(t))
	write(t, filepath.Join(r2.Dir, "ui-access.json"), `{"provider":{"kind":"direct","registrations":[{"kind":"oidc","id":"google","name":"G","issuer":"https://a.example.com","clientId":"c","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"`+secretOIDC+`"}}]},"origin":"https://l.example.com","permissions":[]}`)
	res = create(t, r2, true)
	target = filepath.Join(dir(t), "new")
	restore(t, res.Output, target, false)
	if _, err := os.Stat(filepath.Join(target, "ui-access.json")); !os.IsNotExist(err) {
		t.Errorf("ui-access.json: %v", err)
	}
}

func TestBackupNeverHoldsWhatStaysBehind(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	write(t, filepath.Join(r.Dir, ".before-restore-20260101-000000", "ui-access.json"), "PLANTED_OLD")
	res := create(t, r, false)
	var body []byte
	for _, e := range read(t, res.Output) {
		body = append(body, e.body...)
	}
	for _, s := range []string{"PLANTED_REPLY", "PLANTED_LOG", "PLANTED_MODEL", "PLANTED_CSRF", "PLANTED_SERVICE", "PLANTED_OLD", "PLANTED_SESSION", "PLANTED_LINK", "PLANTED_MEASURE"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("the backup holds %s", s)
		}
	}
	// And it holds no path of this computer, which would say whose it is.
	if bytes.Contains(body, []byte(r.Dir)) {
		t.Error("the backup names the root's path")
	}
	listed := map[string]NotIncluded{}
	for _, n := range res.Manifest.NotIncluded {
		listed[n.What] = n
	}
	for _, want := range []string{"models/", "runs/", "logs/", "runtime/", "service/", ".before-restore-20260101-000000"} {
		if listed[want].Why == "" {
			t.Errorf("%s is left out silently: %v", want, listed)
		}
	}
	if listed["models/"].Bytes == 0 {
		t.Error("the size of what is left out should be listed")
	}
}

// Removing rows frees pages, and a freed page keeps its bytes until something reuses it. The
// second VACUUM INTO is what stops a backup carrying what it was meant to leave out.
func TestBackupLeavesNoFreePages(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := tx.Exec(`INSERT INTO local_sessions (id, token_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at) VALUES (?, ?, 1, 1, 1, 1)`,
			fmt.Sprintf("row-%d", i), fmt.Sprintf("PLANTED_FREE_PAGE_%d_%s", i, strings.Repeat("x", 200))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	res := create(t, r, true)
	for _, e := range read(t, res.Output) {
		if e.header.Name != filesPrefix+"local-base.db" {
			continue
		}
		if bytes.Contains(e.body, []byte("PLANTED_FREE_PAGE_")) {
			t.Error("the bytes of a row it left out are still in the file")
		}
		p := filepath.Join(dir(t), "db")
		if err := os.WriteFile(p, e.body, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := openReadOnly(p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var free int
		if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
			t.Fatal(err)
		}
		if free != 0 {
			t.Errorf("%d free pages", free)
		}
	}
}

// The first run on a real root found a file backup kept and restore refused, so a backup that could
// never be restored. This holds every kind of thing a real root holds, by the list from that run.
func TestRealRootRoundTrip(t *testing.T) {
	r, secret := newRoot(t, dir(t))
	catalogDir := filepath.Join(r.Dir, "catalog.d")
	// A patch of a built-in entry is a real catalog.d file, and isn't a whole entry.
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	builtin := cat.Models[0].ModelID
	write(t, filepath.Join(catalogDir, builtin+".json"), `{"modelId":"`+builtin+`","notes":"My note."}`)
	// Entries the gateway reads past, and things nothing reads.
	write(t, filepath.Join(catalogDir, "broken.json"), `{not json`)
	write(t, filepath.Join(catalogDir, "incomplete.json"), `{"modelId":"nothing-else-here"}`)
	write(t, filepath.Join(catalogDir, "notes.txt"), "mine")
	write(t, filepath.Join(catalogDir, ".DS_Store"), "x")
	write(t, filepath.Join(catalogDir, "half.json.123.tmp"), "x")
	write(t, filepath.Join(r.Dir, "backends.d", "broken.json"), `{"schema":"nope"}`)
	write(t, filepath.Join(r.Dir, "backends.d", "wrong-name.json"), `{"schema":"localbase.backend/v1","id":"another","name":"A","engine":"e","adapter":"process-openai/v1","tasks":["chat"],"capabilities":[],"formats":["gguf"],"architectures":{},"platforms":[{"os":"linux","arch":"arm64","accelerator":"cpu"}],"launch":{"command":"/bin/sh","args":["{port}"],"memory":{"weightsFactor":1,"overheadGb":0,"assumption":"none"}}}`)
	write(t, filepath.Join(r.Dir, "backends.d", "README"), "mine")
	write(t, filepath.Join(r.Dir, "voices", "stray.txt"), "mine")
	write(t, filepath.Join(r.Dir, "catalog-feed.json"), `{"url":"https://feed.example.com"}`)
	write(t, filepath.Join(r.Dir, "cache", "x"), "x")
	write(t, filepath.Join(r.Dir, "bin", "runtimes", "llama-server"), "x")
	write(t, filepath.Join(r.Dir, "logs", "service-stderr.log"), "x")

	// A dry run gives the answer a real run gives.
	if _, err := Create(context.Background(), Options{Root: r, DryRun: true, Output: filepath.Join(dir(t), "b"+Extension)}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	res := create(t, r, false)
	left := map[string]NotIncluded{}
	for _, n := range res.Manifest.NotIncluded {
		left[n.What] = n
	}
	for _, what := range []string{"catalog.d/broken.json", "catalog.d/incomplete.json", "backends.d/broken.json", "backends.d/wrong-name.json"} {
		if !strings.Contains(left[what].Why, "ignores it") {
			t.Errorf("%s should be left out because it is ignored: %+v", what, left[what])
		}
	}
	for _, what := range []string{"catalog.d/notes.txt", "catalog.d/.DS_Store", "backends.d/README", "voices/stray.txt"} {
		if !left[what].Unknown {
			t.Errorf("%s should be listed as unknown: %+v", what, left[what])
		}
	}
	if left["catalog.d/half.json.123.tmp"].Why == "" {
		t.Error("a temporary file should be listed")
	}
	held := map[string]bool{}
	for _, f := range res.Manifest.Files {
		held[f.Path] = true
	}
	for _, want := range []string{"catalog.d/.former-built-ins", "catalog.d/my-model.json", "catalog.d/" + builtin + ".json", "backends.d/custom.json", "voices/" + voiceID + ".wav", "ui-access.json", "email-delivery.json", "credentials/auth-profiles.json", "local-base.db"} {
		if !held[want] {
			t.Errorf("%s should be in the backup: %v", want, held)
		}
	}

	target := filepath.Join(dir(t), "restored")
	restore(t, res.Output, target, false)
	if b, err := os.ReadFile(filepath.Join(target, "catalog.d", ".former-built-ins")); err != nil || string(b) != "an-old-model\nanother-old-model\n" {
		t.Errorf("the adoption log: %q %v", b, err)
	}
	if _, err := root.Open(target); err != nil {
		t.Fatal(err)
	}
	restored, err := catalog.Load(filepath.Join(target, "catalog.d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Diagnostics) != 0 {
		t.Errorf("the restored catalog has problems: %+v", restored.Diagnostics)
	}
	if s := restored.ByID(builtin); s == nil || !strings.Contains(s.Notes, "My note.") {
		t.Errorf("the patch should survive: %+v", s)
	}
	st, err := store.Open(filepath.Join(target, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if k, err := st.ResolveAPIKey(context.Background(), secret); err != nil || k == nil {
		t.Errorf("the key should work: %v %v", k, err)
	}
}

// Whatever else goes wrong, a backup its own restore would refuse is never kept or reported.
func TestBackupRefusesWhatItCouldNotRestore(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	// The database names a recording the root no longer has.
	p, _ := voices.Path(r.Dir, voiceID)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	places := dir(t)
	before := tree(t, filepath.Dir(r.Dir), filepath.Base(r.Dir))
	for _, dry := range []bool{false, true} {
		out := filepath.Join(places, "b"+Extension)
		res, err := Create(context.Background(), Options{Root: r, Output: out, DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "would not restore") || !strings.Contains(err.Error(), voiceID) {
			t.Fatalf("dry run %v: %v %+v", dry, err, res)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("dry run %v left a file: %v", dry, err)
		}
		entries, _ := os.ReadDir(places)
		if len(entries) != 0 {
			t.Errorf("dry run %v left %v", dry, entries)
		}
		if after := tree(t, filepath.Dir(r.Dir), filepath.Base(r.Dir)); after != before {
			t.Errorf("dry run %v left something beside the root:\n%s", dry, after)
		}
	}
}

// A killed run can't clean up. What it left is named by doctor, and removed by the next run.
func TestAbandonedWorkingDirectories(t *testing.T) {
	parent := dir(t)
	r, _ := newRoot(t, parent)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	gone := dead.Process.Pid
	left := filepath.Join(parent, fmt.Sprintf("%sbackup-%d-123", temporaryPrefix, gone))
	write(t, filepath.Join(left, "content", "ui-access.json"), secretOIDC)
	stillWorking := filepath.Join(parent, fmt.Sprintf("%sbackup-%d-456", temporaryPrefix, os.Getpid()))
	write(t, filepath.Join(stillWorking, "x"), "x")
	mine := filepath.Join(parent, temporaryPrefix+"something-else")
	write(t, filepath.Join(mine, "x"), "x")

	if got := Abandoned(r.Dir); len(got) != 1 || got[0] != left {
		t.Fatalf("abandoned: %v", got)
	}
	if _, err := os.Stat(left); err != nil {
		t.Error("looking must not remove it")
	}
	res, err := Create(context.Background(), Options{Root: r, DryRun: true, Output: filepath.Join(dir(t), "b"+Extension)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Cautions, " "), left) {
		t.Errorf("it should say what it removed: %v", res.Cautions)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("it should be gone: %v", err)
	}
	for _, kept := range []string{stillWorking, mine} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s isn't its to remove: %v", kept, err)
		}
	}
}

func TestWorkingDirectoriesArePrivate(t *testing.T) {
	was := syscall.Umask(0)
	defer syscall.Umask(was)
	d := dir(t)
	path, err := newTemporary(d, "backup")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("%o", info.Mode().Perm())
	}
	if !temporaryRe.MatchString(filepath.Base(path)) {
		t.Errorf("the name isn't obvious, or isn't recognised: %s", path)
	}
}

func TestRestoreRefusesABadAdoptionLog(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	for i := range e {
		if e[i].header.Name == filesPrefix+formerLog {
			e[i].body = []byte("not an id, this is a sentence\n")
		}
	}
	reseal(t, e)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	if _, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false); err == nil || !strings.Contains(err.Error(), "not a list of ids") {
		t.Fatal(err)
	}
}

func TestBackupListsWhatItDoesNotKnow(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	write(t, filepath.Join(r.Dir, "something-new", "file"), "x")
	write(t, filepath.Join(r.Dir, "credentials", "another.json"), "{}")
	res, err := Create(context.Background(), Options{Root: r, DryRun: true, Output: filepath.Join(dir(t), "b"+Extension)})
	if err != nil {
		t.Fatal(err)
	}
	unknown := map[string]bool{}
	for _, n := range res.Manifest.NotIncluded {
		if n.Unknown {
			unknown[n.What] = true
		}
	}
	if !unknown["something-new"] || !unknown["credentials/another.json"] {
		t.Errorf("unknown: %v", unknown)
	}
}

func TestBackupModes(t *testing.T) {
	for _, mask := range []int{0, 0o077} {
		t.Run(fmt.Sprintf("umask %04o", mask), func(t *testing.T) {
			was := syscall.Umask(mask)
			defer syscall.Umask(was)
			r, _ := newRoot(t, dir(t))
			res, err := Create(context.Background(), Options{Root: r, Now: time.Unix(1_800_000_000, 0)})
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(res.Output)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("the file is %o", info.Mode().Perm())
			}
			parent, err := os.Stat(filepath.Dir(res.Output))
			if err != nil {
				t.Fatal(err)
			}
			if parent.Mode().Perm() != 0o700 || filepath.Dir(res.Output) != DefaultDir(r.Dir) {
				t.Errorf("the directory %s is %o", filepath.Dir(res.Output), parent.Mode().Perm())
			}
			entries, _ := os.ReadDir(filepath.Dir(res.Output))
			if len(entries) != 1 {
				t.Errorf("left beside it: %v", entries)
			}
		})
	}
}

func TestBackupRefusesPlaces(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	ctx := context.Background()
	taken := filepath.Join(dir(t), "taken"+Extension)
	write(t, taken, "keep me")
	if _, err := Create(ctx, Options{Root: r, Output: taken}); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("an existing file: %v", err)
	}
	if b, _ := os.ReadFile(taken); string(b) != "keep me" {
		t.Error("it was overwritten")
	}
	if _, err := Create(ctx, Options{Root: r, Output: filepath.Join(r.Dir, "b"+Extension)}); err == nil || !strings.Contains(err.Error(), "inside the root") {
		t.Errorf("inside the root: %v", err)
	}
	for _, mode := range []fs.FileMode{0o777, 0o770, 0o1777} {
		open := dir(t)
		if err := os.Chmod(open, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Create(ctx, Options{Root: r, Output: filepath.Join(open, "b"+Extension)}); err == nil || !strings.Contains(err.Error(), "can change") {
			t.Errorf("a %o directory: %v", mode, err)
		}
		os.Chmod(open, 0o700)
	}
	readable := dir(t)
	if err := os.Chmod(readable, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Create(ctx, Options{Root: r, Output: filepath.Join(readable, "b"+Extension)})
	if err != nil || len(res.Cautions) == 0 {
		t.Errorf("a directory others can read gets a caution: %v %v", res, err)
	}
	loose := DefaultDir(r.Dir)
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, Options{Root: r}); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("a loose default directory: %v", err)
	}
}

func TestBackupRefusesPlantedLinks(t *testing.T) {
	outside := filepath.Join(dir(t), "outside")
	write(t, outside, "PLANTED_OUTSIDE")
	for _, rel := range []string{"catalog.d/link.json", "backends.d/link.json", "voices/voice-000000000000.wav"} {
		r, _ := newRoot(t, dir(t))
		if err := os.Symlink(outside, filepath.Join(r.Dir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		_, err := Create(context.Background(), Options{Root: r, Output: filepath.Join(dir(t), "b"+Extension)})
		var unsafe *obs.UnsafeFileError
		if err == nil || !strings.Contains(err.Error(), "link") {
			t.Errorf("%s: %v (%T)", rel, err, unsafe)
		}
	}
}

func TestBackupWhileWriting(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			st.DB.ExecContext(ctx, `INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('test.counter', ?, 'x')`, strconv.Itoa(i))
		}
	}()
	defer func() { cancel(); wg.Wait() }()
	for i := 0; i < 3; i++ {
		res := create(t, r, false)
		restore(t, res.Output, filepath.Join(dir(t), "new"), false)
	}
}

// --- what a restore refuses ---------------------------------------------------------------------

func TestRestoreRefusesBadFiles(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	type edit func(t *testing.T, e []entry) []entry
	regular := func(name string, body string) entry {
		return entry{&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600}, []byte(body)}
	}
	cases := map[string]struct {
		edit edit
		want string
	}{
		"a wrong hash": {func(t *testing.T, e []entry) []entry {
			e[1].body = append([]byte("x"), e[1].body[1:]...)
			return e
		}, "checksum"},
		"an entry larger than the manifest says": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Files[0].Size--
			setManifest(t, e, m)
			return e
		}, "size the manifest says"},
		"an extra entry": {func(t *testing.T, e []entry) []entry {
			return append(e, regular(filesPrefix+"catalog.d/extra.json", "{}"))
		}, "does not list"},
		"a duplicate entry": {func(t *testing.T, e []entry) []entry { return append(e, e[1]) }, "twice"},
		"a missing entry":   {func(t *testing.T, e []entry) []entry { return e[:len(e)-1] }, "is missing"},
		"no manifest first": {func(t *testing.T, e []entry) []entry { return e[1:] }, "does not start with a manifest"},
		"a link": {func(t *testing.T, e []entry) []entry {
			return append(e, entry{&tar.Header{Name: filesPrefix + "catalog.d/l.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}, nil})
		}, "not a plain file"},
		"a device": {func(t *testing.T, e []entry) []entry {
			return append(e, entry{&tar.Header{Name: filesPrefix + "catalog.d/d", Typeflag: tar.TypeChar, Mode: 0o600}, nil})
		}, "not a plain file"},
		"a path out of the root": {func(t *testing.T, e []entry) []entry {
			e = append(e, regular(filesPrefix+"../../escape", "x"))
			reseal(t, e)
			return e
		}, "can't hold"},
		"an absolute path": {func(t *testing.T, e []entry) []entry {
			e = append(e, regular(filesPrefix+"/etc/escape", "x"))
			reseal(t, e)
			return e
		}, "can't hold"},
		"a dotfile that isn't the adoption log": {func(t *testing.T, e []entry) []entry {
			e = append(e, regular(filesPrefix+"catalog.d/.hidden", "x"))
			reseal(t, e)
			return e
		}, "can't hold"},
		"a program in a catalog directory": {func(t *testing.T, e []entry) []entry {
			e = append(e, regular(filesPrefix+"backends.d/run.sh", "#!/bin/sh"))
			reseal(t, e)
			return e
		}, "can't hold"},
		"a file not on the list": {func(t *testing.T, e []entry) []entry {
			e = append(e, regular(filesPrefix+"bin/llama-server", "x"))
			reseal(t, e)
			return e
		}, "can't hold"},
		"an entry outside files": {func(t *testing.T, e []entry) []entry { return append(e, regular("elsewhere", "x")) }, "should not"},
		"a newer format": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Format = "localbase.backup/v2"
			setManifest(t, e, m)
			return e
		}, "Upgrade LocalBase"},
		"an unknown format": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Format = "something else"
			setManifest(t, e, m)
			return e
		}, "unknown format"},
		"an unknown manifest field": {func(t *testing.T, e []entry) []entry {
			e[0].body = append(e[0].body[:len(e[0].body)-1], []byte(`,"extra":1}`)...)
			return e
		}, "can't be read"},
		"a file over the limit": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Files[0].Size = maxFileBytes + 1
			setManifest(t, e, m)
			return e
		}, "size a backup can't hold"},
		"a total over the limit": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			for i := 0; i < 3; i++ {
				m.Files = append(m.Files, File{Path: fmt.Sprintf("catalog.d/big%d.json", i), Size: maxFileBytes, SHA256: strings.Repeat("a", 64)})
			}
			setManifest(t, e, m)
			return e
		}, "larger than a backup can be"},
		"too many files": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			for i := 0; i <= maxEntries; i++ {
				m.Files = append(m.Files, File{Path: fmt.Sprintf("catalog.d/f%d.json", i), SHA256: strings.Repeat("a", 64)})
			}
			setManifest(t, e, m)
			return e
		}, "more files"},
		"a path too long": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Files = append(m.Files, File{Path: "catalog.d/" + strings.Repeat("a", maxPathLength), SHA256: strings.Repeat("a", 64)})
			setManifest(t, e, m)
			return e
		}, "can't hold"},
		"no usable checksum": {func(t *testing.T, e []entry) []entry {
			m := manifestOf(t, e)
			m.Files[0].SHA256 = "nope"
			setManifest(t, e, m)
			return e
		}, "checksum"},
		"no database": {func(t *testing.T, e []entry) []entry {
			var out []entry
			for _, x := range e {
				if x.header.Name != filesPrefix+"local-base.db" {
					out = append(out, x)
				}
			}
			reseal(t, out)
			return out
		}, "no database"},
		"a corrupt database": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if e[i].header.Name == filesPrefix+"local-base.db" {
					for j := 4096; j < 12288 && j < len(e[i].body); j++ {
						e[i].body[j] = 0xFF
					}
				}
			}
			reseal(t, e)
			return e
		}, "database"},
		"an invalid ui-access.json": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if e[i].header.Name == filesPrefix+"ui-access.json" {
					e[i].body = []byte(`{"provider":{"kind":"mystery"}}`)
				}
			}
			reseal(t, e)
			return e
		}, "ui-access.json"},
		"an invalid backend": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if e[i].header.Name == filesPrefix+"backends.d/custom.json" {
					e[i].body = []byte(`{"schema":"nope"}`)
				}
			}
			reseal(t, e)
			return e
		}, "backend"},
		"a backend named like a built-in": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if e[i].header.Name == filesPrefix+"backends.d/custom.json" {
					e[i].body = bytes.Replace(e[i].body, []byte(`"id":"custom"`), []byte(`"id":"other"`), 1)
				}
			}
			reseal(t, e)
			return e
		}, "should be named"},
		"an invalid catalog entry": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if e[i].header.Name == filesPrefix+"catalog.d/my-model.json" {
					e[i].body = []byte(`{"modelId":1}`)
				}
			}
			reseal(t, e)
			return e
		}, "catalog"},
		"a voice that doesn't match": {func(t *testing.T, e []entry) []entry {
			for i := range e {
				if strings.HasPrefix(e[i].header.Name, filesPrefix+"voices/") {
					e[i].body = []byte("another recording")
				}
			}
			reseal(t, e)
			return e
		}, "voice"},
		"a bad imported model": {func(t *testing.T, e []entry) []entry {
			editDB(t, e, `INSERT INTO imported_models (id, definition, created_at, updated_at) VALUES ('a', '{"id":"b"}', 'x', 'x')`)
			return e
		}, "imported model"},
		"an imported model naming a path out": {func(t *testing.T, e []entry) []entry {
			editDB(t, e, `INSERT INTO imported_models (id, definition, created_at, updated_at) VALUES ('a', '{"id":"a","tasks":["chat"],"name":"a","variants":[{"id":"v","artifacts":[{"filename":"../../../evil","role":"primary","required":true,"source":{"kind":"url","url":"https://example.com/x"}}]}]}', 'x', 'x')`)
			return e
		}, "imported model"},
		"an invalid access policy": {func(t *testing.T, e []entry) []entry {
			editDB(t, e,
				`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r', ' not a valid name ', '', 'x', 'x')`,
				`INSERT OR REPLACE INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'r', 'x')`)
			return e
		}, "access policy"},
		"a newer migration level": {func(t *testing.T, e []entry) []entry {
			editDB(t, e, `INSERT INTO "__drizzle_migrations" (hash, created_at) VALUES ('future', 99999999999999)`)
			return e
		}, "newer LocalBase"},
		"a newer Go schema": {func(t *testing.T, e []entry) []entry {
			editDB(t, e, `UPDATE go_schema SET version = 99`)
			return e
		}, "newer LocalBase"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			parent := dir(t)
			target := filepath.Join(parent, "target")
			write(t, filepath.Join(target, ".placeholder"), "")
			os.Remove(filepath.Join(target, ".placeholder"))
			before := tree(t, parent)
			crafted := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, crafted, tc.edit(t, read(t, good)))
			_, err := Stage(context.Background(), crafted, target, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if after := tree(t, parent); after != before {
				t.Errorf("the target changed:\n%s\nwas:\n%s", after, before)
			}
			noLeftovers(t, parent)
		})
	}

	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	refuseBytes := func(name string, b []byte, want string) {
		t.Run(name, func(t *testing.T) {
			parent := dir(t)
			crafted := filepath.Join(dir(t), "crafted"+Extension)
			if err := os.WriteFile(crafted, b, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Stage(context.Background(), crafted, filepath.Join(parent, "target"), false)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
			noLeftovers(t, parent)
		})
	}
	refuseBytes("a truncated file", raw[:len(raw)/2], "damaged")
	refuseBytes("not a backup", []byte("hello"), "not a LocalBase backup")
	refuseBytes("data after the end", append(append([]byte{}, raw...), "more"...), "after its end")

	// Inside the tar, past its end.
	t.Run("data after the tar", func(t *testing.T) {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for _, e := range read(t, good) {
			h := *e.header
			tw.WriteHeader(&h)
			tw.Write(e.body)
		}
		tw.Close()
		gz.Write([]byte("hidden after the end of the tar"))
		gz.Close()
		parent := dir(t)
		crafted := filepath.Join(dir(t), "crafted"+Extension)
		if err := os.WriteFile(crafted, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(context.Background(), crafted, filepath.Join(parent, "target"), false); err == nil || !strings.Contains(err.Error(), "after its end") {
			t.Fatal(err)
		}
	})

	t.Run("a sparse entry", func(t *testing.T) {
		e := read(t, good)
		e = append(e, entry{&tar.Header{Name: filesPrefix + "catalog.d/s.json", Typeflag: tar.TypeReg, Mode: 0o600, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"GNU.sparse.major": "1"}}, []byte("{}")})
		reseal(t, e)
		parent := dir(t)
		crafted := filepath.Join(dir(t), "crafted"+Extension)
		pack(t, crafted, e)
		if _, err := Stage(context.Background(), crafted, filepath.Join(parent, "target"), false); err == nil {
			t.Fatal("a sparse entry was accepted")
		}
	})

	t.Run("a file others can change", func(t *testing.T) {
		loose := filepath.Join(dir(t), "loose"+Extension)
		if err := os.WriteFile(loose, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []fs.FileMode{0o666, 0o620, 0o602} {
			os.Chmod(loose, mode)
			if _, err := Stage(context.Background(), loose, filepath.Join(dir(t), "target"), false); err == nil || !strings.Contains(err.Error(), "can change") {
				t.Errorf("%o: %v", mode, err)
			}
		}
	})

	t.Run("a link to a file", func(t *testing.T) {
		link := filepath.Join(dir(t), "link"+Extension)
		if err := os.Symlink(good, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(context.Background(), link, filepath.Join(dir(t), "target"), false); err == nil || !strings.Contains(err.Error(), "link") {
			t.Error(err)
		}
	})
}

func TestRestoreRefusesCraftedSchema(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	for name, stmt := range map[string]string{
		"a trigger":                       `CREATE TRIGGER evil AFTER UPDATE ON auth_roles BEGIN INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('evil', 'active', 'x', 'x'); END`,
		"a view":                          `CREATE VIEW evil AS SELECT * FROM api_keys`,
		"an extra table":                  `CREATE TABLE evil (id TEXT)`,
		"an extra column":                 `ALTER TABLE api_keys ADD COLUMN evil TEXT`,
		"an extra index":                  `CREATE INDEX evil ON api_keys (name)`,
		"a generated column":              `ALTER TABLE api_keys ADD COLUMN evil TEXT GENERATED ALWAYS AS (name) VIRTUAL`,
		"a trigger on SQLite's own table": `CREATE TRIGGER evil AFTER INSERT ON go_settings BEGIN DELETE FROM api_keys; END`,
	} {
		t.Run(name, func(t *testing.T) {
			e := read(t, good)
			editDB(t, e, stmt)
			crafted := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, crafted, e)
			parent := dir(t)
			_, err := Stage(context.Background(), crafted, filepath.Join(parent, "target"), false)
			if err == nil || !strings.Contains(err.Error(), "does not make") {
				t.Fatalf("got %v", err)
			}
			if !strings.Contains(err.Error(), "evil") {
				t.Errorf("it should name what it found: %v", err)
			}
			noLeftovers(t, parent)
		})
	}
}

// A table a migration makes must be there. A Go table may not be: those are added without raising a
// version, so a real backup from before one lacks it, and opening the database creates it.
func TestRestoreSchemaMissingTables(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, true).Output
	for _, table := range store.GoTables() {
		t.Run("a backup from before "+table, func(t *testing.T) {
			e := read(t, good)
			editDB(t, e, `DROP TABLE `+table)
			crafted := filepath.Join(dir(t), "older"+Extension)
			pack(t, crafted, e)
			target := filepath.Join(dir(t), "target")
			restore(t, crafted, target, false)
			db, err := openReadOnly(filepath.Join(target, "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !hasTable(context.Background(), db, table) {
				t.Errorf("opening it should have made %s", table)
			}
		})
	}
	for name, stmt := range map[string]string{
		"a table a migration makes":  `DROP TABLE auth_role_permissions`,
		"a column a migration makes": `ALTER TABLE api_keys DROP COLUMN revoked_at`,
	} {
		t.Run(name, func(t *testing.T) {
			e := read(t, good)
			editDB(t, e, stmt)
			crafted := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, crafted, e)
			if _, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false); err == nil || !strings.Contains(err.Error(), "is missing") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A real older backup must not be refused: the check compares with what this build makes at the
// backup's own level, so every level has to pass, and migrate on open.
func TestRestoreAcceptsEveryMigrationLevel(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, true).Output
	known, err := store.MigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	for level := 0; level <= known; level++ {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			p := filepath.Join(dir(t), "older.db")
			db, err := sql.Open("sqlite", sqliteDSN(p, ""))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			if err := store.CreateSchemaAt(context.Background(), db, level); err != nil {
				t.Fatal(err)
			}
			db.Close()
			body, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var e []entry
			for _, x := range read(t, good) {
				switch {
				case x.header.Name == filesPrefix+"local-base.db":
					x.body = body
				case strings.HasPrefix(x.header.Name, filesPrefix+"voices/"):
					continue // this empty database has no row for it
				case x.header.Name != manifestEntry && strings.HasPrefix(x.header.Name, filesPrefix) && !strings.HasSuffix(x.header.Name, ".db"):
				}
				e = append(e, x)
			}
			reseal(t, e)
			crafted := filepath.Join(dir(t), "older"+Extension)
			pack(t, crafted, e)
			s, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false)
			if err != nil {
				t.Fatalf("level %d: %v", level, err)
			}
			s.Discard()
		})
	}
}

func TestShapeOfAMigratedDatabaseIsExpected(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	db, err := openReadOnly(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	shape, err := store.ReadShape(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	known, _ := store.MigrationCount()
	expected, err := store.ExpectedShape(ctx, known)
	if err != nil {
		t.Fatal(err)
	}
	if bad := shape.Unexpected(expected); len(bad) > 0 {
		t.Errorf("a real database differs from what is expected: %v", bad)
	}
	if len(shape.Tables) < 10 {
		t.Errorf("the comparison saw only %d tables", len(shape.Tables))
	}
	if !strings.Contains(strings.Join(tableNames(shape), " "), "api_keys") {
		t.Errorf("tables: %v", tableNames(shape))
	}
}

func tableNames(s *store.Shape) []string {
	var out []string
	for name := range s.Tables {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestRestoreDeletesLeftOutRows(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e,
		`INSERT INTO local_sessions (id, token_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at) VALUES ('crafted', 'crafted', 1, 9999999999, 9999999999, 9999999999)`,
		`INSERT INTO local_login_links (token_hash, created_at, expires_at) VALUES ('crafted', 1, 9999999999)`,
		`INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('access.first-run-link-shown', '1', 'x')`,
		`INSERT INTO model_observed_memory (model_id, artifact_sha256, launch_shape, peak_bytes, method, observed_at) VALUES ('m', 'crafted', 's', 1, 'm', 'x')`)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	target := filepath.Join(dir(t), "target")
	restore(t, crafted, target, false)
	db, err := openReadOnly(filepath.Join(target, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range rowsLeftOut {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s holds %d rows after a restore", table, n)
		}
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM go_settings WHERE key = 'access.first-run-link-shown'`).Scan(&n)
	if n != 0 {
		t.Error("the first-run link should show again")
	}
}

func TestRestoreKeepsRevocations(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-removed', 'active', 'x', 'x')`,
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-disabled', 'active', 'x', 'x')`,
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-by-mail', 'active', 'x', 'x')`,
		`INSERT INTO auth_user_emails (user_id, email) VALUES ('user-by-mail', 'moved@example.com')`,
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-pending', 'pending', 'x', 'x')`,
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-active-now', 'pending', 'x', 'x')`,
	} {
		if _, err := st.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	leaked, _, err := st.CreateAPIKey(ctx, "leaked", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := st.CreateAPIKey(ctx, "removed since", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	res := create(t, r, false)

	// After the backup: one key leaks and is revoked, one is deleted, one is made, a user goes.
	st, err = store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeAPIKey(ctx, leaked.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAPIKey(ctx, removed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateAPIKey(ctx, "made since", []string{"inference:chat"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM auth_users WHERE id = 'user-1'`,
		`UPDATE auth_users SET status = 'removed' WHERE id = 'user-removed'`,
		`UPDATE auth_users SET status = 'disabled' WHERE id = 'user-disabled'`,
		// The same address under another id: a withdrawn invitation made again and withdrawn.
		`DELETE FROM auth_user_emails WHERE user_id = 'user-by-mail'`,
		`DELETE FROM auth_users WHERE id = 'user-by-mail'`,
		`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('another-id', 'removed', 'x', 'x')`,
		`INSERT INTO auth_user_emails (user_id, email) VALUES ('another-id', 'moved@example.com')`,
		`UPDATE auth_users SET status = 'active' WHERE id = 'user-active-now'`,
	} {
		if _, err := st.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	rep, _ := restore(t, res.Output, r.Dir, true)
	if rep.RevocationsKept != 1 {
		t.Errorf("revocations kept: %d", rep.RevocationsKept)
	}
	if strings.Join(rep.ReturningKeys, ",") != "removed since" {
		t.Errorf("returning: %v", rep.ReturningKeys)
	}
	if strings.Join(rep.KeysLost, ",") != "made since" {
		t.Errorf("lost: %v", rep.KeysLost)
	}
	if rep.ReturningUsers != 1 {
		t.Errorf("returning users: %d", rep.ReturningUsers)
	}
	if rep.UserStatusesKept != 3 {
		t.Errorf("user statuses kept: %d", rep.UserStatusesKept)
	}
	if rep.UsersChanged != 1 {
		t.Errorf("users going back: %d", rep.UsersChanged)
	}
	if strings.Join(rep.Rules, " ") != SignOutLine+" "+RevokedLine {
		t.Errorf("rules: %v", rep.Rules)
	}

	st, err = store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, err := st.APIKeyByID(ctx, leaked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k == nil || k.RevokedAt == nil {
		t.Errorf("the leaked key works again: %+v", k)
	}
	for id, want := range map[string]string{"user-removed": "removed", "user-disabled": "disabled", "user-by-mail": "removed", "user-pending": "pending", "user-1": "active", "user-active-now": "pending"} {
		var got string
		if err := st.DB.QueryRowContext(ctx, `SELECT status FROM auth_users WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}
}

func TestRestoreReportsWhatMatters(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e,
		`INSERT INTO go_settings (key, value, updated_at) VALUES ('imports.allow-private', 'true', 'x')`,
		`INSERT INTO go_settings (key, value, updated_at) VALUES ('imports.allow-local-paths', 'true', 'x')`,
		`INSERT INTO go_settings (key, value, updated_at) VALUES ('gateway.max-connections', '5', 'x')`,
		`INSERT INTO imported_models (id, definition, created_at, updated_at) VALUES ('a', '{"id":"a","name":"a","tasks":["chat"],"variants":[{"id":"v","artifacts":[{"filename":"a.gguf","role":"primary","required":true,"source":{"kind":"local","path":"/secret/place/a.gguf"}}]}]}', 'x', 'x')`)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	s, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	rep := s.Report
	if len(rep.Programs) != 1 || strings.Join(rep.Programs[0].Command, " ") != "/bin/sh -c run --port {port}" || !rep.Programs[0].Exists {
		t.Errorf("the whole command line: %+v", rep.Programs)
	}
	text, _ := json.Marshal(rep.Settings)
	for _, want := range []string{"0.0.0.0:28300", "otel.example.com", "OTLP headers", "imports.allow-private", "imports.allow-local-paths", "gateway.max-connections"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("%s is missing from %s", want, text)
		}
	}
	if len(rep.Sources) != 1 || rep.Sources[0].Where != "/secret/place/a.gguf" {
		t.Errorf("sources: %+v", rep.Sources)
	}
	if strings.Contains(string(text), secretOTLP) {
		t.Error("the report shows a secret value")
	}
	if strings.Join(rep.Rules, " ") != SignOutLine {
		t.Errorf("a new root has only the sign-out rule: %v", rep.Rules)
	}
	if !strings.Contains(strings.Join(rep.Cautions, " "), "keys list") {
		t.Errorf("a new root says to check its keys: %v", rep.Cautions)
	}
	if len(rep.Ports) != 3 || rep.Ports[0] != 28300 {
		t.Errorf("ports: %v", rep.Ports)
	}
	if !contains(rep.Manifest.RestoreRules, SignOutLine) || !contains(rep.Manifest.RestoreRules, RevokedLine) {
		t.Errorf("the manifest should state the rules: %v", rep.Manifest.RestoreRules)
	}
}

// --- what a restore does -----------------------------------------------------------------------

func TestRestoreIntoANewRoot(t *testing.T) {
	for _, mask := range []int{0, 0o077} {
		t.Run(fmt.Sprintf("umask %04o", mask), func(t *testing.T) {
			r, secret := newRoot(t, dir(t))
			res := create(t, r, false)
			parent := dir(t)
			was := syscall.Umask(mask)
			defer syscall.Umask(was)
			target := filepath.Join(parent, "deep", "new")
			restore(t, res.Output, target, false)
			noLeftovers(t, parent)

			opened, err := root.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			if bad := obs.CheckRootFiles(opened.Dir); len(bad) > 0 {
				t.Errorf("unsafe: %v", bad)
			}
			filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					t.Fatal(err)
				}
				info, _ := d.Info()
				want := fs.FileMode(0o600)
				if d.IsDir() {
					want = 0o700
				}
				if info.Mode().Perm() != want {
					t.Errorf("%s is %o, want %o", p, info.Mode().Perm(), want)
				}
				if !ownedByMe(info) {
					t.Errorf("%s belongs to someone else", p)
				}
				return nil
			})

			st, err := store.Open(opened.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			cfg, err := st.LoadConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Root != opened.Dir || cfg.HFToken != secretHF {
				t.Errorf("config: root %q token %q", cfg.Root, cfg.HFToken)
			}
			if k, err := st.ResolveAPIKey(ctx, secret); err != nil || k == nil {
				t.Errorf("the key should work: %v %v", k, err)
			}
			p, _ := voices.Path(opened.Dir, voiceID)
			if _, err := os.Stat(p); err != nil {
				t.Error(err)
			}
			for _, name := range []string{"models", "runs", "service", ".before-restore"} {
				matches, _ := filepath.Glob(filepath.Join(target, name+"*"))
				if len(matches) != 0 {
					t.Errorf("a new root has %v", matches)
				}
			}
		})
	}
}

func TestRestoreReplace(t *testing.T) {
	parent := dir(t)
	r, _ := newRoot(t, parent)
	res := create(t, r, false)
	model := filepath.Join(r.Dir, "models", "llm", "a.gguf")
	stat := func(p string) uint64 {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Sys().(*syscall.Stat_t).Ino
	}
	inode := stat(model)
	// Changes after the backup, which replacing must undo, and keep aside.
	write(t, filepath.Join(r.Dir, "catalog.d", "later.json"), "{}")
	if _, err := Stage(context.Background(), res.Output, r.Dir, false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("without --replace: %v", err)
	}
	_, done := restore(t, res.Output, r.Dir, true)
	if stat(model) != inode {
		t.Error("the model was moved")
	}
	for _, kept := range []string{"logs/events.jsonl", "runs/run_1.json", "runtime/ui-csrf.key", "service/manifest.json"} {
		if _, err := os.Stat(filepath.Join(r.Dir, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "catalog.d", "later.json")); !os.IsNotExist(err) {
		t.Errorf("later.json should be gone from the catalog: %v", err)
	}
	if done.BeforeDir == "" || !strings.HasPrefix(filepath.Base(done.BeforeDir), beforePrefix) {
		t.Fatalf("before: %q", done.BeforeDir)
	}
	info, err := os.Stat(done.BeforeDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("the old files are in a %o directory", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(done.BeforeDir, "catalog.d", "later.json")); err != nil {
		t.Errorf("the old catalog should be kept: %v", err)
	}
	if _, err := root.Open(r.Dir); err != nil {
		t.Error(err)
	}
	noLeftovers(t, parent)
}

func TestRestoreRefusesTargets(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	ctx := context.Background()

	t.Run("a running gateway", func(t *testing.T) {
		unlock, err := r.Lock("127.0.0.1", 1)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if _, err := Stage(ctx, res.Output, r.Dir, true); err == nil || !strings.Contains(err.Error(), "local-base --root") || !strings.Contains(err.Error(), "stop") {
			t.Fatal(err)
		}
	})
	t.Run("a directory that isn't a root", func(t *testing.T) {
		d := dir(t)
		write(t, filepath.Join(d, "photos", "a.jpg"), "mine")
		before := tree(t, d)
		for _, replace := range []bool{false, true} {
			if _, err := Stage(ctx, res.Output, filepath.Join(d, "photos"), replace); err == nil || !strings.Contains(err.Error(), "not this LocalBase root") {
				t.Fatal(err)
			}
		}
		if tree(t, d) != before {
			t.Error("it changed")
		}
	})
	t.Run("a file", func(t *testing.T) {
		d := dir(t)
		write(t, filepath.Join(d, "f"), "x")
		if _, err := Stage(ctx, res.Output, filepath.Join(d, "f"), false); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatal(err)
		}
	})
	t.Run("a root another user owns", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("as root, nothing belongs to another user")
		}
		// / is someone else's, and holds things: refused without touching it.
		if _, err := Stage(ctx, res.Output, "/usr", false); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("the filesystem root and home", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip(err)
		}
		for _, target := range []string{"/", home} {
			if _, err := Stage(ctx, res.Output, target, true); err == nil {
				t.Errorf("%s was accepted", target)
			}
		}
	})
	t.Run("an unfinished restore", func(t *testing.T) {
		r2, _ := newRoot(t, dir(t))
		write(t, filepath.Join(r2.Dir, root.RestoreJournalName), "{}")
		if _, err := Stage(ctx, res.Output, r2.Dir, true); err == nil || !strings.Contains(err.Error(), "restore --undo") {
			t.Fatal(err)
		}
	})
}

func TestRestoreDryRunWritesNothing(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	parent := dir(t)
	target := filepath.Join(parent, "target")
	before := tree(t, parent)
	s, err := Stage(context.Background(), res.Output, target, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Discard()
	if after := tree(t, parent); after != before {
		t.Errorf("changed:\n%s", after)
	}
	// On a root with data too.
	before = tree(t, r.Dir)
	s, err = Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Discard()
	if after := tree(t, r.Dir); after != before {
		t.Errorf("the root changed:\n%s\nwas:\n%s", after, before)
	}
}

// steps lists every place a restore can stop, as it runs.
func steps(t *testing.T, file, target string, replace bool) []string {
	t.Helper()
	var out []string
	was := stepHook
	stepHook = func(s string) error { out = append(out, s); return nil }
	defer func() { stepHook = was }()
	s, err := Stage(context.Background(), file, target, replace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoreFailureAndCrash(t *testing.T) {
	base := dir(t)
	r, _ := newRoot(t, filepath.Join(base, "probe"))
	res := create(t, r, false)
	all := steps(t, res.Output, r.Dir, true)
	if len(all) < 8 {
		t.Fatalf("steps: %v", all)
	}
	for _, name := range all {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s crash=%v", name, crash), func(t *testing.T) {
				parent := dir(t)
				r, _ := newRoot(t, parent)
				backup := create(t, r, false)
				before := tree(t, r.Dir, "runtime", ".before-restore")
				was := stepHook
				stepHook = func(s string) error {
					if s != name {
						return nil
					}
					if crash {
						panic("crash")
					}
					return fmt.Errorf("injected at %s", s)
				}
				defer func() { stepHook = was }()
				s, err := Stage(context.Background(), backup.Output, r.Dir, true)
				if err != nil {
					t.Fatal(err)
				}
				if crash {
					func() {
						defer func() {
							if recover() == nil {
								t.Fatal("no crash")
							}
						}()
						s.Commit()
					}()
					stepHook = was
					if _, err := root.Open(r.Dir); err == nil || !strings.Contains(err.Error(), "a restore did not finish") || !strings.Contains(err.Error(), "restore --undo") {
						t.Fatalf("after a crash, Open says: %v", err)
					}
					if err := Undo(r.Dir); err != nil {
						t.Fatal(err)
					}
				} else if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "put back") {
					t.Fatalf("commit: %v", err)
				}
				if _, err := root.Open(r.Dir); err != nil {
					t.Fatal(err)
				}
				if after := tree(t, r.Dir, "runtime"); after != before {
					t.Errorf("not put back:\n%s\nwas:\n%s", after, before)
				}
				noLeftovers(t, parent)
			})
		}
	}
}

// A restored root gets the checks a gateway gets before the restore counts as finished. One that
// fails them goes back, rather than leaving a root that stops every command.
func TestRestoreVerifiesBeforeFinishing(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	before := tree(t, r.Dir, "runtime")
	was := stepHook
	stepHook = func(step string) error {
		if step == "in local-base.db" {
			// What is already moved in stops being trustworthy, just before the check.
			if err := os.Chmod(filepath.Join(r.Dir, "ui-access.json"), 0o666); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	defer func() { stepHook = was }()
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "put back") || !strings.Contains(err.Error(), "other users can change") {
		t.Fatalf("commit: %v", err)
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("not put back:\n%s\nwas:\n%s", after, before)
	}
}

func TestRestoreCrashOnANewRoot(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	parent := dir(t)
	target := filepath.Join(parent, "new")
	was := stepHook
	stepHook = func(s string) error {
		if s == "in local-base.db" {
			panic("crash")
		}
		return nil
	}
	defer func() { stepHook = was }()
	s, err := Stage(context.Background(), res.Output, target, false)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { recover() }()
		s.Commit()
	}()
	stepHook = was
	if err := Undo(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(target)
		t.Errorf("a root it made should go: %v %v", err, entries)
	}
	noLeftovers(t, parent)
}

func TestRestoreUndoTreatsJournalAsInput(t *testing.T) {
	makeRoot := func(t *testing.T) (*root.Root, string) {
		parent := dir(t)
		r, _ := newRoot(t, parent)
		return r, parent
	}
	plant := func(t *testing.T, r *root.Root, j string) {
		write(t, filepath.Join(r.Dir, root.RestoreJournalName), j)
	}
	good := `.before-restore-20260101-000000`
	for name, tc := range map[string]struct {
		journal func(outside string) string
		setup   func(t *testing.T, r *root.Root)
		want    string
	}{
		"a path out of the root as the old files": {func(string) string { return `{"version":1,"before":"../../elsewhere"}` }, nil, "doesn't make"},
		"an absolute path":                        {func(o string) string { return `{"version":1,"before":"` + o + `"}` }, nil, "doesn't make"},
		"a name off the list":                     {func(string) string { return `{"version":1,"before":"` + good + `","had":["../../.ssh"]}` }, nil, "doesn't touch"},
		"a staging path":                          {func(o string) string { return `{"version":1,"before":"` + good + `","staging":"` + o + `"}` }, nil, "doesn't make"},
		"not a journal":                           {func(string) string { return `nonsense` }, nil, "can't be read"},
		"an unknown field":                        {func(string) string { return `{"version":1,"before":"` + good + `","delete":"everything"}` }, nil, "can't be read"},
		"old files in a directory others can reach": {func(string) string { return `{"version":1,"before":"` + good + `"}` }, func(t *testing.T, r *root.Root) {
			p := filepath.Join(r.Dir, good)
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "private directory"},
		"old files that are a link": {func(string) string { return `{"version":1,"before":"` + good + `"}` }, func(t *testing.T, r *root.Root) {
			if err := os.Symlink(t.TempDir(), filepath.Join(r.Dir, good)); err != nil {
				t.Fatal(err)
			}
		}, "private directory"},
		"a link among the old files": {func(string) string { return `{"version":1,"before":"` + good + `"}` }, func(t *testing.T, r *root.Root) {
			p := filepath.Join(r.Dir, good)
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/passwd", filepath.Join(p, "ui-access.json")); err != nil {
				t.Fatal(err)
			}
		}, "link"},
	} {
		t.Run(name, func(t *testing.T) {
			r, parent := makeRoot(t)
			outside := filepath.Join(parent, "outside")
			write(t, filepath.Join(outside, "keep"), "keep")
			if tc.setup != nil {
				tc.setup(t, r)
			}
			plant(t, r, tc.journal(outside))
			before := tree(t, parent, "root/runtime")
			err := Undo(r.Dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if after := tree(t, parent, "root/runtime"); after != before {
				t.Errorf("it moved something:\n%s\nwas:\n%s", after, before)
			}
		})
	}

	t.Run("a planted journal on a fresh root can't delete a root", func(t *testing.T) {
		r, _ := makeRoot(t)
		// It claims this root was new, and that nothing was there before.
		plant(t, r, `{"version":1,"before":"`+good+`","newRoot":true}`)
		if err := Undo(r.Dir); err != nil {
			t.Fatal(err)
		}
		// What it can reach is on the constant list. Models, logs and the rest remain.
		for _, kept := range []string{"models/llm/a.gguf", "logs/events.jsonl", "runs/run_1.json"} {
			if _, err := os.Stat(filepath.Join(r.Dir, filepath.FromSlash(kept))); err != nil {
				t.Errorf("%s: %v", kept, err)
			}
		}
	})

	t.Run("nothing to undo", func(t *testing.T) {
		r, _ := makeRoot(t)
		if err := Undo(r.Dir); err == nil || !strings.Contains(err.Error(), "no unfinished restore") {
			t.Fatal(err)
		}
	})

	t.Run("a journal others can write is refused first", func(t *testing.T) {
		r, _ := makeRoot(t)
		plant(t, r, `{"version":1,"before":"`+good+`"}`)
		if err := os.Chmod(filepath.Join(r.Dir, root.RestoreJournalName), 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := root.Open(r.Dir); err == nil || !strings.Contains(err.Error(), "other users can change it") {
			t.Errorf("Open: %v", err)
		}
		if err := Undo(r.Dir); err == nil || !strings.Contains(err.Error(), "other users can change it") {
			t.Errorf("Undo: %v", err)
		}
	})
}
