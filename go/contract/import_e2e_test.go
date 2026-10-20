package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// TestModelImportAcceptance runs the acceptance checks of
// docs/model-import-and-backend-selection.md against the real local-base
// binary on a scratch root and port, with fake Hugging Face, vendor and
// backend servers. Nothing touches the network or the live install.

const (
	vendorSecret = "vendor-secret-7d1e9a"
	hfToken      = "hf_e2eTokenValue12345"
	signedValue  = "signedquery8842"
)

type seen struct {
	mu   sync.Mutex
	reqs []string // method path auth header-values
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hv []string
	for _, vs := range r.Header {
		hv = append(hv, vs...)
	}
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path+" "+strings.Join(hv, "|"))
}

func (s *seen) contains(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reqs {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func ggufFile(t *testing.T, arch string, extra map[string]any, size int) []byte {
	t.Helper()
	kv := map[string]any{"general.architecture": arch, "general.file_type": 15, arch + ".context_length": 4096}
	for k, v := range extra {
		kv[k] = v
	}
	p := filepath.Join(t.TempDir(), "f.gguf")
	if err := testutil.GGUF(p, kv, size); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	return b
}

func hexsum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func serveBytes(w http.ResponseWriter, r *http.Request, b []byte) {
	http.ServeContent(w, r, "", testutil.Epoch, bytes.NewReader(b))
}

type e2e struct {
	t      *testing.T
	root   string
	env    []string
	base   string
	key    string
	stop   func()
	outDir string
}

func (e *e2e) cli(extraEnv []string, args ...string) cliResult {
	e.t.Helper()
	cmd := exec.Command(goCLI(e.t), append([]string{"--root", e.root}, args...)...)
	cmd.Env = append(append(cleanEnv(), e.env...), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	r := cliResult{code, out.String(), errb.String()}
	for _, s := range []string{vendorSecret, hfToken, signedValue} {
		if strings.Contains(r.stdout+r.stderr, s) {
			e.t.Fatalf("%v printed a credential (%s):\n%s%s", args, s, r.stdout, r.stderr)
		}
	}
	return r
}

func (e *e2e) json(args ...string) map[string]any {
	e.t.Helper()
	r := e.cli(nil, append([]string{"--json"}, args...)...)
	var env struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || !env.OK {
		e.t.Fatalf("%v: exit %d\n%s%s", args, r.code, r.stdout, r.stderr)
	}
	return env.Data
}

func (e *e2e) serve() {
	e.t.Helper()
	port, llmPort := freePort(), freePort()
	cmd := exec.Command(goCLI(e.t), "--root", e.root, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--llm-port", fmt.Sprint(llmPort))
	cmd.Env = append(cleanEnv(), e.env...)
	logf, _ := os.Create(filepath.Join(e.outDir, fmt.Sprintf("serve-%d.log", port)))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	e.stop = func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		logf.Close()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := http.Get(e.base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			e.stop()
			b, _ := os.ReadFile(logf.Name())
			e.t.Fatalf("gateway did not start:\n%s", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *e2e) chat(model string) *Resp {
	return do(e.t, "POST", e.base+"/v1/chat/completions", bearer(e.key),
		`{"model":"`+model+`","messages":[{"role":"user","content":"fake:reply hello there"}]}`)
}

func (e *e2e) setBackend(model, backend string) map[string]any {
	e.t.Helper()
	body := `{"action":"set","model":"` + model + `","backendId":"` + backend + `"}`
	if backend == "" {
		body = `{"action":"reset","model":"` + model + `"}`
	}
	r := do(e.t, "POST", e.base+"/_localbase/model-backend", bearer(e.key), body)
	if r.Status != 200 {
		e.t.Fatalf("set backend %s: %d %s", backend, r.Status, r.Body)
	}
	return r.JSON(e.t)["selection"].(map[string]any)
}

func (e *e2e) selection(model string) map[string]any {
	e.t.Helper()
	r := do(e.t, "GET", e.base+"/_localbase/model-backend?model="+model, bearer(e.key), "")
	if r.Status != 200 {
		e.t.Fatalf("selection: %d %s", r.Status, r.Body)
	}
	return r.JSON(e.t)["selection"].(map[string]any)
}

func wantCode(t *testing.T, r *Resp, status int, code string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("want %d %s, got %d %s", status, code, r.Status, r.Body)
	}
	if code != "" {
		if got := r.ErrorCode(t); got != code {
			t.Fatalf("want code %s, got %v: %s", code, got, r.Body)
		}
	}
}

// platformBackends are the built-in builds for this host's own platform:
// the automatic llama.cpp choice and a transcription-only backend.
func platformBackends() (llama, wrongTask string) {
	if goruntime.GOOS == "darwin" {
		return "llama.cpp-metal", "whisper.cpp-metal"
	}
	return "", "whisper.cpp-vulkan" // Linux: CUDA, Vulkan or CPU, depending on the machine
}

func TestModelImportAcceptance(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("Go-only feature")
	}
	tmp := t.TempDir()
	tmp, _ = filepath.EvalSymlinks(tmp)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}

	// --- fake sources ---------------------------------------------------------------
	weights := ggufFile(t, "qwen3", nil, 64*1024)
	projector := ggufFile(t, "clip", map[string]any{"clip.has_vision_encoder": true}, 8*1024)
	mystery := ggufFile(t, "mystery-arch", nil, 32*1024)
	const commit = "aaaabbbbccccddddeeeeffff0000111122223333"
	cdnSeen, hfSeen, vendorSeen, otherSeen := &seen{}, &seen{}, &seen{}, &seen{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnSeen.add(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/model-Q4_K_M.gguf"):
			serveBytes(w, r, weights)
		case strings.HasSuffix(r.URL.Path, "/mmproj-model-f16.gguf"):
			serveBytes(w, r, projector)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cdn.Close()
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hfSeen.add(r)
		gated := strings.Contains(r.URL.Path, "gated")
		if gated && r.Header.Get("Authorization") != "Bearer "+hfToken {
			w.WriteHeader(401)
			return
		}
		repo := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/models/"), "/revision/")[0]
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/") && strings.Contains(r.URL.Path, "/revision/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "pipeline_tag": "image-text-to-text",
				"gguf": map[string]any{"architecture": "qwen3", "context_length": 4096},
				"siblings": []any{
					map[string]any{"rfilename": "model-Q4_K_M.gguf", "lfs": map[string]any{"sha256": hexsum(weights), "size": len(weights)}},
					map[string]any{"rfilename": "mmproj-model-f16.gguf", "lfs": map[string]any{"sha256": hexsum(projector), "size": len(projector)}},
					map[string]any{"rfilename": "modeling_evil.py", "size": 12},
				}})
			_ = repo
		case strings.Contains(r.URL.Path, "/resolve/"+commit+"/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/"):]
			http.Redirect(w, r, cdn.URL+"/blobs"+name+"?X-Amz-Signature="+signedValue, 302)
		default:
			w.WriteHeader(404)
		}
	}))
	defer hf.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSeen.add(r)
		serveBytes(w, r, mystery)
	}))
	defer other.Close()
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorSeen.add(r)
		if r.Header.Get("X-Api-Key") != vendorSecret {
			http.Error(w, "no key", 403)
			return
		}
		switch r.URL.Path {
		case "/files/mystery.gguf":
			serveBytes(w, r, mystery)
		case "/moved/mystery.gguf":
			http.Redirect(w, r, other.URL+"/mystery.gguf?token="+signedValue, 302)
		}
	}))
	defer vendor.Close()

	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp,
		env: []string{"HF_ENDPOINT=" + hf.URL, "LOCALBASE_LLAMA_SERVER=" + fake, "LOCALBASE_DEVICE_ID=device-one",
			"VENDOR_KEY=" + vendorSecret}}
	if r := e.cli(nil, "init"); r.code != 0 {
		t.Fatalf("init: %s%s", r.stdout, r.stderr)
	}
	key := e.json("keys", "create", "--name", "e2e", "--scopes", "inference:chat,models:read,models:manage")
	e.key = key["secret"].(string)
	autoLlama, wrongTask := platformBackends()

	// 1. A public Hugging Face import resolves an immutable revision, never
	// fetches repository code, and chooses this platform's variant/backend.
	d := e.json("models", "import", "hf:org/model-GGUF", "--id", "vision-model")
	plan := d["plan"].(map[string]any)
	sel := d["selection"].(map[string]any)
	if plan["revision"] != commit || plan["custom"] != false {
		t.Fatalf("plan %v", plan)
	}
	if b, _ := sel["backendId"].(string); b == "" || (autoLlama != "" && b != autoLlama) {
		t.Fatalf("automatic backend %q on %s", b, goruntime.GOOS)
	}
	if hfSeen.contains(".py") || cdnSeen.contains(".py") {
		t.Fatal("repository code was requested")
	}
	if cdnSeen.contains(hfToken) {
		t.Fatal("token on the CDN")
	}
	// An authenticated (gated) repository needs the token, which stays on the HF origin.
	if r := e.cli(nil, "models", "import", "hf:org/gated-GGUF", "--id", "gated-model"); r.code == 0 {
		t.Fatal("gated import without a token succeeded")
	}
	if r := e.cli([]string{"HF_TOKEN=" + hfToken}, "models", "import", "hf:org/gated-GGUF", "--id", "gated-model"); r.code != 0 {
		t.Fatalf("gated import with a token: %s%s", r.stdout, r.stderr)
	}
	if !hfSeen.contains("Bearer " + hfToken) {
		t.Fatal("the token never reached Hugging Face")
	}
	if cdnSeen.contains(hfToken) {
		t.Fatal("the Hugging Face token followed the redirect to the CDN")
	}

	// 2. A direct URL shows the custom-source warning and needs consent;
	// private headers stay on their origin; unknown metadata leaves the
	// backend blank, and the import still finishes.
	if d := e.json("auth-profiles", "add", "--id", "vendor", "--origin", vendor.URL, "--header-env", "X-Api-Key=VENDOR_KEY"); d == nil {
		t.Fatal("profile")
	}
	src := vendor.URL + "/files/mystery.gguf?sig=" + signedValue
	r := e.cli(nil, "models", "import", src, "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "mystery")
	if r.code != 2 || !strings.Contains(r.stdout+r.stderr, "custom source that LocalBase has not verified") ||
		!strings.Contains(r.stdout+r.stderr, "plain HTTP") {
		t.Fatalf("custom source without consent: %d %s%s", r.code, r.stdout, r.stderr)
	}
	d = e.json("models", "import", src, "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "mystery", "--yes")
	sel = d["selection"].(map[string]any)
	if sel["backendId"] != "" || sel["mode"] != "auto" {
		t.Fatalf("unknown architecture must leave the backend blank: %v", sel)
	}
	if opts := sel["recommendation"].(map[string]any)["options"].([]any); len(opts) < 10 {
		t.Fatalf("every registered backend must stay selectable, got %d", len(opts))
	}
	art := d["model"].(map[string]any)["variants"].([]any)[0].(map[string]any)["artifacts"].([]any)[0].(map[string]any)
	if art["hashTrust"] != "observed" || art["sha256"] != hexsum(mystery) {
		t.Fatalf("unpinned URL must record an observed hash: %v", art)
	}
	// Redirects to another origin do not carry the private header.
	e.json("models", "import", vendor.URL+"/moved/mystery.gguf", "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "moved", "--yes")
	if otherSeen.contains(vendorSecret) {
		t.Fatal("the private header followed a redirect to another origin")
	}
	// A wrong trusted checksum blocks publication.
	r = e.cli(nil, "models", "import", src, "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "pinned", "--yes",
		"--sha256", strings.Repeat("0", 64))
	if r.code == 0 || fileExists(filepath.Join(e.root, "models", "imported", "pinned")) {
		t.Fatalf("checksum mismatch published: %s%s", r.stdout, r.stderr)
	}
	// An incomplete multi-file model names its missing files.
	split := filepath.Join(tmp, "split")
	os.MkdirAll(split, 0o755)
	for i := 1; i <= 2; i++ {
		os.WriteFile(filepath.Join(split, fmt.Sprintf("big-%05d-of-00003.gguf", i)), ggufFile(t, "qwen3", map[string]any{"split.count": 3, "split.no": i - 1}, 4096), 0o644)
	}
	r = e.cli(nil, "models", "import", filepath.Join(split, "big-00001-of-00003.gguf"), "--task", "chat")
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "big-00003-of-00003.gguf") {
		t.Fatalf("incomplete split: %s%s", r.stdout, r.stderr)
	}

	// 3. Run through the gateway: the selection decides the backend.
	e.serve()
	defer func() { e.stop() }()
	if r := e.chat("vision-model"); r.Status != 200 || !strings.Contains(string(r.Body), "hello there") {
		t.Fatalf("automatic backend chat: %d %s", r.Status, r.Body)
	}
	wantCode(t, e.chat("mystery"), 409, "backend_not_selected")

	// Any registered backend can be chosen, with its finding shown.
	s := e.setBackend("mystery", "llama.cpp-cpu")
	if s["mode"] != "manual" || s["finding"].(map[string]any)["rating"] == "supported" {
		t.Fatalf("manual unknown choice: %v", s)
	}
	if r := e.chat("mystery"); r.Status != 200 {
		t.Fatalf("manual CPU backend chat: %d %s", r.Status, r.Body)
	}
	// A detected-incompatible choice is allowed and reports the real startup error.
	s = e.setBackend("mystery", wrongTask)
	if s["finding"].(map[string]any)["rating"] != "incompatible" {
		t.Fatalf("incompatible finding: %v", s)
	}
	r2 := e.chat("mystery")
	wantCode(t, r2, 503, "backend_startup_failed")
	if !strings.Contains(string(r2.Body), "does not run chat models") {
		t.Fatalf("startup error: %s", r2.Body)
	}

	// 4. A custom adapter through the versioned interface.
	def := func(id string, factor float64) string {
		p := filepath.Join(tmp, id+".json")
		b, _ := json.Marshal(map[string]any{"schema": "localbase.backend/v1", "id": id, "name": "Custom " + id, "engine": "fake-engine",
			"adapter": "process-openai/v1", "tasks": []string{"chat"}, "formats": []string{"gguf"},
			"architectures": map[string]any{"supported": []string{"mystery-arch"}},
			"platforms":     []any{map[string]any{"os": goruntime.GOOS, "arch": map[string]string{"arm64": "arm64", "amd64": "x64"}[goruntime.GOARCH], "accelerator": "cpu"}},
			"launch": map[string]any{"command": fake, "args": []string{"--host", "{host}", "--port", "{port}", "-m", "{model}"}, "healthPath": "/health",
				"memory": map[string]any{"weightsFactor": factor, "overheadGb": 0, "assumption": "weights times the factor; declared for this test"}}})
		os.WriteFile(p, b, 0o644)
		return p
	}
	e.json("backends", "add", "--file", def("fake-custom", 1))
	e.json("backends", "add", "--file", def("fake-hungry", 1e7))
	s = e.setBackend("mystery", "fake-custom")
	if s["finding"].(map[string]any)["rating"] != "supported" {
		t.Fatalf("custom backend finding: %v", s)
	}
	if r := e.chat("mystery"); r.Status != 200 {
		t.Fatalf("custom backend chat: %d %s", r.Status, r.Body)
	}
	// An override never bypasses memory admission.
	e.setBackend("mystery", "fake-hungry")
	wantCode(t, e.chat("mystery"), 503, "insufficient_memory")

	// 5. Manual choices survive reinspection and restart.
	e.json("models", "inspect", "mystery")
	e.stop()
	e.serve()
	if s := e.selection("mystery"); s["mode"] != "manual" || s["backendId"] != "fake-hungry" {
		t.Fatalf("after restart: %v", s)
	}
	// Device-local: another device starts in automatic mode.
	d2 := e.cli([]string{"LOCALBASE_DEVICE_ID=device-two"}, "--json", "models", "backend", "mystery")
	if !strings.Contains(d2.stdout, `"mode":"auto"`) {
		t.Fatalf("another device inherited the choice: %s", d2.stdout)
	}
	// A removed backend is kept as unavailable, never replaced.
	e.json("backends", "remove", "fake-hungry")
	if s := e.selection("mystery"); s["backendId"] != "fake-hungry" || s["unavailable"] != true {
		t.Fatalf("removed backend: %v", s)
	}
	wantCode(t, e.chat("mystery"), 409, "backend_unavailable")
	// Reset to automatic restores detection: blank again for this model.
	if s := e.setBackend("mystery", ""); s["mode"] != "auto" || s["backendId"] != "" {
		t.Fatalf("reset: %v", s)
	}
	wantCode(t, e.chat("mystery"), 409, "backend_not_selected")

	// 6. Exports, the API listing and logs carry no credentials, and the
	// OpenAI-compatible routes are unchanged in shape.
	exp := e.cli(nil, "--json", "models", "export", "mystery")
	if !strings.Contains(exp.stdout, `"authProfile":"vendor"`) || strings.Contains(exp.stdout, "device-one") {
		t.Fatalf("export: %s", exp.stdout)
	}
	list := do(t, "GET", e.base+"/v1/models", bearer(e.key), "")
	if list.Status != 200 || !strings.Contains(string(list.Body), `"id":"vision-model"`) || !strings.Contains(string(list.Body), `"object":"list"`) {
		t.Fatalf("models list: %s", list.Body)
	}
	e.stop()
	e.stop = func() {}
	logs, _ := filepath.Glob(filepath.Join(e.root, "logs", "*"))
	logs = append(logs, func() []string { m, _ := filepath.Glob(filepath.Join(tmp, "serve-*.log")); return m }()...)
	for _, f := range logs {
		b, _ := os.ReadFile(f)
		for _, s := range []string{vendorSecret, hfToken, signedValue} {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s contains a credential", filepath.Base(f))
			}
		}
	}
	db, _ := os.ReadFile(filepath.Join(e.root, "local-base.db"))
	for _, s := range []string{vendorSecret, signedValue} {
		if bytes.Contains(db, []byte(s)) {
			t.Fatal("the database contains a credential; it belongs in the auth profile file only")
		}
	}

	// 7. Rollback stays a binary swap: TS opens the database Go wrote.
	if _, err := exec.LookPath("bun"); err == nil && *implFlag == "both" {
		tsr := runCLI(t, "ts", e.root, "--json", "keys", "list")
		if tsr.code != 0 {
			t.Fatalf("TS could not open the database: %s%s", tsr.stdout, tsr.stderr)
		}
	}
}
