package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// TestModelImportAcceptance runs the acceptance checks of
// docs/model-import-and-backend-selection.md (AC1-AC10) and the operator's
// review points (OP1-OP5) against the real local-base binary on scratch
// roots and ports, with fake Hugging Face, CDN, vendor and backend servers.
// Nothing touches the network or the live install. Subtests run in order
// and share state; their names map one-to-one to the PR's traceability
// table.

const (
	vendorSecret = "vendor-secret-7d1e9a"
	hfToken      = "hf_e2eTokenValue12345"
	signedValue  = "signedquery8842"
)

var plantedSecrets = []string{vendorSecret, hfToken, signedValue}

type seen struct {
	mu   sync.Mutex
	reqs []string // method path header-values
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
	noAuth bool
	stop   func()
	// kill ends the gateway with SIGKILL (no shutdown at all).
	kill   func()
	outDir string
	// impl is "go" (default) or "ts".
	impl string
}

// scan fails when output contains a planted secret.
func (e *e2e) scan(label, text string) {
	e.t.Helper()
	for _, s := range plantedSecrets {
		if strings.Contains(text, s) {
			e.t.Fatalf("%s contains a credential (%s):\n%s", label, s, text)
		}
	}
}

func (e *e2e) command(args ...string) *exec.Cmd {
	if e.impl == "ts" {
		cmd := exec.Command("bun", append([]string{"run", "src/cli.ts"}, args...)...)
		cmd.Dir = repoRoot()
		return cmd
	}
	return exec.Command(goCLI(e.t), args...)
}

