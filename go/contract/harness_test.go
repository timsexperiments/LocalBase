// Package contract is a black-box HTTP contract suite that runs against the
// TypeScript and the Go gateway alike.
//
//	go test ./contract -args -impl=go          # build and launch the Go gateway
//	go test ./contract -args -impl=ts          # launch `bun run src/cli.ts serve`
//	go test ./contract -args -impl=both        # both, plus differential checks
//	go test ./contract -args -url=http://127.0.0.1:P -key=lb_... -keyless-key=lb_...
//	                                           # an already running gateway
//
// Launched gateways get a fresh scratch root in a temp dir and ports in
// 24000-29999. LOCALBASE_CONTRACT_MODELS names a models/llm directory (read
// only) holding qwen2.5-coder-1.5b-instruct-q4_k_m; the TS gateway needs it
// because it refuses to start without the active model's files. Adding
// LOCALBASE_CONTRACT_RUNTIMES (a bin/runtimes directory, copied) runs the
// real-inference cases.
package contract

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var (
	implFlag   = flag.String("impl", "go", "go | ts | both")
	urlFlag    = flag.String("url", "", "test an already running gateway at this base URL")
	keyFlag    = flag.String("key", "", "with -url: a key with default scopes")
	narrowFlag = flag.String("keyless-key", "", "with -url: a key with only inference:chat")
)

const (
	chatModel      = "qwen2.5-coder-1.5b-instruct-q4_k_m"
	decisionModel  = "clef-flash-q4_k_m"
	embeddingModel = "qwen3-embedding-0.6b-q8_0"
)

// Target is one gateway under test.
type Target struct {
	Name      string
	BaseURL   string
	Key       string // default scopes
	ChatOnly  string // inference:chat only
	RealModel bool
	stop      func()
}

var targets []*Target

func TestMain(m *testing.M) {
	flag.Parse()
	code := func() int {
		if *urlFlag != "" {
			targets = []*Target{{Name: "external", BaseURL: strings.TrimRight(*urlFlag, "/"), Key: *keyFlag, ChatOnly: *narrowFlag}}
			return m.Run()
		}
		impls := []string{*implFlag}
		if *implFlag == "both" {
			impls = []string{"ts", "go"}
		}
		for _, impl := range impls {
			t, err := launch(impl)
			if err != nil {
				fmt.Fprintf(os.Stderr, "launch %s: %v\n", impl, err)
				for _, t := range targets {
					t.stop()
				}
				return 1
			}
			targets = append(targets, t)
		}
		defer func() {
			for _, t := range targets {
				t.stop()
			}
		}()
		return m.Run()
	}()
	os.Exit(code)
}

func freePort() int {
	for {
		p := 24000 + rand.IntN(6000)
		if p == 24173 || (p >= 25611 && p <= 25613) {
			continue
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			ln.Close()
			return p
		}
	}
}

