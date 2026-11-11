// Package harness launches the LocalBase gateways the black-box suites test (go/test/contract): the
// shared targets the matrix tests run against (Targets), a gateway of a test's own
// (Launch), the scratch root a gateway serves (Fixture), and the request helpers (Do). Its
// writes go through fsguard. It is for test packages only: no non-test file outside go/test
// and go/tools may import it (TestNoProductionImports).
//
// The flags -impl, -url, -key and -keyless-key are registered here, so every test binary that
// imports the harness takes them. go/test/contract's package doc lists them and the environment.
//
// The shared targets are launched by the first test that asks for them, once per test binary,
// and stopped by Main when the package's tests end. A package whose tests never ask launches
// none.
package harness

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

var (
	implFlag   = flag.String("impl", "go", "go | ts | both")
	urlFlag    = flag.String("url", "", "test an already running gateway at this base URL")
	keyFlag    = flag.String("key", "", "with -url: a key with default scopes")
	narrowFlag = flag.String("keyless-key", "", "with -url: a key with only inference:chat")
)

const (
	ChatModel      = "qwen2.5-coder-1.5b-instruct-q4_k_m"
	DecisionModel  = "clef-flash-q4_k_m"
	EmbeddingModel = "qwen3-embedding-0.6b-q8_0"
)

// Target is one gateway under test.
type Target struct {
	Name      string
	BaseURL   string
	Key       string // default scopes
	ChatOnly  string // inference:chat only
	Admin     string // every permission
	RealModel bool
	Root      string // scratch root of a launched gateway
	stop      func()
	// halt stops the process and keeps its files; stop also removes them.
	halt func()
}

// Stop stops a launched gateway and removes its scratch root (LOCALBASE_CONTRACT_KEEP keeps it).
// It does nothing for an external target.
func (tg *Target) Stop() {
	if tg.stop != nil {
		tg.stop()
	}
}

// Halt stops a launched gateway's process and keeps its files, for checks on what it left.
// It does nothing for an external target.
func (tg *Target) Halt() {
	if tg.halt != nil {
		tg.halt()
	}
}

// sharedTargets is the targets the matrix tests run against: launched by the first test that
// asks, once per test binary, and stopped by Main.
type sharedTargets struct {
	once     sync.Once
	launched bool // a launch was attempted
	targets  []*Target
	err      error
}

var shared = &sharedTargets{}

// launchShared is what the first Targets call runs. A test replaces it to pin Main's lifecycle
// without a gateway.
var launchShared = launchTargets

// Impl is the -impl flag: go, ts or both.
func Impl() string { return *implFlag }

// Targets returns the shared targets, launching them on the first call. A launch failure fails
// every test that asks, with the launch's error.
func Targets(t testing.TB) []*Target {
	t.Helper()
	shared.once.Do(func() {
		shared.launched = true
		shared.targets, shared.err = launchShared()
	})
	if shared.err != nil {
		t.Fatalf("shared targets: %v", shared.err)
	}
	return shared.targets
}

// Main runs a package's tests and then stops the shared targets, if a test launched them. A
// package's TestMain calls it: os.Exit(harness.Main(m)), with its own cleanup after.
func Main(m *testing.M) int { return run(m.Run) }

func run(tests func() int) int {
	flag.Parse()
	defer func() {
		for _, t := range shared.targets {
			t.Stop()
		}
	}()
	return tests()
}

