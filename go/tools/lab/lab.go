// Package lab runs real LocalBase gateways for the reliability tools (soak,
// chaos, perf): a real binary and real child processes, driven only from the
// outside by HTTP and signals, over fake backends and a fake memory host.
//
// Everything stays inside one scratch directory, ports come from the block in
// ports.go, and the environment is scrubbed, so a lab gateway cannot reach the
// installed service, its root or the network. The binary is built with the
// localbase_lab tag: only that build has the fake host and the debug listener
// (see internal/memory/host_lab.go and cmd/localbase/lab_debug.go).
package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// The models a lab gateway serves, all fake: sparse files at the catalog's
// sizes, so admission sees real demands without any weights on disk.
const (
	ChatSmall = "qwen3.5-4b-q4_k_m"
	ChatLarge = "qwen3.8-27b-q4_k_m"
	Embedding = "qwen3-embedding-0.6b-q8_0"
	Decision  = "clef-flash-q4_k_m"
	STT       = "whisper-base-q8_0"
	TTS       = "qwen3-tts-1.7b-base-q4_k_m"
	Image     = "dreamshaper-8-lcm"
)

// fixtureModels are the models a fixture installs.
var fixtureModels = []string{ChatSmall, ChatLarge, Embedding, Decision, STT, TTS, Image}

// Workspace is one scratch directory: the binaries it built and the roots and
// logs of the gateways it runs. Close removes it unless Keep is set.
type Workspace struct {
	Dir  string
	Bin  string
	Keep bool
	// moduleDir is go/, where the builds run.
	moduleDir string
	ports     *portBlock
}

// NewWorkspace creates a scratch directory and refuses to work anywhere near
// the installed root.
func NewWorkspace(parent string) (*Workspace, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	dir, err := os.MkdirTemp(parent, "lblab-")
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	if err := refuseLive(dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	module, err := moduleDir()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	w := &Workspace{Dir: dir, Bin: filepath.Join(dir, "bin"), moduleDir: module, ports: newPortBlock()}
	return w, os.MkdirAll(w.Bin, 0o700)
}

// refuseLive stops a path inside the installed root or the installed binaries.
func refuseLive(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, live := range []string{filepath.Join(home, ".local", "share", "local-base"), filepath.Join(home, ".local", "bin")} {
		if real, err := filepath.EvalSymlinks(live); err == nil {
			live = real
		}
		if path == live || strings.HasPrefix(path, live+string(filepath.Separator)) {
			return fmt.Errorf("lab: %s is inside %s", path, live)
		}
	}
	return nil
}

func moduleDir() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("lab: run from inside the go module")
	}
	return filepath.Dir(mod), nil
}

// TakePort reserves one port from the block, for a server a test runs itself.
func (w *Workspace) TakePort() (int, error) {
	ports, err := w.ports.take(1)
	if err != nil {
		return 0, err
	}
	return ports[0], nil
}

// Close stops anything still running from this workspace and removes it.
func (w *Workspace) Close() {
	w.KillStray()
	if !w.Keep {
		os.RemoveAll(w.Dir)
	}
}

