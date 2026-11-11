package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// slowArtifact serves a model file: the first download sends half and then
// stalls until released (or the client goes away); later requests resume
// with a Range request.
type slowArtifact struct {
	data    []byte
	release chan struct{}
	mu      sync.Mutex
	ranges  []string
}

func (a *slowArtifact) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.ranges = append(a.ranges, r.Header.Get("Range"))
	first := len(a.ranges) == 1
	a.mu.Unlock()
	if !first {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(a.data)))
	w.WriteHeader(200)
	w.Write(a.data[:len(a.data)/2])
	w.(http.Flusher).Flush()
	select {
	case <-a.release:
	case <-r.Context().Done():
	}
}

// TestInstallInterruptedByGatewayStop: an accepted install that the
// gateway's shutdown (SIGTERM) or death (SIGKILL) interrupts is reported
// as interrupted after a restart, not forgotten; the shutdown does not
// wait for the download; installing again resumes the partial file.
func TestInstallInterruptedByGatewayStop(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	for _, how := range []string{"sigterm", "sigkill"} {
		t.Run(how, func(t *testing.T) {
			data := make([]byte, 8<<20)
			for i := range data {
				data[i] = byte(i * 31)
			}
			sum := sha256.Sum256(data)
			art := &slowArtifact{data: data, release: make(chan struct{})}
			srv := rangeServer(t, art, false)
			tmp := fsguard.ScratchDir(t)
			e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
			if r := e.cli(nil, "--json", "init"); r.code != 0 {
				t.Fatalf("init: %s", r.stderr)
			}
			entry := map[string]any{
				"modelId": "slow-stt", "displayName": "Slow", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
				"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
				"source": srv.URL + "/localbase/slow", "repositoryRevision": strings.Repeat("b", 40),
				"artifacts": []any{map[string]any{"sourcePath": "ggml-slow.bin", "filename": "ggml-slow.bin",
					"expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
				"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
				"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
			}
			b, _ := json.Marshal(entry)
			fsguard.MkdirAll(filepath.Join(e.root, "catalog.d"), 0o700)
			fsguard.WriteFile(filepath.Join(e.root, "catalog.d", "slow-stt.json"), b, 0o600)
			// The fixture serves the model from this machine.
			st, err := store.Open(filepath.Join(e.root, "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			state := func() map[string]any {
				r := e.do("GET", "/_localbase/model-management", "")
				for _, m := range r.JSON(t)["models"].([]any) {
					if m.(map[string]any)["id"] == "slow-stt" {
						return m.(map[string]any)
					}
				}
				t.Fatalf("slow-stt is not listed: %s", r.Body)
				return nil
			}
			e.serve("--no-auth")
			if r := e.do("POST", "/_localbase/model-management", `{"modelId":"slow-stt","action":"install"}`); r.Status != 202 {
				t.Fatalf("install: %d %s", r.Status, r.Body)
			}
			// Wait until half the file is on disk.
			deadline := time.Now().Add(15 * time.Second)
			for state()["installedBytes"].(float64) < float64(len(data)/2) {
				if time.Now().After(deadline) {
					t.Fatalf("no progress: %v", state())
				}
				time.Sleep(100 * time.Millisecond)
			}
			start := time.Now()
			if how == "sigterm" {
				e.stop()
				if d := time.Since(start); d > 8*time.Second {
					t.Errorf("shutdown waited %v for the download", d)
				}
			} else {
				e.kill()
			}
			e.serve("--no-auth")
			s := state()
			op, _ := s["operation"].(map[string]any)
			if op == nil || op["state"] != "failed" || op["detail"] != "Interrupted when the gateway stopped. Install again to resume." ||
				s["installed"] != false || s["installedBytes"].(float64) < float64(len(data)/2) {
				t.Fatalf("after restart: %v", s)
			}
			if why, _ := op["error"].(map[string]any); why["reason"] != "interrupted" {
				t.Errorf("after restart, the reason: %v", op)
			}
			close(art.release)
			if r := e.do("POST", "/_localbase/model-management", `{"modelId":"slow-stt","action":"install"}`); r.Status != 202 {
				t.Fatalf("install again: %d %s", r.Status, r.Body)
			}
			deadline = time.Now().Add(30 * time.Second)
			for {
				s = state()
				if op, _ := s["operation"].(map[string]any); op != nil && op["state"] != "running" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("install did not finish: %v", s)
				}
				time.Sleep(100 * time.Millisecond)
			}
			e.stop()
			if op := s["operation"].(map[string]any); op["state"] != "complete" || s["installed"] != true {
				t.Fatalf("resumed install: %v", s)
			}
			art.mu.Lock()
			defer art.mu.Unlock()
			if len(art.ranges) < 2 || !strings.HasPrefix(art.ranges[len(art.ranges)-1], "bytes=") {
				t.Errorf("the second install did not resume: %v", art.ranges)
			}
		})
	}
}

// A failed install says why, in the operations API and on the command line, with
// the old sentence unchanged. Go only: TS has one sentence for every cause.
func TestInstallFailureReasonExposed(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }), false)
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	sum := sha256.Sum256([]byte("gated"))
	b, _ := json.Marshal(map[string]any{
		"modelId": "gated-stt", "displayName": "Gated", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": srv.URL + "/localbase/gated", "repositoryRevision": strings.Repeat("c", 40),
		"artifacts":       []any{map[string]any{"sourcePath": "g.bin", "filename": "g.bin", "expectedSizeBytes": 5, "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
	})
	fsguard.MkdirAll(filepath.Join(e.root, "catalog.d"), 0o700)
	fsguard.WriteFile(filepath.Join(e.root, "catalog.d", "gated-stt.json"), b, 0o600)
	allowPrivateImports(t, e.root)

	human := e.cli(nil, "--non-interactive", "models", "install", "gated-stt")
	if human.code == 0 || !strings.Contains(human.stderr, "HTTP 403") || !strings.Contains(human.stderr, "Reason: gated_or_auth_required") {
		t.Errorf("human: %d %s", human.code, human.stderr)
	}
	machine := e.cli(nil, "--json", "models", "install", "gated-stt")
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code, Message, Reason string
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(machine.stdout), &env); err != nil || env.OK || env.Error.Reason != "gated_or_auth_required" || env.Error.Code != "operational_error" {
		t.Errorf("--json: %v %s", err, machine.stdout)
	}

	e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
	defer e.stop()
	if r := e.do("POST", "/_localbase/model-management", `{"modelId":"gated-stt","action":"install"}`); r.Status != 202 {
		t.Fatalf("install: %d %s", r.Status, r.Body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		op, _ := harness.EntryOf(t, e.do("GET", "/_localbase/model-management", "").JSON(t), "gated-stt")["operation"].(map[string]any)
		if op != nil && op["state"] != "running" {
			detail, _ := op["detail"].(string)
			why, _ := op["error"].(map[string]any)
			if op["state"] != "failed" || why["reason"] != "gated_or_auth_required" || !strings.HasPrefix(detail, "Installation failed.") {
				t.Errorf("operation: %v", op)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running: %v", op)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
