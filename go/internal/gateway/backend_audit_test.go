package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Choosing what runs a model is on record: who, which model, which backend. What a
// refused caller sends stays out of the log.
func TestChoosingABackendIsAudited(t *testing.T) {
	ts, key, st, scratch := importAPIGateway(t)
	logs := &syncBuffer{}
	ts.Config.Handler.(*Server).Log = slog.New(slog.NewJSONHandler(logs, nil))
	_, readOnly, err := st.CreateAPIKey(context.Background(), "read", []string{"models:read"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, credential string, body any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	events := func(name string) []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["event"] == name {
				out = append(out, m)
			}
		}
		return out
	}
	const route = "/_localbase/model-backend"
	if code, out := post(route, key, map[string]any{"action": "set", "model": chatModel, "backendId": "llama.cpp-cpu"}); code != 200 {
		t.Fatalf("set: %d %s", code, out)
	}
	if code, out := post(route, key, map[string]any{"action": "reset", "model": chatModel}); code != 200 {
		t.Fatalf("reset: %d %s", code, out)
	}
	const planted = "planted-value-q7"
	if code, _ := post(route, key, map[string]any{"action": "set", "model": chatModel, "backendId": planted}); code != 400 {
		t.Errorf("an unknown backend: %d", code)
	}
	if code, _ := post(route, key, map[string]any{"action": "set", "model": planted, "backendId": "llama.cpp-cpu"}); code != 404 {
		t.Errorf("an unknown model: %d", code)
	}
	if code, _ := post(route, readOnly, map[string]any{"action": "set", "model": chatModel, "backendId": "llama.cpp-cpu"}); code != 403 {
		t.Errorf("without models:manage: %d", code)
	}
	changed := events("audit.model-backend-changed")
	if len(changed) != 2 {
		t.Fatalf("%d changes: %s", len(changed), logs.String())
	}
	for i, want := range []map[string]any{{"action": "set", "backend_id": "llama.cpp-cpu"}, {"action": "reset", "backend_id": ""}} {
		e := changed[i]
		if e["action"] != want["action"] || e["backend_id"] != want["backend_id"] || e["actor_kind"] != "api-key" ||
			!strings.HasPrefix(e["actor_id"].(string), "key_") || e["target_kind"] != "model" || e["target_id"] != chatModel {
			t.Errorf("change %d: %v", i, e)
		}
	}
	refused := events("audit.model-backend-refused")
	if len(refused) != 3 {
		t.Fatalf("%d refusals: %s", len(refused), logs.String())
	}
	if refused[2]["status"] != float64(403) || refused[2]["reason"] == "" {
		t.Errorf("%v", refused[2])
	}
	if strings.Contains(logs.String(), planted) {
		t.Error("what a refused caller sent reached the log")
	}
	if strings.Contains(logs.String(), key) {
		t.Error("a key reached the log")
	}

	// Choosing one while importing counts too. Automatic isn't a choice.
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(make([]byte, 16<<10)); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if r.Header.Get("Range") == "bytes=0-0" {
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer source.Close()
	start := func(id, backend string) string {
		t.Helper()
		body := map[string]any{"action": "start", "source": source.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": id}
		if backend != "" {
			body["backend"] = backend
		}
		code, out := post("/_localbase/model-imports", key, body)
		if code != 202 {
			t.Fatalf("start: %d %s", code, out)
		}
		var started struct{ Job string }
		json.Unmarshal([]byte(out), &started)
		return started.Job
	}
	jobs := []string{start("chose", "llama.cpp-cpu"), start("auto", "auto"), start("none", "")}
	for _, j := range jobs {
		if code, out := post("/_localbase/model-imports", key, map[string]any{"action": "cancel", "job": j}); code != 200 {
			t.Errorf("cancel: %d %s", code, out)
		}
	}
	// A cancel answers at once, and the job ends on its own: it stops its download, removes what it
	// downloaded, and then says cancelled. Wait for that, so nothing writes under the root while
	// the test removes it (a pool of one made the job slow enough to be caught at it).
	for _, j := range jobs {
		if state := importEnded(t, ts.URL, key, j); state != "cancelled" {
			t.Errorf("job %s ended %q", j, state)
		}
	}
	if left, err := os.ReadDir(filepath.Join(scratch, "root", "models", ".staging")); err == nil && len(left) != 0 {
		t.Errorf("cancelled imports left %d downloads behind", len(left))
	}
	all := events("audit.model-backend-changed")
	if len(all) != 3 {
		t.Fatalf("%d changes after the imports: %s", len(all), logs.String())
	}
	if e := all[2]; e["action"] != "import" || e["target_id"] != "chose" || e["backend_id"] != "llama.cpp-cpu" || !strings.HasPrefix(e["actor_id"].(string), "key_") {
		t.Errorf("%v", e)
	}
}

// importEnded waits, up to 10 s, for an import to stop running, and returns the state it ended in.
func importEnded(t *testing.T, base, key, job string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequest("GET", base+"/_localbase/model-imports?id="+job, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ State string }
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if got.State != "running" {
			return got.State
		}
		if time.Now().After(deadline) {
			t.Fatalf("import %s was still running 10 s after it was cancelled", job)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
