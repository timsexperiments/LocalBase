package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

const (
	freshTiny   = "contract-tiny-llm"
	freshSecond = "contract-second-llm"
	freshPath   = "/_localbase/model-management"
)

// freshRootModel writes a user catalog entry for an LLM served by src.
func freshRootModel(t *testing.T, root, id, src string, data []byte) {
	t.Helper()
	sum := sha256.Sum256(data)
	b, _ := json.Marshal(map[string]any{
		"modelId": id, "displayName": id, "kind": "llm", "provider": "LocalBase tests", "family": "Qwen",
		"version": "test", "size": "tiny", "quant": "Q4_K_M", "storageGb": 0.01,
		"source": src + "/localbase/" + id, "repositoryRevision": strings.Repeat("c", 40),
		"artifacts": []any{map[string]any{"sourcePath": id + ".gguf", "filename": id + ".gguf",
			"expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"text"}, "outputModalities": []string{"text"}, "capabilities": []string{"chat"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
	})
	fsguard.MkdirAll(filepath.Join(root, "catalog.d"), 0o700)
	if err := fsguard.WriteFile(filepath.Join(root, "catalog.d", id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// freshRootInit is init plus the small launch and reserves the other end-to-end
// tests use, so a tiny model fits on a small CI runner with admission still on.
// It sets memory and launch size only: models stay as init left them.
func freshRootInit(t *testing.T, e *e2e, tmp string) {
	t.Helper()
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	cfg := filepath.Join(tmp, "small.toml")
	fsguard.WriteFile(cfg, []byte("ctxSize = 2048\nctxMode = \"explicit\"\nparallel = 1\n\n[memory.systemReserve]\npercent = 1\nminimumGb = 0\n\n[memory.acceleratorReserve]\npercent = 1\nminimumGb = 0\n"), 0o644)
	if r := e.cli(nil, "--non-interactive", "configure", "--config", cfg); r.code != 0 {
		t.Fatalf("configure: %s%s", r.stdout, r.stderr)
	}
}

// TestFreshRootFirstInstall runs what a new user does, with nothing seeded in
// the database: init, list, install, chat. The deadlock it guards against: the
// default model could not be installed because it was selected, nor disabled
// because it was the default. Deliberately Go only (DESIGN 13.6 item 10).
func TestFreshRootFirstInstall(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := fsguard.ScratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/timsexperiments/LocalBase/go/internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	tiny, second := ggufFile(t, "qwen3", nil, 64*1024), ggufFile(t, "qwen3", nil, 32*1024)
	files := map[string][]byte{freshTiny + ".gguf": tiny, freshSecond + ".gguf": second}
	src := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			w.WriteHeader(404)
			return
		}
		serveBytes(w, r, data)
	}), false)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true, env: []string{"LOCALBASE_LLAMA_SERVER=" + fake}}
	onFixedHost(t, e, 16, 12<<10) // the model has to fit, whatever the runner has free
	freshRootInit(t, e, tmp)
	allowPrivateImports(t, e.root)
	freshRootModel(t, e.root, freshTiny, src.URL, tiny)
	freshRootModel(t, e.root, freshSecond, src.URL, second)
	e.serve("--no-auth")
	defer e.stop()

	state := func(id string) map[string]any {
		t.Helper()
		return harness.EntryOf(t, e.do("GET", freshPath, "").JSON(t), id)
	}
	act := func(id, action string) *harness.Resp {
		t.Helper()
		return e.do("POST", freshPath, `{"modelId":"`+id+`","action":"`+action+`"}`)
	}
	install := func(id string) {
		t.Helper()
		if r := act(id, "install"); r.Status != 202 {
			t.Fatalf("install %s: %d %s", id, r.Status, r.Body)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			op, _ := state(id)["operation"].(map[string]any)
			if op != nil && op["state"] != "running" {
				if op["state"] != "complete" {
					t.Fatalf("install %s: %v", id, op)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("install %s still running: %v", id, op)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	defaultLLM := func() string {
		t.Helper()
		for _, m := range e.do("GET", freshPath, "").JSON(t)["models"].([]any) {
			m := m.(map[string]any)
			if m["active"] == true && m["installed"] == false && m["enabled"] == true && strings.HasPrefix(m["id"].(string), "qwen") {
				return m["id"].(string)
			}
		}
		t.Fatal("no uninstalled default LLM on a fresh root")
		return ""
	}

	// The fresh state: every default can be installed, nothing says otherwise.
	def := defaultLLM()
	for _, m := range e.do("GET", freshPath, "").JSON(t)["models"].([]any) {
		m := m.(map[string]any)
		if m["active"] == true && m["installed"] == false {
			if r, _ := m["installUnavailableReason"].(string); strings.Contains(r, "Disable") {
				t.Errorf("%s: %s", m["id"], r)
			}
		}
	}
	// Disabling the default is refused with a way out, not a circle.
	r := act(def, "disable")
	if r.Status != 409 || r.ErrorCode(t) != "conflict" || !strings.Contains(string(r.Body), "Install another LLM first") {
		t.Errorf("disable the uninstalled default: %d %s", r.Status, r.Body)
	}
	if r := e.chat(freshTiny); r.Status < 400 {
		t.Errorf("chat with a model not installed: %d %s", r.Status, r.Body)
	}

	// The first install leaves chat working, with no further call.
	install(freshTiny)
	if s := state(freshTiny); s["installed"] != true || s["enabled"] != true || s["active"] != true {
		t.Fatalf("after the first install: %v", s)
	}
	if s := state(def); s["active"] != false {
		t.Errorf("the old default: %v", s)
	}
	if r := e.chatRetrying(freshTiny); r.Status != 200 {
		t.Fatalf("chat: %d %s", r.Status, r.Body)
	}
	// Installed and in use: refused, with the reason in the listing too.
	if r := act(freshTiny, "install"); r.Status != 409 || r.ErrorCode(t) != "conflict" {
		t.Errorf("install the running default: %d %s", r.Status, r.Body)
	}
	if s := state(freshTiny); s["canInstall"] != false {
		t.Errorf("the running default: %v", s)
	}
	// The old default, no longer active, can now be disabled.
	if r := act(def, "disable"); r.Status != 200 {
		t.Errorf("disable the old default: %d %s", r.Status, r.Body)
	}
	// The only installed LLM is refused, naming the way out.
	r = act(freshTiny, "disable")
	if r.Status != 409 || !strings.Contains(string(r.Body), "Install another LLM and activate it") {
		t.Errorf("disable the only LLM: %d %s", r.Status, r.Body)
	}

	// With a working default, another install changes nothing...
	install(freshSecond)
	if s := state(freshSecond); s["active"] != false || s["enabled"] != false || s["installed"] != true {
		t.Fatalf("a second install: %v", s)
	}
	if s := state(freshTiny); s["active"] != true {
		t.Fatalf("the default after a second install: %v", s)
	}
	// ...and one idempotent call makes it the default, then the old one can go.
	for range 2 {
		if r := act(freshSecond, "activate"); r.Status != 200 {
			t.Fatalf("activate: %d %s", r.Status, r.Body)
		}
	}
	if s := state(freshSecond); s["active"] != true || s["enabled"] != true {
		t.Fatalf("after activate: %v", s)
	}
	if r := act(freshTiny, "disable"); r.Status != 200 {
		t.Errorf("disable the old default: %d %s", r.Status, r.Body)
	}
	if r := e.chatRetrying(freshSecond); r.Status != 200 {
		t.Errorf("chat on the new default: %d %s", r.Status, r.Body)
	}
}

// The same first step from the command line: `models install` on a fresh root
// leaves a working default for the next serve.
func TestFreshRootCLIInstall(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := fsguard.ScratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/timsexperiments/LocalBase/go/internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	tiny := ggufFile(t, "qwen3", nil, 64*1024)
	src := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveBytes(w, r, tiny) }), false)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true, env: []string{"LOCALBASE_LLAMA_SERVER=" + fake}}
	onFixedHost(t, e, 16, 12<<10) // the model has to fit, whatever the runner has free
	freshRootInit(t, e, tmp)
	allowPrivateImports(t, e.root)
	freshRootModel(t, e.root, freshTiny, src.URL, tiny)
	if r := e.cli(nil, "--non-interactive", "models", "install", freshTiny); r.code != 0 {
		t.Fatalf("install: %s", r.stderr)
	}
	e.serve("--no-auth")
	defer e.stop()
	if s := harness.EntryOf(t, e.do("GET", freshPath, "").JSON(t), freshTiny); s["active"] != true || s["enabled"] != true || s["installed"] != true {
		t.Fatalf("after install: %v", s)
	}
	if r := e.chatRetrying(freshTiny); r.Status != 200 {
		t.Fatalf("chat: %d %s", r.Status, r.Body)
	}
}

// allowPrivateImports opts a root in to catalog sources on this machine, where these
// tests serve their models from.
func allowPrivateImports(t *testing.T, root string) {
	t.Helper()
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
}
