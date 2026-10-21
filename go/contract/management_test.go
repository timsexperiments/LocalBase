package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// tinyModel is a user catalog entry whose 4 KiB file is already in place, so
// an install completes through checksum verification without a network.
const tinyModel = "contract-tiny-stt"

func tinyBytes() []byte {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func addTinyCatalogModel(rootDir, sttDir string) error {
	data := tinyBytes()
	sum := sha256.Sum256(data)
	entry := map[string]any{
		"modelId": tinyModel, "displayName": "Contract Tiny", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": "https://huggingface.co/localbase/contract-tiny", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts": []any{map[string]any{"sourcePath": "ggml-contract-tiny.bin", "filename": "ggml-contract-tiny.bin",
			"expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
	}
	b, _ := json.MarshalIndent(entry, "", "  ")
	if err := os.MkdirAll(filepath.Join(rootDir, "catalog.d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rootDir, "catalog.d", tinyModel+".json"), b, 0o600); err != nil {
		return err
	}
	os.MkdirAll(sttDir, 0o700)
	return os.WriteFile(filepath.Join(sttDir, "ggml-contract-tiny.bin"), data, 0o600)
}

// TestDifferentialModelManagement compares /_localbase/model-management on
// both gateways: the catalog state, every rejection, enable/activate/disable,
// uninstall, and an install polled to completion.
func TestDifferentialModelManagement(t *testing.T) {
	const path = "/_localbase/model-management"
	post := func(key func(*Target) string, body string) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			h := bearer(key(tg))
			h["Content-Type"] = "application/json"
			out[tg.Name] = do(t, "POST", tg.BaseURL+path, h, body)
		}
		return out
	}
	admin := func(tg *Target) string { return tg.Admin }
	reader := func(tg *Target) string { return tg.Key }
	state := func(tg *Target) map[string]any {
		r := do(t, "GET", tg.BaseURL+path, bearer(tg.Admin), "")
		if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s GET: %d %s", tg.Name, r.Status, r.Body)
		}
		return r.JSON(t)
	}
	same := func(label string) {
		t.Helper()
		var first map[string]any
		for _, tg := range targets {
			s := state(tg)
			if first == nil {
				first = s
				continue
			}
			// Free space moves between two reads; everything else must match.
			if d := diffJSON("", first, s, func(p string) bool { return strings.HasSuffix(p, ".availableBytes") }); len(d) > 0 {
				if len(d) > 15 {
					d = d[:15]
				}
				t.Errorf("%s: state differs:\n  %s", label, strings.Join(d, "\n  "))
			}
		}
	}
	sameOK := func(label string, rs map[string]*Resp, status int) {
		t.Helper()
		var first string
		for name, r := range rs {
			if r.Status != status {
				t.Errorf("%s %s: %d %s", label, name, r.Status, r.Body)
			}
			body := strings.TrimSpace(string(r.Body))
			if first == "" {
				first = body
			} else if body != first {
				t.Errorf("%s: bodies differ:\n %s\n %s", label, first, r.Body)
			}
		}
	}
	same("initial")
	// The active LLM, whichever earlier tests left as the default.
	activeLLM := chatModel
	for _, m := range state(targets[0])["models"].([]any) {
		e := m.(map[string]any)
		if id, _ := e["id"].(string); e["active"] == true && slices.Contains([]string{chatModel, decisionModel, embeddingModel}, id) {
			activeLLM = id
		}
	}
	for label, body := range map[string]string{
		"not json": `{`, "array": `[]`, "extra key": `{"modelId":"x","action":"enable","x":1}`, "empty id": `{"modelId":"","action":"enable"}`,
		"bad action": `{"modelId":"x","action":"delete"}`, "long id": `{"modelId":"` + strings.Repeat("m", 201) + `","action":"enable"}`,
		"too large": `{"modelId":"` + strings.Repeat("m", 1100) + `","action":"enable"}`, "unknown model": `{"modelId":"no-such","action":"enable"}`,
		"enable missing files": `{"modelId":"flux1-schnell-q4_0","action":"activate"}`,
		"activate unselected":  `{"modelId":"` + tinyModel + `","action":"activate"}`,
		"disable default llm":  `{"modelId":"` + activeLLM + `","action":"disable"}`,
		"uninstall selected":   `{"modelId":"whisper-base-en-q8_0","action":"uninstall"}`,
		"install selected":     `{"modelId":"whisper-base-en-q8_0","action":"install"}`,
		"experimental":         `{"modelId":"no-such","action":"install"}`,
	} {
		sameError(t, "management "+label, post(admin, body))
	}
	sameError(t, "management as reader", post(reader, `{"modelId":"`+tinyModel+`","action":"enable"}`))
	sameError(t, "management PUT", func() map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			out[tg.Name] = do(t, "PUT", tg.BaseURL+path, bearer(tg.Admin), "")
		}
		return out
	}())

	// Install (already present: verified, then complete), polled to the end.
	sameOK("install", post(admin, `{"modelId":"`+tinyModel+`","action":"install"}`), 202)
	for _, tg := range targets {
		deadline := time.Now().Add(30 * time.Second)
		for {
			op := entryOf(t, state(tg), tinyModel)["operation"].(map[string]any)
			if op["state"] != "running" {
				if op["state"] != "complete" || op["detail"] != "Installation complete" {
					t.Errorf("%s install: %v", tg.Name, op)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s install still running: %v", tg.Name, op)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	same("after install")
	sameOK("enable", post(admin, `{"modelId":"`+tinyModel+`","action":"enable"}`), 200)
	sameOK("activate", post(admin, `{"modelId":"`+tinyModel+`","action":"activate"}`), 200)
	same("after activate")
	sameOK("disable active stt", post(admin, `{"modelId":"`+tinyModel+`","action":"disable"}`), 200)
	// Deliberate difference: TS reconciles a running runtime eagerly when the
	// active model changes, so while its STT runtime is up it still references
	// the model and refuses the uninstall (409 conflict). Go switches on the
	// next request, holds nothing, and uninstalls.
	tsEager := false
	for _, tg := range targets {
		if tg.Name == "ts" {
			mods := do(t, "GET", tg.BaseURL+"/health", nil, "").JSON(t)["modalities"].(map[string]any)
			state := mods["stt"].(map[string]any)["state"]
			tsEager = state != "idle" && state != "disabled"
		}
	}
	uninstall := post(admin, `{"modelId":"`+tinyModel+`","action":"uninstall"}`)
	if !tsEager {
		sameOK("uninstall", uninstall, 200)
		same("after uninstall")
	} else if r := uninstall["ts"]; r.Status != 409 || r.ErrorCode(t) != "conflict" {
		t.Errorf("ts uninstall with a running STT runtime: %d %s", r.Status, r.Body)
	}
	for _, tg := range targets {
		if tg.Name == "ts" && tsEager {
			continue
		}
		if e := entryOf(t, state(tg), tinyModel); e["installed"] != false || e["installedBytes"] != 0.0 {
			t.Errorf("%s after uninstall: %v", tg.Name, e)
		}
	}
}

func entryOf(t *testing.T, state map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range state["models"].([]any) {
		if e := m.(map[string]any); e["id"] == id {
			return e
		}
	}
	t.Fatalf("%s not listed", id)
	return nil
}

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
	if *implFlag != "go" && *implFlag != "both" {
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
			tmp := t.TempDir()
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
			os.MkdirAll(filepath.Join(e.root, "catalog.d"), 0o700)
			os.WriteFile(filepath.Join(e.root, "catalog.d", "slow-stt.json"), b, 0o600)
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