// fixture builds a scratch root both implementations accept: marker, a
// migrated database with a config row and two keys, the remote feed off,
// and optionally the real chat model and llama-server package.
func fixture(dir string, llmPort int) (key, chatOnly string, real bool, err error) {
	r, err := root.Init(dir)
	if err != nil {
		return "", "", false, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return "", "", false, err
	}
	defer st.Close()
	sel, _ := json.Marshal([]string{chatModel, decisionModel, embeddingModel})
	_, err = st.DB.Exec(`INSERT INTO config (id, root, gateway_host, gateway_port, host, port, ctx_size, ctx_mode,
		stt_host, stt_port, selected_llm_models, selected_stt_models, selected_tts_models, selected_image_models,
		selected_video_models, allow_experimental, active_llm_model, active_stt_model, active_tts_model,
		active_image_model, active_video_model, hf_token, install_missing_models, parallel, otel_endpoint,
		otel_headers, otel_sample_ratio, memory_system_reserve_percent, memory_system_reserve_minimum_gb,
		memory_accelerator_reserve_percent, memory_accelerator_reserve_minimum_gb)
		VALUES ('default', ?, '127.0.0.1', 2273, '127.0.0.1', ?, 65536, 'auto', '127.0.0.1', ?, ?, '[]', '[]', '[]', '[]',
		0, ?, '', '', '', '', '', 0, '1', '', '', 100, 15, 8, 10, 2)`,
		r.Dir, llmPort, freePort(), string(sel), chatModel)
	if err != nil {
		return "", "", false, err
	}
	_, key, err = st.CreateAPIKey(context.Background(), "contract", []string{
		"inference:chat", "inference:embeddings", "inference:decision", "inference:image", "inference:video",
		"inference:speech", "inference:transcription", "models:read"})
	if err != nil {
		return "", "", false, err
	}
	_, chatOnly, err = st.CreateAPIKey(context.Background(), "contract-chat-only", []string{"inference:chat"})
	if err != nil {
		return "", "", false, err
	}
	os.WriteFile(filepath.Join(r.Dir, "catalog-feed.json"), []byte(`{"url":null}`), 0o600)
	os.MkdirAll(r.ModelsDir("llm"), 0o700)
	modelLinked := false
	if models := os.Getenv("LOCALBASE_CONTRACT_MODELS"); models != "" {
		src := filepath.Join(models, "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf")
		if _, err := os.Stat(src); err == nil {
			modelLinked = os.Symlink(src, filepath.Join(r.ModelsDir("llm"), filepath.Base(src))) == nil
		}
		// The 0.6 GB embedding model, when present, for the embeddings cases.
		if emb := filepath.Join(models, "Qwen3-Embedding-0.6B-Q8_0.gguf"); fileExists(emb) {
			os.Symlink(emb, filepath.Join(r.ModelsDir("llm"), filepath.Base(emb)))
		}
	}
	if runtimes := os.Getenv("LOCALBASE_CONTRACT_RUNTIMES"); runtimes != "" && modelLinked {
		{
			// Copy, never link, the runtime package: backends must not run from the live root.
			os.MkdirAll(filepath.Join(r.Dir, "bin"), 0o700)
			out, cerr := exec.Command("cp", "-R", runtimes, filepath.Join(r.Dir, "bin", "runtimes")).CombinedOutput()
			if cerr != nil {
				return "", "", false, fmt.Errorf("copy runtimes: %v %s", cerr, out)
			}
			real = true
		}
	}
	return key, chatOnly, real, nil
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Dir(filepath.Dir(wd)) // go/contract -> repo
}

func launch(impl string) (*Target, error) {
	dir, err := os.MkdirTemp("", "lbgo-contract-"+impl+"-")
	if err != nil {
		return nil, err
	}
	dir, _ = filepath.EvalSymlinks(dir)
	rootDir := filepath.Join(dir, "root")
	port, llmPort := freePort(), freePort()
	key, chatOnly, real, err := fixture(rootDir, llmPort)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	args := []string{"--root", rootDir, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--llm-port", fmt.Sprint(llmPort)}
	switch impl {
	case "go":
		bin := filepath.Join(dir, "local-base")
		if out, err := exec.Command("go", "build", "-o", bin, "../cmd/localbase").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build: %v %s", err, out)
		}
		cmd = exec.Command(bin, args...)
	case "ts":
		cmd = exec.Command("bun", append([]string{"run", "src/cli.ts"}, args...)...)
		cmd.Dir = repoRoot()
	default:
		return nil, fmt.Errorf("unknown impl %q", impl)
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LOCALBASE_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "LOCALBASE_CATALOG_FEED_URL=off")
	logf, _ := os.Create(filepath.Join(dir, "serve.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := func() {
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
		if os.Getenv("LOCALBASE_CONTRACT_KEEP") == "" {
			os.RemoveAll(dir)
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
	return &Target{Name: impl, BaseURL: base, Key: key, ChatOnly: chatOnly, RealModel: real, stop: stop}, nil
}

// --- request helpers ---------------------------------------------------------

type Resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *Resp) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

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

func do(t *testing.T, method, url string, headers map[string]string, body string) *Resp {
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

func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

// each runs f as a subtest per target.
func each(t *testing.T, f func(t *testing.T, tg *Target)) {
	for _, tg := range targets {
		t.Run(tg.Name, func(t *testing.T) { f(t, tg) })
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