// Build compiles the gateway (with the race detector when race is set) and the
// fake backend under its several names.
func (w *Workspace) Build(ctx context.Context, race bool) error {
	args := []string{"build", "-tags", "localbase_lab"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "-o", filepath.Join(w.Bin, "local-base"), "./cmd/localbase")
	if err := w.goBuild(ctx, args); err != nil {
		return err
	}
	fake := filepath.Join(w.Bin, "llama-server")
	if err := w.goBuild(ctx, []string{"build", "-o", fake, "./internal/runtime/fakebackend"}); err != nil {
		return err
	}
	for _, name := range []string{"sd-server", "tts-server", "whisper-server"} {
		if err := os.Link(fake, filepath.Join(w.Bin, name)); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) goBuild(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = w.moduleDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// HostSpec is the fake machine's fixed shape; availability changes through
// FakeHost.
type HostSpec struct {
	CapacityGiB      float64
	Unified          bool
	GPUWorkingSetGiB float64
	AvailableGiB     float64
}

// DefaultHost is a 64 GiB Apple-style machine with plenty free: its reserve is
// 9.6 GiB and it counts as constrained below 12 GiB.
func DefaultHost() HostSpec {
	return HostSpec{CapacityGiB: 64, Unified: true, GPUWorkingSetGiB: 48, AvailableGiB: 48}
}

// Options configure one gateway.
type Options struct {
	Name           string // a directory name inside the workspace
	Host           HostSpec
	QueueCapacity  int
	QueueTimeoutMS int
	Parallel       string
	FakeDelayMS    int
	// CtxSize is --ctx-size (0: automatic).
	CtxSize int
	// LogLevel is --log-level (empty: the default, info).
	LogLevel string
	// FakeEnv adds environment entries, for the fake backends' behaviors.
	FakeEnv []string
	// Dir places this gateway's directory somewhere other than the workspace, for
	// a root on a small volume. It must be a scratch path too.
	Dir string
	// PrepareRoot runs after the fixture is written, before the gateway starts.
	PrepareRoot func(rootDir string) error
	// RuntimePortBase starts the gateway with --runtime-port-base instead of a flag
	// per runtime. The caller finds the five free ports.
	RuntimePortBase int
	// Reuse restarts on another gateway's root, with its keys. SamePorts keeps
	// its ports too, as a real restart would.
	Reuse     *Gateway
	SamePorts bool
}

// Keys are the API keys a fixture creates.
type Keys struct {
	User  string   // every inference scope and models:read
	Admin string   // every permission
	Runs  []string // one owner each, for detached runs
}

// Gateway is one running gateway process.
type Gateway struct {
	W        *Workspace
	Opts     Options
	Dir      string
	Root     string
	Base     string
	Port     int
	Debug    string
	Keys     Keys
	Fake     *FakeHost
	LogPath  string
	Cmd      *exec.Cmd
	ports    map[string]int
	logFile  *os.File
	waitDone chan struct{}
	waitErr  error
	// Startup is how long the process took from exec to answering /health.
	Startup time.Duration
}

// Start creates a fixture (or reuses Opts.Reuse's root) and starts a gateway,
// returning once it answers /health.
func (w *Workspace) Start(ctx context.Context, o Options) (*Gateway, error) {
	if o.Name == "" {
		o.Name = "gw"
	}
	if o.Host.CapacityGiB == 0 {
		o.Host = DefaultHost()
	}
	if o.QueueCapacity == 0 {
		o.QueueCapacity = 16
	}
	if o.QueueTimeoutMS == 0 {
		o.QueueTimeoutMS = 60_000
	}
	if o.Parallel == "" {
		o.Parallel = "1"
	}
	g := &Gateway{W: w, Opts: o, waitDone: make(chan struct{})}
	g.Dir = filepath.Join(w.Dir, o.Name)
	if o.Dir != "" {
		if err := refuseLive(o.Dir); err != nil {
			return nil, err
		}
		g.Dir = o.Dir
	}
	g.Root = filepath.Join(g.Dir, "root")
	g.Fake = &FakeHost{Path: filepath.Join(g.Dir, "host.json"), Spec: o.Host}
	g.LogPath = filepath.Join(g.Dir, "serve.log")
	if o.Reuse != nil && o.SamePorts {
		g.ports = o.Reuse.ports
	} else {
		ports, err := w.ports.take(7)
		if err != nil {
			return nil, err
		}
		g.ports = map[string]int{"gateway": ports[0], "debug": ports[1], "llm": ports[2], "stt": ports[3], "tts": ports[4], "image": ports[5], "video": ports[6]}
	}
	var err error
	if o.Reuse != nil {
		g.Dir, g.Root, g.Keys, g.Fake, g.LogPath = o.Reuse.Dir, o.Reuse.Root, o.Reuse.Keys, o.Reuse.Fake, o.Reuse.LogPath
	} else {
		if err := os.MkdirAll(filepath.Join(g.Dir, "home"), 0o700); err != nil {
			return nil, err
		}
		if err := g.Fake.Set(o.Host.AvailableGiB, "normal"); err != nil {
			return nil, err
		}
		if g.Keys, err = writeFixture(g.Root, o); err != nil {
			return nil, fmt.Errorf("fixture: %w", err)
		}
		if err := writeLabBackends(g.Root, w.Bin); err != nil {
			return nil, fmt.Errorf("fixture: %w", err)
		}
		if o.PrepareRoot != nil {
			if err := o.PrepareRoot(g.Root); err != nil {
				return nil, fmt.Errorf("fixture: %w", err)
			}
		}
	}
	if err := g.launch(ctx); err != nil {
		return g, err
	}
	if o.Reuse != nil {
		return g, nil
	}
	return g, g.chooseBackends(ctx)
}

// chooseBackends gives each model that automatic selection left without a backend
// the lab's own definition for its task. Selection differs by machine: a
// host with no GPU rates the built-in image and speech-to-text backends too low
// to choose, and some platforms have no build of a backend at all. The choice is the
// API's own, as a person would make it.
func (g *Gateway) chooseBackends(ctx context.Context) error {
	c := NewClient(g.Base)
	defer c.CloseIdle()
	for _, id := range fixtureModels {
		r := c.Do(ctx, "GET", "/_localbase/model-backend?model="+id, g.Keys.Admin, "", nil)
		if r.Err != nil || r.Status != 200 {
			return fmt.Errorf("reading the backend of %s: %d %v %.300s", id, r.Status, r.Err, r.Body)
		}
		var got struct {
			Selection struct {
				BackendID string `json:"backendId"`
			} `json:"selection"`
		}
		if err := json.Unmarshal(r.Body, &got); err != nil {
			return fmt.Errorf("reading the backend of %s: %w", id, err)
		}
		fallback, ok := labBackendFor[id]
		if got.Selection.BackendID != "" || !ok {
			continue
		}
		set := c.PostJSON(ctx, "/_localbase/model-backend", g.Keys.Admin, map[string]any{"action": "set", "model": id, "backendId": fallback})
		if set.Err != nil || set.Status != 200 {
			return fmt.Errorf("choosing %s for %s: %d %v %.300s", fallback, id, set.Status, set.Err, set.Body)
		}
	}
	return nil
}

// labBackendFor is the lab's backend for each model, used only where automatic
// selection found none.
var labBackendFor = map[string]string{ChatSmall: "lab-llama", ChatLarge: "lab-llama", Embedding: "lab-llama", Decision: "lab-llama",
	Image: "lab-sd", TTS: "lab-tts", STT: "lab-whisper"}

// writeLabBackends defines the fake backends for any platform. They stay
// out of automatic selection, so a machine that has real choices keeps them.
func writeLabBackends(rootDir, bin string) error {
	dir := filepath.Join(rootDir, "backends.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	automatic := false
	platform := []map[string]string{{"os": goruntime.GOOS, "arch": archName(), "accelerator": "cpu"}}
	for _, d := range []struct {
		id, adapter, executable string
		tasks, formats, arch    []string
	}{
		{"lab-llama", "llama-server/v1", "llama-server", []string{"chat", "embeddings", "decision"}, []string{"gguf"}, []string{"qwen3"}},
		{"lab-sd", "sd-server/v1", "sd-server", []string{"image"}, []string{"gguf", "safetensors"}, []string{"sd1"}},
		{"lab-tts", "tts-server/v1", "tts-server", []string{"speech"}, []string{"gguf"}, []string{"qwen3-tts"}},
		{"lab-whisper", "whisper-server/v1", "whisper-server", []string{"transcription"}, []string{"ggml", "gguf"}, []string{"whisper"}},
	} {
		def, err := json.MarshalIndent(map[string]any{
			"schema": "localbase.backend/v1", "id": d.id, "name": "Lab " + d.executable, "engine": "lab-fake", "adapter": d.adapter,
			"tasks": d.tasks, "capabilities": []string{}, "formats": d.formats,
			"architectures": map[string]any{"supported": d.arch}, "platforms": platform, "automatic": &automatic,
			"launch": map[string]any{"command": filepath.Join(bin, d.executable), "args": []string{}}}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, d.id+".json"), def, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// archName is a platform's architecture in the backend definitions' words.
func archName() string {
	if goruntime.GOARCH == "amd64" {
		return "x64"
	}
	return goruntime.GOARCH
}

func (g *Gateway) launch(ctx context.Context) error {
	g.Port, g.Debug = g.ports["gateway"], fmt.Sprintf("127.0.0.1:%d", g.ports["debug"])
	g.Base = fmt.Sprintf("http://127.0.0.1:%d", g.Port)
	args := []string{"--root", g.Root, "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(g.Port),
		"--inference-queue-capacity", strconv.Itoa(g.Opts.QueueCapacity), "--inference-queue-timeout-ms", strconv.Itoa(g.Opts.QueueTimeoutMS)}
	if g.Opts.RuntimePortBase > 0 {
		args = append(args, "--runtime-port-base", strconv.Itoa(g.Opts.RuntimePortBase))
	} else {
		args = append(args, "--llm-port", strconv.Itoa(g.ports["llm"]), "--stt-port", strconv.Itoa(g.ports["stt"]), "--tts-port", strconv.Itoa(g.ports["tts"]),
			"--image-port", strconv.Itoa(g.ports["image"]), "--video-port", strconv.Itoa(g.ports["video"]))
	}
	if g.Opts.CtxSize > 0 {
		args = append(args, "--ctx-size", strconv.Itoa(g.Opts.CtxSize))
	}
	if g.Opts.LogLevel != "" {
		args = append(args, "--log-level", g.Opts.LogLevel)
	}
	cmd := exec.Command(filepath.Join(g.W.Bin, "local-base"), args...)
	cmd.Env = g.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f, err := os.OpenFile(g.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	g.logFile = f
	cmd.Stdout, cmd.Stderr = f, f
	began := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	g.Cmd = cmd
	watchOwner(cmd.Process.Pid, g.W.Bin)
	go func() {
		g.waitErr = cmd.Wait()
		close(g.waitDone)
	}()
	if err := g.WaitHealthy(ctx, 60*time.Second); err != nil {
		return err
	}
	g.Startup = time.Since(began)
	return nil
}

// runtimeDir holds this gateway's coordination files, so nothing lands outside
// the scratch directory whatever the machine's XDG_RUNTIME_DIR is.
func (g *Gateway) runtimeDir() string { return filepath.Join(g.Dir, "run") }

// LeasePaths are where the gateway's root operation lease and its mutation lock
// live, for a test that needs to get in their way.
func (g *Gateway) LeasePaths() (lease, mutation string) {
	lease = filepath.Join(g.runtimeDir(), "local-base", root.Hash(g.Root)+".operation.lock")
	return lease, lease + ".mutation"
}

// env is a scrubbed environment: no LOCALBASE_ or token settings from the
// caller, a scratch home, and no route off this machine.
// CLIEnv is the environment for running local-base by hand against this root: the gateway's home and
// runtime directory, not this machine's, and not its debug listener, whose address is the gateway's.
func (g *Gateway) CLIEnv() []string {
	var env []string
	for _, kv := range g.env() {
		if !strings.HasPrefix(kv, "LOCALBASE_LAB_DEBUG_ADDR=") {
			env = append(env, kv)
		}
	}
	return env
}

func (g *Gateway) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "LOCALBASE_"), strings.HasPrefix(name, "OTEL_"), strings.HasPrefix(name, "GORACE"),
			strings.HasPrefix(strings.ToUpper(name), "HF_"), strings.EqualFold(name, "http_proxy"), strings.EqualFold(name, "https_proxy"),
			strings.EqualFold(name, "no_proxy"), name == "HOME", name == "GOMAXPROCS":
			continue
		}
		env = append(env, kv)
	}
	bin := g.W.Bin
	env = append(env, "HOME="+filepath.Join(g.Dir, "home"), "XDG_RUNTIME_DIR="+g.runtimeDir(),
		"GOMAXPROCS=2",
		"GORACE=log_path="+filepath.Join(g.Dir, "race")+" halt_on_error=0",
		"LOCALBASE_LAB_HOST_FILE="+g.Fake.Path, "LOCALBASE_LAB_DEBUG_ADDR="+g.Debug,
		"LOCALBASE_CATALOG_FEED_URL=off",
		"LOCALBASE_LLAMA_SERVER="+filepath.Join(bin, "llama-server"), "LOCALBASE_SD_SERVER="+filepath.Join(bin, "sd-server"),
		"LOCALBASE_TTS_SERVER="+filepath.Join(bin, "tts-server"), "LOCALBASE_WHISPER_SERVER="+filepath.Join(bin, "whisper-server"),
		"FAKE_DELAY_MS="+strconv.Itoa(g.Opts.FakeDelayMS),
		"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost",
		"https_proxy=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9", "no_proxy=127.0.0.1,localhost")
	return append(env, g.Opts.FakeEnv...)
}

// WaitHealthy waits for /health to answer 200, failing at once when the process
// has exited.
func (g *Gateway) WaitHealthy(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		if resp, err := client.Get(g.Base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-g.waitDone:
			return fmt.Errorf("gateway exited during startup: %v (log %s)", g.waitErr, g.LogPath)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gateway not healthy after %v (log %s)", budget, g.LogPath)
		}
	}
}

// Signal sends sig to the gateway process alone, as a service manager would.
func (g *Gateway) Signal(sig syscall.Signal) error { return g.Cmd.Process.Signal(sig) }

// Done is closed when the gateway process has exited.
func (g *Gateway) Done() <-chan struct{} { return g.waitDone }

// ExitErr is the process's exit result, valid once Done is closed.
func (g *Gateway) ExitErr() error { return g.waitErr }

// Stop terminates the gateway and waits for it, killing its group after grace.
// It returns how long the exit took.
func (g *Gateway) Stop(grace time.Duration) (time.Duration, error) {
	began := time.Now()
	select {
	case <-g.waitDone:
		return 0, g.waitErr
	default:
	}
	_ = g.Signal(syscall.SIGTERM)
	select {
	case <-g.waitDone:
		return time.Since(began), g.waitErr
	case <-time.After(grace):
		_ = syscall.Kill(-g.Cmd.Process.Pid, syscall.SIGKILL)
		<-g.waitDone
		return time.Since(began), fmt.Errorf("gateway did not stop within %v", grace)
	}
}

// LLMBase is the LLM backend's own address, for measuring against it directly.
func (g *Gateway) LLMBase() string { return fmt.Sprintf("http://127.0.0.1:%d", g.ports["llm"]) }

// CLI runs local-base against this gateway's root, in its environment, and returns
// what it printed.
func (g *Gateway) CLI(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(g.W.Bin, "local-base"), append([]string{"--root", g.Root}, args...)...)
	// Not the debug listener: that address belongs to the gateway.
	for _, kv := range g.env() {
		if !strings.HasPrefix(kv, "LOCALBASE_LAB_DEBUG_ADDR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("%w: %s", err, exit.Stderr)
	}
	return out, err
}

// Addr is the gateway's host:port.
func (g *Gateway) Addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(g.Port)) }