// launchTargets launches what -url or -impl asks for.
func launchTargets() ([]*Target, error) {
	if *urlFlag != "" {
		// The key may come from the environment, so it never appears on a command line.
		if *keyFlag == "" {
			*keyFlag = os.Getenv("LOCALBASE_CONTRACT_KEY")
		}
		return []*Target{{Name: "external", BaseURL: strings.TrimRight(*urlFlag, "/"), Key: *keyFlag, ChatOnly: *narrowFlag}}, nil
	}
	impls := []string{*implFlag}
	if *implFlag == "both" {
		impls = []string{"ts", "go"}
	}
	var targets []*Target
	for _, impl := range impls {
		t, err := Launch(impl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "launch %s: %v\n", impl, err)
			for _, t := range targets {
				t.stop()
			}
			return nil, fmt.Errorf("launch %s: %v", impl, err)
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// Fixture builds a scratch root both implementations accept: marker, a
// migrated database with a config row and two keys, the remote feed off,
// and optionally the real chat model and llama-server package.
//
// Fixture takes NoReserve; Env is for Launch and does nothing here.
func Fixture(dir string, llmPort int, opts ...Option) (key, chatOnly, admin string, real bool, err error) {
	o := optionsOf(opts)
	r, err := root.Init(dir)
	if err != nil {
		return "", "", "", false, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return "", "", "", false, err
	}
	defer st.Close()
	sel, _ := json.Marshal([]string{ChatModel, DecisionModel, EmbeddingModel})
	// A real whisper model (an APFS clone), when LOCALBASE_CONTRACT_STT_MODELS has it.
	sttSel := `["whisper-base-en-q8_0"]`
	if d := os.Getenv("LOCALBASE_CONTRACT_STT_MODELS"); d != "" && fileExists(filepath.Join(d, "ggml-tiny.en-q8_0.bin")) {
		fsguard.MkdirAll(r.ModelsDir("stt"), 0o700)
		if err := fsguard.CloneModel(filepath.Join(d, "ggml-tiny.en-q8_0.bin"), filepath.Join(r.ModelsDir("stt"), "ggml-tiny.en-q8_0.bin")); err != nil {
			return "", "", "", false, err
		}
		sttSel = `["whisper-base-en-q8_0","whisper-tiny-en-q8_0"]`
	}
	if err := linkCrispASR(r); err != nil {
		return "", "", "", false, err
	}
	// The real FastWan video files (APFS clones), when
	// LOCALBASE_CONTRACT_VIDEO_MODELS has them (an operator-approved memory window).
	if d := os.Getenv("LOCALBASE_CONTRACT_VIDEO_MODELS"); d != "" {
		fsguard.MkdirAll(r.ModelsDir("video"), 0o700)
		for _, f := range []string{"FastWan2.2-TI2V-5B-q6_k.gguf", "umt5-xxl-encoder-Q8_0.gguf", "taew2_2.safetensors"} {
			if fileExists(filepath.Join(d, f)) {
				if err := fsguard.CloneModel(filepath.Join(d, f), filepath.Join(r.ModelsDir("video"), f)); err != nil {
					return "", "", "", false, err
				}
			}
		}
	}
	// A real image model (an APFS clone), when LOCALBASE_CONTRACT_IMAGE_MODELS has it.
	imageSel := `["dreamshaper-8-lcm","flux1-schnell-q4_0","realvis-xl-v5.0","sdxl-turbo","juggernaut-xl-v9"]`
	if d := os.Getenv("LOCALBASE_CONTRACT_IMAGE_MODELS"); d != "" && fileExists(filepath.Join(d, imageFile)) {
		fsguard.MkdirAll(r.ModelsDir("image"), 0o700)
		if err := fsguard.CloneModel(filepath.Join(d, imageFile), filepath.Join(r.ModelsDir("image"), imageFile)); err != nil {
			return "", "", "", false, err
		}
		imageSel = `["dreamshaper-8-lcm","flux1-schnell-q4_0","realvis-xl-v5.0","sdxl-turbo","juggernaut-xl-v9","dreamshaper-v8"]`
	}
	_, err = st.DB.Exec(`INSERT INTO config (id, root, gateway_host, gateway_port, host, port, ctx_size, ctx_mode,
		stt_host, stt_port, selected_llm_models, selected_stt_models, selected_tts_models, selected_image_models,
		selected_video_models, allow_experimental, active_llm_model, active_stt_model, active_tts_model,
		active_image_model, active_video_model, hf_token, install_missing_models, parallel, otel_endpoint,
		otel_headers, otel_sample_ratio, memory_system_reserve_percent, memory_system_reserve_minimum_gb,
		memory_accelerator_reserve_percent, memory_accelerator_reserve_minimum_gb)
		VALUES ('default', ?, '127.0.0.1', 2273, '127.0.0.1', ?, 65536, 'auto', '127.0.0.1', ?, ?, ?, ?, ?, ?,
		1, ?, 'whisper-base-en-q8_0', 'qwen3-tts-1.7b-base-q4_k_m', 'dreamshaper-8-lcm', 'wan2.1-t2v-1.3b-q8_0', '', 0, '1', '', '', 100, 15, 8, 10, 2)`,
		r.Dir, llmPort, testutil.FreePort(), string(sel), sttSel, `["qwen3-tts-1.7b-base-q4_k_m","kokoro-82m-v1.0-q8"]`,
		imageSel, `["wan2.1-t2v-1.3b-q8_0","fastwan2.2-ti2v-5b-q6_k"]`, ChatModel)
	if err != nil {
		return "", "", "", false, err
	}
	if o.noReserve {
		if _, err = st.DB.Exec(`UPDATE config SET memory_system_reserve_percent = 1, memory_system_reserve_minimum_gb = 0,
			memory_accelerator_reserve_percent = 1, memory_accelerator_reserve_minimum_gb = 0 WHERE id = 'default'`); err != nil {
			return "", "", "", false, err
		}
	}
	_, key, err = st.CreateAPIKey(context.Background(), "contract", []string{
		"inference:chat", "inference:embeddings", "inference:decision", "inference:image", "inference:video",
		"inference:speech", "inference:transcription", "models:read"})
	if err != nil {
		return "", "", "", false, err
	}
	_, chatOnly, err = st.CreateAPIKey(context.Background(), "contract-chat-only", []string{"inference:chat"})
	if err != nil {
		return "", "", "", false, err
	}
	_, admin, err = st.CreateAPIKey(context.Background(), "contract-admin", auth.Permissions)
	if err != nil {
		return "", "", "", false, err
	}
	fsguard.WriteFile(filepath.Join(r.Dir, "catalog-feed.json"), []byte(`{"url":null}`), 0o600)
	if err := addTinyCatalogModel(r.Dir, r.ModelsDir("stt")); err != nil {
		return "", "", "", false, err
	}
	// Active non-LLM models: sparse files at the expected sizes, which pass the
	// install check of both implementations without real weights.
	cat, _ := catalog.LoadWithFormer()
	for _, id := range []string{"whisper-base-en-q8_0", "qwen3-tts-1.7b-base-q4_k_m", "dreamshaper-8-lcm", "wan2.1-t2v-1.3b-q8_0"} {
		spec := cat.ByID(id)
		dir := r.ModelsDir(spec.Kind)
		fsguard.MkdirAll(dir, 0o700)
		// The real Qwen3-TTS files (APFS clones), when LOCALBASE_CONTRACT_TTS_MODELS has them.
		if d := os.Getenv("LOCALBASE_CONTRACT_TTS_MODELS"); d != "" && id == "qwen3-tts-1.7b-base-q4_k_m" && fileExists(filepath.Join(d, spec.Artifacts[0].Filename)) {
			for _, a := range spec.Artifacts {
				if err := fsguard.CloneModel(filepath.Join(d, a.Filename), filepath.Join(dir, a.Filename)); err != nil {
					return "", "", "", false, err
				}
			}
			continue
		}
		for _, a := range spec.Artifacts {
			// A real file cloned in above (the shared umt5 encoder) is left alone.
			if err := fsguard.Sparse(filepath.Join(dir, a.Filename), a.ExpectedSizeBytes); err != nil {
				return "", "", "", false, err
			}
		}
	}
	fsguard.MkdirAll(r.ModelsDir("llm"), 0o700)
	modelLinked := false
	if models := os.Getenv("LOCALBASE_CONTRACT_MODELS"); models != "" {
		src := filepath.Join(models, "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf")
		if _, err := os.Stat(src); err == nil {
			if err := fsguard.CloneModel(src, filepath.Join(r.ModelsDir("llm"), filepath.Base(src))); err != nil {
				return "", "", "", false, err
			}
			modelLinked = true
		}
		// The 0.6 GB embedding model, when present, for the embeddings cases.
		if emb := filepath.Join(models, "Qwen3-Embedding-0.6B-Q8_0.gguf"); fileExists(emb) {
			if err := fsguard.CloneModel(emb, filepath.Join(r.ModelsDir("llm"), filepath.Base(emb))); err != nil {
				return "", "", "", false, err
			}
		}
	}
	if runtimes := os.Getenv("LOCALBASE_CONTRACT_RUNTIMES"); runtimes != "" && modelLinked {
		{
			// Copy, never link, the runtime package: backends must not run from the live root.
			// Merge into bin/runtimes, which other fixtures may already have created.
			if err := fsguard.CopyDir(runtimes, r.RuntimesDir()); err != nil {
				return "", "", "", false, err
			}
			real = true
		}
	}
	if err := fsguard.AssertNoLinks(r.Dir); err != nil {
		return "", "", "", false, err
	}
	return key, chatOnly, admin, real, nil
}

// Option changes what Launch or Fixture builds.
type Option func(*options)

type options struct {
	env       []string
	noReserve bool
}

func optionsOf(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Env adds environment entries to a launched gateway's, after the harness's own (Launch only).
func Env(extra []string) Option { return func(o *options) { o.env = append(o.env, extra...) } }

// NoReserve sets the memory reserves to their minimum.
// It is for a test about something past admission, with a fake backend that uses no
// memory: a CI runner is too small to admit a real model's estimate on the default reserve.
func NoReserve() Option { return func(o *options) { o.noReserve = true } }

// repoRoot is testutil.RepoRoot for code with no test to fail: its failure is an error.
func repoRoot() (string, error) {
	var f rootFailure
	dir := testutil.RepoRoot(&f)
	return dir, f.err
}

type rootFailure struct{ err error }

func (f *rootFailure) Helper() {}

func (f *rootFailure) Fatalf(format string, args ...any) { f.err = fmt.Errorf(format, args...) }

// Launch launches a gateway of its own on a new fixture (Env adds environment entries).
func Launch(impl string, opts ...Option) (*Target, error) {
	o := optionsOf(opts)
	dir, err := fsguard.ScratchTemp("lbgo-contract-" + impl + "-")
	if err != nil {
		return nil, err
	}
	dir, _ = filepath.EvalSymlinks(dir)
	rootDir := filepath.Join(dir, "root")
	port, llmPort := testutil.FreePort(), testutil.FreePort()
	key, chatOnly, admin, real, err := Fixture(rootDir, llmPort, opts...)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	args := []string{"--root", rootDir, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--llm-port", fmt.Sprint(llmPort),
		"--tts-port", fmt.Sprint(testutil.FreePort()), "--image-port", fmt.Sprint(testutil.FreePort()), "--video-port", fmt.Sprint(testutil.FreePort())}
	switch impl {
	case "go":
		bin := filepath.Join(dir, "local-base")
		if out, err := exec.Command("go", GoBuildArgs(bin)...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build: %v %s", err, out)
		}
		cmd = exec.Command(bin, args...)
	case "ts":
		cmd = exec.Command("bun", append([]string{"run", "src/cli.ts"}, args...)...)
		repo, err := repoRoot()
		if err != nil {
			return nil, err
		}
		cmd.Dir = repo
	default:
		return nil, fmt.Errorf("unknown impl %q", impl)
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LOCALBASE_") && !strings.HasPrefix(kv, "HF_TOKEN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// Offline: a missing fixture must fail, not be downloaded (loopback stays reachable).
	cmd.Env = Guarded(append(cmd.Env, "LOCALBASE_CATALOG_FEED_URL=off"))
	cmd.Env = append(cmd.Env, o.env...)
	cmd.Env = append(cmd.Env, CoverEnv()...)
	logf, _ := fsguard.Create(filepath.Join(dir, "serve.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	WatchGateway(cmd.Process.Pid)
	halted := false
	halt := func() {
		if halted {
			return
		}
		halted = true
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
	stop := func() {
		halt()
		if os.Getenv("LOCALBASE_CONTRACT_KEEP") == "" {
			fsguard.RemoveAll(dir)
		}
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			stop()
			return nil, fmt.Errorf("%s gateway did not become healthy; log in %s", impl, filepath.Join(dir, "serve.log"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return &Target{Name: impl, BaseURL: base, Key: key, ChatOnly: chatOnly, Admin: admin, RealModel: real, Root: rootDir, stop: stop, halt: halt}, nil
}

// --- request helpers ---------------------------------------------------------

// Resp is a response Do read in full.
type Resp struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON is the body as a JSON object; it fails the test if it isn't one.
func (r *Resp) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// ErrorCode is the error envelope's code; it fails the test without an envelope, and reports any
// field the envelope lacks.
func (r *Resp) ErrorCode(t *testing.T) any {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope: %s", r.Body)
	}
	for _, k := range []string{"message", "type", "param", "code"} {
		if _, ok := e[k]; !ok {
			t.Errorf("error envelope missing %q: %s", k, r.Body)
		}
	}
	return e["code"]
}

// Do sends one request (a JSON body when body is not empty) and reads the whole response.
func Do(t *testing.T, method, url string, headers map[string]string, body string) *Resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &Resp{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// Bearer is the Authorization header for key.
func Bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// WatchGateway stops a gateway's process group if this test process dies
// first (a panic skips every cleanup), so no scratch gateway or backend
// outlives the suite. The watchdog exits once the gateway is gone.
func WatchGateway(pid int) {
	script := fmt.Sprintf(`while kill -0 %d 2>/dev/null && kill -0 %d 2>/dev/null; do sleep 1; done; `+
		`if kill -0 %d 2>/dev/null; then kill -TERM -%d 2>/dev/null; sleep 10; kill -KILL -%d 2>/dev/null; fi`,
		os.Getpid(), pid, pid, pid, pid)
	w := exec.Command("sh", "-c", script)
	w.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if w.Start() == nil {
		go w.Wait()
	}
}

// GoBuildArgs builds cmd/localbase to out, with coverage when
// LOCALBASE_CONTRACT_COVERDIR is set.
func GoBuildArgs(out string) []string {
	args := []string{"build", "-o", out}
	if os.Getenv("LOCALBASE_CONTRACT_COVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=github.com/timsexperiments/LocalBase/go/...")
	}
	return append(args, "github.com/timsexperiments/LocalBase/go/cmd/localbase")
}

// CoverEnv is what a covered binary needs to write its coverage.
func CoverEnv() []string {
	if d := os.Getenv("LOCALBASE_CONTRACT_COVERDIR"); d != "" {
		return []string{"GOCOVERDIR=" + d}
	}
	return nil
}