func (e *e2e) cli(extraEnv []string, args ...string) cliResult {
	e.t.Helper()
	cmd := e.command(append([]string{"--root", e.root}, args...)...)
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
	e.scan(fmt.Sprint(args), r.stdout+r.stderr)
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

// do sends an API request and scans the response for credentials.
func (e *e2e) do(method, path, body string) *Resp {
	e.t.Helper()
	h := bearer(e.key)
	if e.noAuth {
		h = map[string]string{}
	}
	r := do(e.t, method, e.base+path, h, body)
	e.scan(method+" "+path, string(r.Body))
	return r
}

func (e *e2e) serve(extra ...string) {
	e.t.Helper()
	port, llmPort := freePort(), freePort()
	args := append([]string{"--root", e.root, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--llm-port", fmt.Sprint(llmPort),
		"--tts-port", fmt.Sprint(freePort()), "--image-port", fmt.Sprint(freePort()), "--video-port", fmt.Sprint(freePort())}, extra...)
	cmd := e.command(args...)
	cmd.Env = append(cleanEnv(), e.env...)
	logf, _ := os.Create(filepath.Join(e.outDir, fmt.Sprintf("serve-%d.log", port)))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	watchGateway(cmd.Process.Pid)
	e.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	stopped := false
	e.kill = func() {
		if stopped {
			return
		}
		stopped = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		logf.Close()
	}
	e.stop = func() {
		if stopped {
			return
		}
		stopped = true
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
	return e.do("POST", "/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"user","content":"fake:reply hello there"}]}`)
}

// chatRetrying retries a transient insufficient_memory 503 as a client
// honouring Retry-After would: a small CI runner's free memory moves with
// everything else on it, while admission itself stays on.
func (e *e2e) chatRetrying(model string) *Resp {
	for i := 0; ; i++ {
		r := e.chat(model)
		if r.Status != 503 || !strings.Contains(string(r.Body), "insufficient_memory") || i == 5 {
			return r
		}
		time.Sleep(2 * time.Second)
	}
}

func (e *e2e) setBackend(model, backend string) map[string]any {
	e.t.Helper()
	body := `{"action":"set","model":"` + model + `","backendId":"` + backend + `"}`
	if backend == "" {
		body = `{"action":"reset","model":"` + model + `"}`
	}
	r := e.do("POST", "/_localbase/model-backend", body)
	if r.Status != 200 {
		e.t.Fatalf("set backend %s: %d %s", backend, r.Status, r.Body)
	}
	return r.JSON(e.t)["selection"].(map[string]any)
}

func (e *e2e) selection(model string) map[string]any {
	e.t.Helper()
	r := e.do("GET", "/_localbase/model-backend?model="+model, "")
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

// platformBackends are this host's automatic llama.cpp build ("" on Linux:
// CUDA, Vulkan or CPU, depending on the machine) and a transcription-only
// backend built for this platform.
func platformBackends() (llama, wrongTask string) {
	if goruntime.GOOS == "darwin" {
		return "llama.cpp-metal", "whisper.cpp-metal"
	}
	return "", "whisper.cpp-vulkan"
}

func warningCodes(f map[string]any) []string {
	var out []string
	for _, w := range f["warnings"].([]any) {
		out = append(out, w.(map[string]any)["code"].(string))
	}
	return out
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

	// --- fake sources -------------------------------------------------------------
	weights := ggufFile(t, "qwen3", nil, 64*1024)
	projector := ggufFile(t, "clip", map[string]any{"clip.has_vision_encoder": true}, 8*1024)
	mystery := ggufFile(t, "mystery-arch", nil, 32*1024)
	const commit = "aaaabbbbccccddddeeeeffff0000111122223333"
	cdnSeen, hfSeen, vendorSeen, otherSeen := &seen{}, &seen{}, &seen{}, &seen{}
	cdn := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnSeen.add(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/model-Q4_K_M.gguf"):
			serveBytes(w, r, weights)
		case strings.HasSuffix(r.URL.Path, "/mmproj-model-f16.gguf"):
			serveBytes(w, r, projector)
		default:
			w.WriteHeader(404)
		}
	}), false)
	defer cdn.Close()
	hf := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hfSeen.add(r)
		if strings.Contains(r.URL.Path, "gated") && r.Header.Get("Authorization") != "Bearer "+hfToken {
			w.WriteHeader(401)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/") && strings.Contains(r.URL.Path, "/revision/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "pipeline_tag": "image-text-to-text",
				"gguf": map[string]any{"architecture": "qwen3", "context_length": 4096},
				"siblings": []any{
					map[string]any{"rfilename": "model-Q4_K_M.gguf", "lfs": map[string]any{"sha256": hexsum(weights), "size": len(weights)}},
					map[string]any{"rfilename": "mmproj-model-f16.gguf", "lfs": map[string]any{"sha256": hexsum(projector), "size": len(projector)}},
					map[string]any{"rfilename": "modeling_evil.py", "size": 12},
				}})
		case strings.Contains(r.URL.Path, "/resolve/"+commit+"/"):
			// Like the Hub: a signed redirect to a CDN on another origin.
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/"):]
			http.Redirect(w, r, cdn.URL+"/xet"+name+"?X-Amz-Signature="+signedValue, 302)
		default:
			w.WriteHeader(404)
		}
	}), false)
	defer hf.Close()
	other := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSeen.add(r)
		serveBytes(w, r, mystery)
	}), false)
	defer other.Close()
	vendor := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), false)
	defer vendor.Close()
	// HTTPS sources that redirect to plain HTTP: a direct URL and a Hugging
	// Face endpoint whose download hop points at an http:// CDN.
	tlsSource := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, vendor.URL+"/files/mystery.gguf", 302)
	}), true)
	defer tlsSource.Close()
	tlsHF := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "pipeline_tag": "text-generation",
				"siblings": []any{map[string]any{"rfilename": "model-Q4_K_M.gguf", "lfs": map[string]any{"sha256": hexsum(weights), "size": len(weights)}}}})
		default:
			http.Redirect(w, r, cdn.URL+"/downgrade/model-Q4_K_M.gguf", 302)
		}
	}), true)
	defer tlsHF.Close()
	caFile := filepath.Join(tmp, "test-ca.pem")
	var caPEM []byte
	for _, s := range []*httptest.Server{tlsSource, tlsHF} {
		caPEM = append(caPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})...)
	}
	os.WriteFile(caFile, caPEM, 0o644)

	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp,
		env: []string{"HF_ENDPOINT=" + hf.URL, "LOCALBASE_LLAMA_SERVER=" + fake, "LOCALBASE_DEVICE_ID=device-one",
			"VENDOR_KEY=" + vendorSecret, "LOCALBASE_DOWNLOAD_CA_FILE=" + caFile}}
	defer func() {
		if e.stop != nil {
			e.stop()
		}
	}()
	ok := true
	step := func(name string, fn func(t *testing.T)) {
		if !ok {
			t.Run(name, func(t *testing.T) { t.Skip("an earlier step failed") })
			return
		}
		ok = t.Run(name, func(st *testing.T) {
			e.t = st
			fn(st)
		})
		e.t = t
	}
	autoLlama, wrongTask := platformBackends()

	step("setup", func(t *testing.T) {
		if r := e.cli(nil, "init"); r.code != 0 {
			t.Fatalf("init: %s%s", r.stdout, r.stderr)
		}
		// Small launches and reserves, so the tiny test models fit on small CI
		// runners; memory admission itself stays on (an override must not
		// bypass it, see the AC5 memory step).
		cfg := filepath.Join(tmp, "small.toml")
		os.WriteFile(cfg, []byte("ctxSize = 2048\nctxMode = \"explicit\"\nparallel = 1\n\n[memory.systemReserve]\npercent = 1\nminimumGb = 0\n\n[memory.acceleratorReserve]\npercent = 1\nminimumGb = 0\n"), 0o644)
		if r := e.cli(nil, "--non-interactive", "configure", "--config", cfg); r.code != 0 {
			t.Fatalf("configure: %s%s", r.stdout, r.stderr)
		}
		e.key = e.json("keys", "create", "--name", "e2e", "--scopes", "inference:chat,models:read,models:manage")["secret"].(string)
		e.json("auth-profiles", "add", "--id", "vendor", "--origin", vendor.URL, "--header-env", "X-Api-Key=VENDOR_KEY")
	})

	step("AC6 public and authenticated Hugging Face imports resolve an immutable revision", func(t *testing.T) {
		d := e.json("models", "import", "hf:org/model-GGUF", "--id", "vision-model")
		if plan := d["plan"].(map[string]any); plan["revision"] != commit || plan["custom"] != false {
			t.Fatalf("plan %v", plan)
		}
		if a := d["model"].(map[string]any)["variants"].([]any)[0].(map[string]any)["artifacts"].([]any)[0].(map[string]any); a["source"].(map[string]any)["revision"] != commit || a["hashTrust"] != "authoritative" {
			t.Fatalf("artifact %v", a)
		}
		if r := e.cli(nil, "models", "import", "hf:org/gated-GGUF", "--id", "gated-model"); r.code == 0 {
			t.Fatal("gated import without a token succeeded")
		}
		if r := e.cli([]string{"HF_TOKEN=" + hfToken}, "models", "import", "hf:org/gated-GGUF", "--id", "gated-model"); r.code != 0 {
			t.Fatalf("gated import with a token: %s%s", r.stdout, r.stderr)
		}
		if !hfSeen.contains("Bearer " + hfToken) {
			t.Fatal("the token never reached Hugging Face")
		}
	})

	step("OP3 the Hugging Face token stays off CDN origins and anonymous CDN downloads work", func(t *testing.T) {
		if cdnSeen.contains(hfToken) {
			t.Fatal("the Hugging Face token followed the redirect to the CDN")
		}
		if !cdnSeen.contains("/xet/model-Q4_K_M.gguf") {
			t.Fatal("the public import did not download from the CDN")
		}
	})

	step("AC9 downloading weights never executes repository code or adds a backend", func(t *testing.T) {
		if hfSeen.contains(".py") || cdnSeen.contains(".py") {
			t.Fatal("repository code was requested")
		}
		for _, b := range e.json("backends", "list")["backends"].([]any) {
			if b.(map[string]any)["builtIn"] != true {
				t.Fatalf("a download registered a backend: %v", b)
			}
		}
	})

	step("AC1 the logical model picks this platform's variant and backend by rule", func(t *testing.T) {
		sel := e.json("models", "backend", "vision-model")["selection"].(map[string]any)
		b, _ := sel["backendId"].(string)
		if b == "" || (autoLlama != "" && b != autoLlama) {
			t.Fatalf("automatic backend %q on %s", b, goruntime.GOOS)
		}
	})

	step("AC7 a direct URL shows the custom-source warning and keeps private headers off progress, errors, exports and redirects", func(t *testing.T) {
		src := vendor.URL + "/files/mystery.gguf?sig=" + signedValue
		r := e.cli(nil, "models", "import", src, "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "mystery")
		if r.code != 2 || !strings.Contains(r.stdout+r.stderr, "custom source that LocalBase has not verified") || !strings.Contains(r.stdout+r.stderr, "plain HTTP") {
			t.Fatalf("custom source without consent: %d %s%s", r.code, r.stdout, r.stderr)
		}
		e.json("models", "import", src, "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "mystery", "--yes")
		if !vendorSeen.contains(vendorSecret) {
			t.Fatal("the private header never reached its origin")
		}
		e.json("models", "import", vendor.URL+"/moved/mystery.gguf", "--auth-profile", "vendor", "--allow-http", "--task", "chat", "--id", "moved", "--yes")
		if otherSeen.contains(vendorSecret) {
			t.Fatal("the private header followed a redirect to another origin")
		}
		if exp := e.cli(nil, "--json", "models", "export", "mystery"); !strings.Contains(exp.stdout, `"authProfile":"vendor"`) {
			t.Fatalf("export lost the profile reference: %s", exp.stdout)
		}
	})

	step("OP3 an HTTPS to plain-HTTP redirect is refused, even with --allow-http, and no header crosses it", func(t *testing.T) {
		// The profile even lists the http:// origin; nothing may reach it.
		e.json("auth-profiles", "add", "--id", "tlsvendor", "--origin", tlsSource.URL, "--origin", vendor.URL, "--header-env", "X-Api-Key=VENDOR_KEY")
		before := vendorSeen.count()
		for _, extra := range [][]string{nil, {"--allow-http"}} {
			args := append([]string{"models", "import", tlsSource.URL + "/dl/mystery.gguf", "--auth-profile", "tlsvendor", "--task", "chat",
				"--id", "downgraded", "--yes"}, extra...)
			r := e.cli(nil, args...)
			if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "refusing a redirect from HTTPS to plain HTTP") {
				t.Fatalf("%v: %d %s%s", extra, r.code, r.stdout, r.stderr)
			}
		}
		if vendorSeen.count() != before {
			t.Fatal("a request crossed the HTTPS to HTTP downgrade")
		}
		if fileExists(filepath.Join(e.root, "models", "imported", "downgraded")) {
			t.Fatal("a downgraded download was published")
		}
		// A Hugging Face download hop to an http:// CDN is refused the same way.
		cdnBefore := cdnSeen.count()
		r := e.cli([]string{"HF_ENDPOINT=" + tlsHF.URL, "HF_TOKEN=" + hfToken}, "models", "import", "hf:org/plain-cdn", "--id", "hf-downgraded")
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "refusing a redirect from HTTPS to plain HTTP") {
			t.Fatalf("HF downgrade: %d %s%s", r.code, r.stdout, r.stderr)
		}
		if cdnSeen.count() != cdnBefore || cdnSeen.contains(hfToken) {
			t.Fatal("the HF download reached the http:// CDN")
		}
	})

	step("AC3 unknown metadata leaves the backend blank and every backend selectable; AC5 imports finish without a backend", func(t *testing.T) {
		sel := e.json("models", "backend", "mystery")["selection"].(map[string]any)
		if sel["backendId"] != "" || sel["mode"] != "auto" {
			t.Fatalf("unknown architecture must leave the backend blank: %v", sel)
		}
		reg := e.json("backends", "list")["backends"].([]any)
		if opts := sel["recommendation"].(map[string]any)["options"].([]any); len(opts) != len(reg) {
			t.Fatalf("%d options for %d registered backends", len(opts), len(reg))
		}
	})

	step("AC8 an unpinned URL records an observed hash; a trusted-checksum mismatch blocks publication", func(t *testing.T) {
		m := e.json("models", "export", "mystery")["model"].(map[string]any)
		a := m["variants"].([]any)[0].(map[string]any)["artifacts"].([]any)[0].(map[string]any)
		if a["hashTrust"] != "observed" || a["sha256"] != hexsum(mystery) {
			t.Fatalf("artifact %v", a)
		}
		r := e.cli(nil, "models", "import", vendor.URL+"/files/mystery.gguf", "--auth-profile", "vendor", "--allow-http", "--task", "chat",
			"--id", "pinned", "--yes", "--sha256", strings.Repeat("0", 64))
		if r.code == 0 || fileExists(filepath.Join(e.root, "models", "imported", "pinned")) {
			t.Fatalf("checksum mismatch published: %s%s", r.stdout, r.stderr)
		}
	})

	step("AC2 a GGUF extension is not evidence; an incomplete multi-file model names its missing files", func(t *testing.T) {
		dir := filepath.Join(tmp, "split")
		os.MkdirAll(dir, 0o755)
		for i := 1; i <= 2; i++ {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("big-%05d-of-00003.gguf", i)), ggufFile(t, "qwen3", map[string]any{"split.count": 3, "split.no": i - 1}, 4096), 0o644)
		}
		r := e.cli(nil, "models", "import", filepath.Join(dir, "big-00001-of-00003.gguf"), "--task", "chat")
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "big-00003-of-00003.gguf") {
			t.Fatalf("incomplete split: %s%s", r.stdout, r.stderr)
		}
		os.WriteFile(filepath.Join(tmp, "llama-3-8b-instruct.Q4_K_M.gguf"), []byte("not a model at all, only a name"), 0o644)
		e.json("models", "import", filepath.Join(tmp, "llama-3-8b-instruct.Q4_K_M.gguf"), "--task", "chat", "--id", "named-only")
		sel := e.json("models", "backend", "named-only")["selection"].(map[string]any)
		if sel["backendId"] != "" {
			t.Fatalf("a file name produced a backend: %v", sel)
		}
		for _, o := range sel["recommendation"].(map[string]any)["options"].([]any) {
			if r := o.(map[string]any)["rating"]; r == "supported" || r == "likely" {
				t.Fatalf("a file name produced a %s rating: %v", r, o)
			}
		}
	})

	step("OP4 models inspect --json has a findings table with codes and sentences", func(t *testing.T) {
		d := e.json("models", "inspect", "vision-model")
		findings := d["findings"].([]any)
		backends := e.json("backends", "list")["backends"].([]any)
		if len(findings) != len(backends) {
			t.Fatalf("%d findings for 1 variant and %d backends", len(findings), len(backends))
		}
		for _, f := range findings {
			for _, r := range f.(map[string]any)["reasons"].([]any) {
				rm := r.(map[string]any)
				if rm["code"] == "" || !strings.HasSuffix(rm["message"].(string), ".") {
					t.Fatalf("reason %v", rm)
				}
			}
		}
	})

	step("OP5 CrispASR is automatic only where pinned, and only for speech", func(t *testing.T) {
		// Pinned: the macOS arm64 release. Elsewhere there is no managed
		// package: "not installed", never automatic.
		pinned := goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64"
		for _, b := range e.json("backends", "list")["backends"].([]any) {
			bm := b.(map[string]any)
			id := bm["id"].(string)
			if !strings.HasPrefix(id, "crispasr") {
				continue
			}
			switch {
			case pinned && id != "crispasr-vulkan":
				if bm["install"] != "installable" || bm["installLabel"] != "installs on first use" || bm["automatic"] != true ||
					fmt.Sprint(bm["automaticTasks"]) != "[speech]" {
					t.Fatalf("pinned CrispASR in backends list: %v", bm)
				}
			case bm["install"] != "missing" || bm["installLabel"] != "not installed (no managed package yet)":
				t.Fatalf("unpinned CrispASR in backends list: %v", bm)
			}
		}
		if r := e.cli(nil, "backends", "list"); r.code != 0 || !strings.Contains(r.stdout, "not installed (no managed package yet)") {
			t.Fatalf("human backends list: %s%s", r.stdout, r.stderr)
		}
		// A speech model CrispASR is the routing table's first choice for.
		crisp := "crispasr-metal"
		if goruntime.GOOS == "linux" {
			crisp = "crispasr-cpu"
		}
		define := func(id, task, arch string, artifacts ...any) map[string]any {
			def := map[string]any{"id": id, "name": id, "tasks": []string{task}, "capabilities": []string{},
				"origin": "import", "variants": []any{map[string]any{"id": "q8",
					"format": map[string]any{"value": "gguf", "source": "declared"}, "architecture": map[string]any{"value": arch, "source": "declared"},
					"precision": map[string]any{}, "contextLength": map[string]any{}, "dimensions": map[string]any{}, "artifacts": artifacts}}}
			b, _ := json.Marshal(def)
			file := filepath.Join(tmp, id+".json")
			os.WriteFile(file, b, 0o644)
			return e.json("models", "add-definition", file)["selection"].(map[string]any)
		}
		local := map[string]any{"kind": "local"}
		sel := define("kokoro-def", "speech", "kokoro",
			map[string]any{"filename": "kokoro.gguf", "role": "weights", "required": true, "source": local},
			map[string]any{"filename": "af_heart.bin", "role": "voice", "required": true, "source": local})
		if want := map[bool]string{true: crisp, false: ""}[pinned]; sel["backendId"] != want {
			t.Fatalf("Kokoro automatic backend %q, want %q: %v", sel["backendId"], want, sel)
		}
		found := false
		for _, o := range sel["recommendation"].(map[string]any)["options"].([]any) {
			om := o.(map[string]any)
			if om["backendId"] != crisp {
				continue
			}
			found = true
			codes := strings.Join(warningCodes(om), ",")
			if om["rating"] != "supported" {
				t.Fatalf("%s should rate this Kokoro variant supported: %v", crisp, om)
			}
			if pinned && (om["install"] != "installable" || !strings.Contains(codes, "installs-on-first-use")) ||
				!pinned && (om["install"] != "missing" || !strings.Contains(codes, "no-managed-package") || !strings.Contains(codes, "not-chosen-automatically")) {
				t.Fatalf("CrispASR option %v", om)
			}
		}
		if !found {
			t.Fatalf("%s is not listed", crisp)
		}
		// Recognition stays unqualified: a GGUF whisper model never goes to
		// CrispASR automatically, wherever it is pinned.
		sel = define("whisper-def", "transcription", "whisper",
			map[string]any{"filename": "whisper.gguf", "role": "weights", "required": true, "source": local})
		if strings.HasPrefix(fmt.Sprint(sel["backendId"]), "crispasr") {
			t.Fatalf("CrispASR chosen automatically for transcription: %v", sel)
		}
		for _, o := range sel["recommendation"].(map[string]any)["options"].([]any) {
			om := o.(map[string]any)
			if om["backendId"] == crisp && !strings.Contains(strings.Join(warningCodes(om), ","), "not-chosen-automatically") {
				t.Fatalf("CrispASR transcription option: %v", om)
			}
		}
		// Choosing it by hand is allowed.
		s := e.json("models", "backend", "kokoro-def", "--set", crisp)["selection"].(map[string]any)
		if s["backendId"] != crisp || s["mode"] != "manual" {
			t.Fatalf("manual CrispASR: %v", s)
		}
		e.json("models", "backend", "kokoro-def", "--auto")
	})

	step("AC5 overrides report real startup errors without bypassing integrity or resource checks", func(t *testing.T) {
		e.serve()
		if r := e.chatRetrying("vision-model"); r.Status != 200 || !strings.Contains(string(r.Body), "hello there") {
			t.Fatalf("automatic backend chat: %d %s", r.Status, r.Body)
		}
		wantCode(t, e.chat("mystery"), 409, "backend_not_selected")
		if s := e.setBackend("mystery", "llama.cpp-cpu"); s["mode"] != "manual" || s["finding"].(map[string]any)["rating"] == "supported" {
			t.Fatalf("manual unknown choice: %v", s)
		}
		if r := e.chatRetrying("mystery"); r.Status != 200 {
			t.Fatalf("manual CPU backend chat: %d %s", r.Status, r.Body)
		}
		if s := e.setBackend("mystery", wrongTask); s["finding"].(map[string]any)["rating"] != "incompatible" {
			t.Fatalf("incompatible finding: %v", s)
		}
		r := e.chat("mystery")
		wantCode(t, r, 503, "backend_startup_failed")
		if !strings.Contains(string(r.Body), "does not run chat models") {
			t.Fatalf("startup error: %s", r.Body)
		}
	})

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

	step("AC9 a custom adapter declares support through the same versioned interface", func(t *testing.T) {
		e.json("backends", "add", "--file", def("fake-custom", 1))
		e.json("backends", "add", "--file", def("fake-hungry", 1e7))
		if s := e.setBackend("mystery", "fake-custom"); s["finding"].(map[string]any)["rating"] != "supported" {
			t.Fatalf("custom backend finding: %v", s)
		}
		if r := e.chat("mystery"); r.Status != 200 {
			t.Fatalf("custom backend chat: %d %s", r.Status, r.Body)
		}
	})

	step("AC5 an override never bypasses memory admission", func(t *testing.T) {
		e.setBackend("mystery", "fake-hungry")
		wantCode(t, e.chat("mystery"), 503, "insufficient_memory")
	})

	step("AC4 manual selections survive reinspection and restart", func(t *testing.T) {
		e.json("models", "inspect", "mystery")
		e.stop()
		e.serve()
		if s := e.selection("mystery"); s["mode"] != "manual" || s["backendId"] != "fake-hungry" {
			t.Fatalf("after restart: %v", s)
		}
	})

	step("OP1 exports carry no selection or device hash; another root and device start in automatic mode", func(t *testing.T) {
		exp := e.cli(nil, "--json", "models", "export", "mystery")
		for _, s := range []string{"device-one", "fake-hungry", "manual", "selection"} {
			if strings.Contains(exp.stdout, s) {
				t.Fatalf("export contains %q: %s", s, exp.stdout)
			}
		}
		for _, args := range [][]string{{"--json", "models", "backend", "mystery"}, {"--json", "models", "inspect", "mystery"}} {
			if out := e.cli(nil, args...); strings.Contains(out.stdout, "device-one") {
				t.Fatalf("%v shows the device hash", args)
			}
		}
		file := filepath.Join(tmp, "mystery-export.json")
		os.WriteFile(file, []byte(exp.stdout), 0o644)
		// The exported bytes: no selection, device id (override or hash) or credential.
		exported, _ := os.ReadFile(file)
		e.scan("exported model", string(exported))
		for _, s := range []string{"device", "backendId", realDeviceID(t)} {
			if strings.Contains(string(exported), s) {
				t.Fatalf("export contains %q", s)
			}
		}
		second := &e2e{t: t, root: filepath.Join(tmp, "root-two"), outDir: tmp, env: []string{"LOCALBASE_DEVICE_ID=device-two"}}
		if r := second.cli(nil, "init"); r.code != 0 {
			t.Fatalf("init second root: %s", r.stderr)
		}
		d := second.json("models", "add-definition", file)
		if sel := d["selection"].(map[string]any); sel["mode"] != "auto" {
			t.Fatalf("second root inherited a manual choice: %v", sel)
		}
		if n := selectionRows(t, filepath.Join(second.root, "local-base.db")); n != 0 {
			t.Fatalf("the second root has %d selection rows", n)
		}
		if n := selectionRows(t, filepath.Join(e.root, "local-base.db")); n == 0 {
			t.Fatal("the first root lost its manual choice")
		}
		// The same root on another device is automatic too.
		d2 := e.cli([]string{"LOCALBASE_DEVICE_ID=device-two"}, "--json", "models", "backend", "mystery")
		if !strings.Contains(d2.stdout, `"mode":"auto"`) {
			t.Fatalf("another device inherited the choice: %s", d2.stdout)
		}
	})

	step("AC4 a removed backend is kept as unavailable; reset to automatic restores detection", func(t *testing.T) {
		e.json("backends", "remove", "fake-hungry")
		if s := e.selection("mystery"); s["backendId"] != "fake-hungry" || s["unavailable"] != true {
			t.Fatalf("removed backend: %v", s)
		}
		wantCode(t, e.chat("mystery"), 409, "backend_unavailable")
		if s := e.setBackend("mystery", ""); s["mode"] != "auto" || s["backendId"] != "" {
			t.Fatalf("reset: %v", s)
		}
		wantCode(t, e.chat("mystery"), 409, "backend_not_selected")
	})

	step("AC10 OpenAI-compatible routes are unchanged", func(t *testing.T) {
		list := e.do("GET", "/v1/models", "")
		var body struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		if list.Status != 200 || json.Unmarshal(list.Body, &body) != nil || body.Object != "list" {
			t.Fatalf("models list: %s", list.Body)
		}
		found := false
		for _, m := range body.Data {
			found = found || (m.ID == "vision-model" && m.Object == "model")
		}
		if !found {
			t.Fatalf("imported model not listed: %s", list.Body)
		}
	})

	step("OP2 no output, page, log, export or database contains a planted secret", func(t *testing.T) {
		if r := e.do("GET", "/_localbase/auth-profiles", ""); r.Status != 200 || !strings.Contains(string(r.Body), `"X-Api-Key"`) {
			t.Fatalf("auth profiles: %d %s", r.Status, r.Body)
		}
		for _, p := range []string{"/_localbase/backends", "/_localbase/model-backend?model=mystery", "/_localbase/model-backend?model=vision-model", "/_localbase/models"} {
			e.do("GET", p, "")
		}
		e.json("auth-profiles", "list")
		e.json("models", "export", "moved")
		e.stop()
		// The UI pages, through a loopback --no-auth gateway.
		e.serve("--no-auth")
		e.noAuth = true
		for _, p := range []string{"/app/models/import", "/app/backends", "/app/models/m/mystery", "/app/models/m/moved", "/app/models/m/vision-model", "/app/models/get"} {
			r := e.do("GET", p, "")
			if r.Status != 200 {
				t.Fatalf("%s: %d", p, r.Status)
			}
		}
		if r := e.do("GET", "/app/models/m/kokoro-def", ""); r.Status != 200 || !strings.Contains(string(r.Body), "not installed (no managed package yet)") {
			t.Fatalf("CrispASR in the selector: %d", r.Status)
		}
		// A page gives the browser its CSRF cookie and token, as htmx sends them.
		pageReq, _ := http.NewRequest("GET", e.base+"/app/models/import", nil)
		pageResp, err := http.DefaultClient.Do(pageReq)
		if err != nil {
			t.Fatal(err)
		}
		pageBody, _ := io.ReadAll(pageResp.Body)
		pageResp.Body.Close()
		var csrfCookie *http.Cookie
		for _, c := range pageResp.Cookies() {
			if c.Name == "lb_csrf" {
				csrfCookie = c
			}
		}
		csrf := regexp.MustCompile(`"X-CSRF-Token": "([0-9a-f]{64})"`).FindStringSubmatch(string(pageBody))
		if csrfCookie == nil || csrf == nil {
			t.Fatalf("no CSRF cookie or token on the page")
		}
		form := "source=" + vendor.URL + "/files/mystery.gguf%3Fsig%3D" + signedValue + "&task=chat&auth_profile=vendor&allow_http=1&id=ui-mystery"
		req, _ := http.NewRequest("POST", e.base+"/app/models/import/resolve", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Localbase-UI", "1")
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrf[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		e.scan("import resolve page", string(b))
		if !strings.Contains(string(b), "custom source") {
			t.Fatalf("resolve page: %s", b)
		}
		// Finish the import through the page: only the token travels.
		m := regexp.MustCompile(`name="token" value="([0-9a-f-]{36})"`).FindStringSubmatch(string(b))
		if m == nil {
			t.Fatalf("no token in the resolve page: %s", b)
		}
		post := func(path, form string) string {
			req, _ := http.NewRequest("POST", e.base+path, strings.NewReader(form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			req.Header.Set("X-Localbase-UI", "1")
			req.AddCookie(csrfCookie)
			req.Header.Set("X-CSRF-Token", csrf[1])
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			e.scan(path, string(b))
			return string(b)
		}
		if page := post("/app/models/import/start", "token="+m[1]+"&backend=auto"); !strings.Contains(page, "Confirm that you trust") {
			t.Fatalf("a custom source started without consent: %s", page)
		}
		// The token survives a refused start; a new check gives a new one anyway.
		page := post("/app/models/import/start", "token="+m[1]+"&backend=auto&confirm=1")
		job := regexp.MustCompile(`/app/models/import/jobs/(imp_[0-9a-f-]{36})`).FindStringSubmatch(page)
		if job == nil {
			t.Fatalf("no job: %s", page)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			req, _ := http.NewRequest("GET", e.base+"/app/models/import/jobs/"+job[1], nil)
			req.Header.Set("HX-Request", "true")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			e.scan("import job", string(b))
			if strings.Contains(string(b), "Imported") {
				break
			}
			if strings.Contains(string(b), "import failed") || time.Now().After(deadline) {
				t.Fatalf("UI import: %d %q (job %s, start page %q)", resp.StatusCode, b, job[1], page)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if page := post("/app/models/import/start", "token="+m[1]+"&backend=auto&confirm=1"); !strings.Contains(page, "expired") {
			t.Fatalf("a used token was accepted again: %s", page)
		}
		e.stop()
		files, _ := filepath.Glob(filepath.Join(e.root, "logs", "*"))
		more, _ := filepath.Glob(filepath.Join(tmp, "serve-*.log"))
		for _, f := range append(files, more...) {
			b, _ := os.ReadFile(f)
			e.scan(filepath.Base(f), string(b))
		}
		db, _ := os.ReadFile(filepath.Join(e.root, "local-base.db"))
		e.scan("local-base.db", string(db))
	})

	step("rollback: TS opens the database Go wrote", func(t *testing.T) {
		if _, err := exec.LookPath("bun"); err != nil || *implFlag != "both" {
			t.Skip("needs bun and -impl=both")
		}
		if r := runCLI(t, "ts", e.root, "--json", "keys", "list"); r.code != 0 {
			t.Fatalf("TS could not open the database: %s%s", r.stdout, r.stderr)
		}
	})
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// selectionRows counts stored backend choices in a database.
func selectionRows(t *testing.T, db string) int {
	t.Helper()
	st, err := store.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM model_backend_selections`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// realDeviceID is this machine's hashed device id (without the test override).
func realDeviceID(t *testing.T) string {
	t.Helper()
	old, had := os.LookupEnv("LOCALBASE_DEVICE_ID")
	os.Unsetenv("LOCALBASE_DEVICE_ID")
	defer func() {
		if had {
			os.Setenv("LOCALBASE_DEVICE_ID", old)
		}
	}()
	return backends.DeviceID(backends.ProbeOptions{})
}