// RaceReports lists the race detector's report files for this gateway.
func (g *Gateway) RaceReports() []string {
	matches, _ := filepath.Glob(filepath.Join(g.Dir, "race.*"))
	return matches
}

// writeFixture builds a root: the database, the keys, the models as sparse
// files, and a catalog patch removing the artifact hashes. Without a hash the
// gateway records no observed peak, so a fake backend's tiny footprint never
// replaces a model's real demand and admission decides the same on every OS.
func writeFixture(dir string, o Options) (Keys, error) {
	var keys Keys
	r, err := root.Init(dir)
	if err != nil {
		return keys, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return keys, err
	}
	defer func() { _ = st.Close() }()
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		return keys, err
	}
	cfg := conf.Default(r.Dir, int(o.Host.CapacityGiB), cat)
	cfg.Selected = map[string][]string{"llm": {ChatSmall, ChatLarge, Embedding, Decision}, "stt": {STT}, "tts": {TTS}, "image": {Image}, "video": {}}
	cfg.Active = map[string]string{"llm": ChatSmall, "stt": STT, "tts": TTS, "image": Image, "video": ""}
	cfg.Parallel = o.Parallel
	cfg.AllowExperimental = true
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		return keys, err
	}
	if err := os.MkdirAll(r.CatalogDir(), 0o700); err != nil {
		return keys, err
	}
	for _, id := range fixtureModels {
		spec := cat.ByID(id)
		if spec == nil {
			return keys, fmt.Errorf("no catalog entry %s", id)
		}
		modelsDir := r.ModelsDir(spec.Kind)
		if err := os.MkdirAll(modelsDir, 0o700); err != nil {
			return keys, err
		}
		arts := make([]map[string]any, 0, len(spec.Artifacts))
		for _, a := range spec.Artifacts {
			if spec.Kind == "llm" && a.Role == "primary" {
				// Automatic backend selection reads the GGUF header.
				if err := testutil.GGUF(filepath.Join(modelsDir, a.Filename), map[string]any{"general.architecture": "qwen3", "general.file_type": 15, "qwen3.context_length": 262144}, 4096); err != nil {
					return keys, err
				}
			}
			if err := sparse(filepath.Join(modelsDir, a.Filename), a.ExpectedSizeBytes); err != nil {
				return keys, err
			}
			art := map[string]any{"sourcePath": a.SourcePath, "filename": a.Filename, "expectedSizeBytes": a.ExpectedSizeBytes, "role": a.Role}
			arts = append(arts, art)
		}
		patch, _ := json.MarshalIndent(map[string]any{"modelId": id, "artifacts": arts}, "", "  ")
		if err := os.WriteFile(filepath.Join(r.CatalogDir(), id+".json"), patch, 0o600); err != nil {
			return keys, err
		}
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "catalog-feed.json"), []byte(`{"url":null}`), 0o600); err != nil {
		return keys, err
	}
	ctx := context.Background()
	scopes := []string{"inference:chat", "inference:embeddings", "inference:decision", "inference:image", "inference:video",
		"inference:speech", "inference:transcription", "models:read"}
	if _, keys.User, err = st.CreateAPIKey(ctx, "lab", scopes); err != nil {
		return keys, err
	}
	if _, keys.Admin, err = st.CreateAPIKey(ctx, "lab-admin", auth.Permissions); err != nil {
		return keys, err
	}
	for i := range 4 {
		_, k, err := st.CreateAPIKey(ctx, fmt.Sprintf("lab-runs-%d", i), scopes)
		if err != nil {
			return keys, err
		}
		keys.Runs = append(keys.Runs, k)
	}
	return keys, nil
}

func sparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// PrimaryFile is a catalog model's main file; the fake backend names it in its
// answers, so a client can tell which runtime answered.
func PrimaryFile(id string) string {
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		return id
	}
	spec := cat.ByID(id)
	if spec == nil {
		return id
	}
	return spec.Primary().Filename
}
