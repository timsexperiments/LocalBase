package contract

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An install stuck on a silent server can be cancelled: other installs are free
// again at once, the partial stays, and installing again resumes it. Go only: TS has
// no cancel, and answers this action 400.
func TestInstallCancel(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	fake := filepath.Join(tmp, "fakebackend")
	if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	big, other := ggufFile(t, "qwen3", nil, 64*1024), ggufFile(t, "qwen3", nil, 32*1024)
	half := len(big) / 2
	var stall atomic.Bool
	stall.Store(true)
	release := make(chan struct{})
	defer close(release)
	src := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case freshTiny + ".gguf":
			if stall.Load() {
				w.Header().Set("Content-Length", fmt.Sprint(len(big)))
				w.WriteHeader(http.StatusOK)
				w.Write(big[:half])
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			serveBytes(w, r, big)
		case freshSecond + ".gguf":
			serveBytes(w, r, other)
		default:
			w.WriteHeader(404)
		}
	}), false)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true, env: []string{"LOCALBASE_LLAMA_SERVER=" + fake}}
	freshRootInit(t, e, tmp)
	freshRootModel(t, e.root, freshTiny, src.URL, big)
	freshRootModel(t, e.root, freshSecond, src.URL, other)
	allowPrivateImports(t, e.root)
	e.serve("--no-auth")
	defer e.stop()

	act := func(id, action string) *Resp {
		t.Helper()
		return e.do("POST", freshPath, `{"modelId":"`+id+`","action":"`+action+`"}`)
	}
	op := func(id string) map[string]any {
		t.Helper()
		o, _ := entryOf(t, e.do("GET", freshPath, "").JSON(t), id)["operation"].(map[string]any)
		return o
	}
	waitState := func(id, want string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(30 * time.Millisecond) {
			o := op(id)
			if o != nil && o["state"] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %v, want %s", id, o, want)
			}
		}
	}
	partial := filepath.Join(e.root, "models", "llm", freshTiny+".gguf.partial")

	if r := act(freshTiny, "cancel"); r.Status != 409 {
		t.Errorf("cancel with nothing running: %d %s", r.Status, r.Body)
	}
	if r := act(freshTiny, "install"); r.Status != 202 {
		t.Fatalf("install: %d %s", r.Status, r.Body)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if info, err := os.Stat(partial); err == nil && info.Size() == int64(half) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the partial never reached the stall")
		}
	}
	if r := act(freshSecond, "install"); r.Status != 409 {
		t.Errorf("a second install during the first: %d %s", r.Status, r.Body)
	}
	if r := act(freshSecond, "cancel"); r.Status != 409 {
		t.Errorf("cancelling another model's install: %d %s", r.Status, r.Body)
	}
	began := time.Now()
	r := act(freshTiny, "cancel")
	if r.Status != 200 || r.JSON(t)["state"] != "cancelled" || time.Since(began) > 3*time.Second {
		t.Fatalf("cancel: %d %s after %v", r.Status, r.Body, time.Since(began))
	}
	o := op(freshTiny)
	if o["state"] != "cancelled" || o["error"] != nil {
		t.Errorf("a cancelled install is not a failed one: %v", o)
	}
	// Cancelling again is safe: the answer is how it ended, and nothing changes.
	if again := act(freshTiny, "cancel"); again.Status != 200 || again.JSON(t)["state"] != "cancelled" {
		t.Errorf("cancel again: %d %s", again.Status, again.Body)
	}
	if info, err := os.Stat(partial); err != nil || info.Size() != int64(half) {
		t.Errorf("the partial should stay: %v %v", info, err)
	}
	// The slot is free at once.
	if r := act(freshSecond, "install"); r.Status != 202 {
		t.Fatalf("install after the cancel: %d %s", r.Status, r.Body)
	}
	waitState(freshSecond, "complete")
	// Installing again resumes, and completes.
	stall.Store(false)
	if r := act(freshTiny, "install"); r.Status != 202 {
		t.Fatalf("install again: %d %s", r.Status, r.Body)
	}
	waitState(freshTiny, "complete")
	if again := act(freshTiny, "cancel"); again.Status != 200 || again.JSON(t)["state"] != "complete" {
		t.Errorf("cancel after it finished: %d %s", again.Status, again.Body)
	}
	if o := op(freshTiny); o["state"] != "complete" {
		t.Errorf("that cancel changed the outcome: %v", o)
	}
}

// The command, on a real binary, against a real gateway with auth on: the same result as the route,
// and it needs nothing but being on this machine with the instance token.
func TestInstallCancelCommand(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	big := ggufFile(t, "qwen3", nil, 64*1024)
	half := len(big) / 2
	release := make(chan struct{})
	defer close(release)
	src := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		w.WriteHeader(http.StatusOK)
		w.Write(big[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), false)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	freshRootInit(t, e, tmp)
	freshRootModel(t, e.root, freshTiny, src.URL, big)
	allowPrivateImports(t, e.root)
	if r := e.cli(nil, "models", "cancel", freshTiny); r.code != 1 || !strings.Contains(r.stderr, "No gateway is running") {
		t.Errorf("with no gateway: %d %s%s", r.code, r.stdout, r.stderr)
	}
	e.key = e.json("keys", "create", "--name", "manager", "--scopes", "models:manage,models:read")["secret"].(string)
	e.serve()
	defer e.stop()
	if r := e.cli(nil, "models", "cancel", freshTiny); r.code != 1 || !strings.Contains(r.stderr, "No installation of this model is running") {
		t.Errorf("with nothing running: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if r := e.do("POST", freshPath, `{"modelId":"`+freshTiny+`","action":"install"}`); r.Status != 202 {
		t.Fatalf("install: %d %s", r.Status, r.Body)
	}
	partial := filepath.Join(e.root, "models", "llm", freshTiny+".gguf.partial")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if info, err := os.Stat(partial); err == nil && info.Size() == int64(half) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the partial never reached the stall")
		}
	}
	// What lets the command through is only readable by whoever owns the root.
	owner, err := os.Stat(filepath.Join(e.root, "runtime", "gateway.lock", "owner.json"))
	if err != nil || owner.Mode().Perm() != 0o600 {
		t.Errorf("the instance token's file: %v %v", owner, err)
	}
	r := e.cli(nil, "models", "cancel", freshTiny)
	if r.code != 0 || !strings.Contains(r.stdout, "Installation cancelled. The downloaded part is kept") {
		t.Errorf("cancel: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if info, err := os.Stat(partial); err != nil || info.Size() != int64(half) {
		t.Errorf("the partial should stay: %v %v", info, err)
	}
	// Again is safe, and --json has the route's operation.
	var out struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	again := e.cli(nil, "--json", "models", "cancel", freshTiny)
	if err := json.Unmarshal([]byte(again.stdout), &out); err != nil || again.code != 0 || !out.OK || out.Data["state"] != "cancelled" || out.Data["action"] != "install" {
		t.Errorf("again: %d %s %v", again.code, again.stdout, err)
	}
	logged, err := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil || !strings.Contains(string(logged), `"audit.models-changed"`) || !strings.Contains(string(logged), `"actor_kind":"cli"`) {
		t.Errorf("no audit of the cancel: %v", err)
	}
	if help := e.cli(nil, "models", "cancel", "--help"); help.code != 0 || !strings.Contains(help.stdout, "Stop an install") {
		t.Errorf("--help: %d %s", help.code, help.stdout)
	}
}
