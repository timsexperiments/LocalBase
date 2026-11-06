package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Choosing what runs a model is on record: who, which model, which backend. What a
// refused caller sends stays out of the log.
func TestChoosingABackendIsAudited(t *testing.T) {
	ts, key, st, _ := importAPIGateway(t)
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
	all := events("audit.model-backend-changed")
	if len(all) != 3 {
		t.Fatalf("%d changes after the imports: %s", len(all), logs.String())
	}
	if e := all[2]; e["action"] != "import" || e["target_id"] != "chose" || e["backend_id"] != "llama.cpp-cpu" || !strings.HasPrefix(e["actor_id"].(string), "key_") {
		t.Errorf("%v", e)
	}
}
