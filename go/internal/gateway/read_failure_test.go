package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type readFailureClient struct {
	t   *testing.T
	url string
}

func (c readFailureClient) get(path string) (int, http.Header, string) {
	c.t.Helper()
	resp, err := http.Get(c.url + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// A client's model list isn't taken away by a failed internal read. The body is the same but for the
// one figure that needed it, and the failure is in a header and the log.
func TestAFailedPeakReadDoesNotTakeAwayTheClientsList(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	c := readFailureClient{t, ts.URL}
	code, header, before := c.get("/v1/models")
	if code != 200 || header.Get("X-Localbase-Warning") != "" {
		t.Fatalf("before: %d %q", code, header.Get("X-Localbase-Warning"))
	}
	if _, err := s.Store.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	code, header, after := c.get("/v1/models")
	if code != 200 || header.Get("X-Localbase-Warning") != store.ReadPeaks {
		t.Errorf("%d, warning %q: %.200s", code, header.Get("X-Localbase-Warning"), after)
	}
	if !strings.Contains(logs.String(), "models.read-failed") || !strings.Contains(logs.String(), "reading the memory peaks seen for") {
		t.Errorf("not logged: %s", logs.String())
	}
	shape := func(body string) map[string]map[string]any {
		var list struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil || list.Object != "list" {
			t.Fatalf("%v: %.200s", err, body)
		}
		out := map[string]map[string]any{}
		for _, m := range list.Data {
			out[m["id"].(string)] = m
		}
		return out
	}
	was, now := shape(before), shape(after)
	if len(was) == 0 || len(was) != len(now) {
		t.Fatalf("%d models, then %d", len(was), len(now))
	}
	missingMemory := 0
	for id, m := range now {
		for _, field := range []string{"id", "object", "created", "owned_by"} {
			if m[field] != was[id][field] {
				t.Errorf("%s: %s changed", id, field)
			}
		}
		if _, has := m["memory"]; !has {
			missingMemory++
		}
		delete(m, "memory")
		w := was[id]
		delete(w, "memory")
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(w)
		if !bytes.Equal(a, b) {
			t.Errorf("%s changed beyond the figure:\n%s\n%s", id, b, a)
		}
	}
	if missingMemory == 0 {
		t.Error("a figure without what was seen was given as if it were whole")
	}
	if code, header, body := c.get("/v1/models/" + ChatModel); code != 200 || header.Get("X-Localbase-Warning") != store.ReadPeaks {
		t.Errorf("one model: %d %q %.200s", code, header.Get("X-Localbase-Warning"), body)
	}
}

// Our own routes say so in the body, and leave out only the figures that needed the read.
func TestOurModelRoutesSayTheyAreDegraded(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c := readFailureClient{t, ts.URL}
	if code, _, body := c.get("/_localbase/models"); code != 200 || strings.Contains(body, `"degraded"`) {
		t.Fatalf("before: %d %.200s", code, body)
	}
	if _, err := s.Store.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	check := func(body string, model map[string]any) {
		t.Helper()
		var out struct {
			Degraded     bool                `json:"degraded"`
			ReadProblems []map[string]string `json:"readProblems"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Degraded || len(out.ReadProblems) == 0 || !strings.HasPrefix(out.ReadProblems[0]["step"], "reading the memory peaks seen for ") || out.ReadProblems[0]["reason"] != "the read failed" {
			t.Errorf("%+v", out)
		}
		if strings.Contains(body, "no such table") {
			t.Errorf("shows the raw error: %.300s", body)
		}
		catalogPart := model["catalog"].(map[string]any)
		memory := catalogPart["memory"].(map[string]any)
		fit := memory["fit"].(map[string]any)
		if fit["verdict"] != "unknown" || memory["source"] != "unavailable" || memory["demandBytes"] != float64(0) {
			t.Errorf("figures that needed the read: %v", memory)
		}
		if memory["storageEstimateGb"] == float64(0) || memory["reference"] == nil {
			t.Errorf("figures that didn't need it are gone: %v", memory)
		}
	}
	code, _, body := c.get("/_localbase/models/" + ChatModel)
	if code != 200 {
		t.Fatalf("one: %d %.200s", code, body)
	}
	var one map[string]any
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatal(err)
	}
	check(body, one)
	code, _, body = c.get("/_localbase/models")
	if code != 200 {
		t.Fatalf("list: %d %.200s", code, body)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list.Data {
		if m["id"] == ChatModel {
			check(body, m)
			found = true
		}
	}
	if !found {
		t.Error("the model isn't in the list")
	}
}

// A failed read of a stored runtime report: the list stays, with what the files say.
func TestAFailedCapabilityReadWarnsAndKeepsTheList(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c := readFailureClient{t, ts.URL}
	if _, err := s.Store.DB.Exec(`DROP TABLE model_runtime_capabilities`); err != nil {
		t.Fatal(err)
	}
	code, header, body := c.get("/v1/models")
	if code != 200 || header.Get("X-Localbase-Warning") != store.ReadCapabilities || !strings.Contains(body, ChatModel) {
		t.Errorf("%d %q %.200s", code, header.Get("X-Localbase-Warning"), body)
	}
	code, _, body = c.get("/_localbase/models/" + ChatModel)
	if code != 200 || !strings.Contains(body, `"degraded":true`) || !strings.Contains(body, "reading what the runtime reported for "+ChatModel) {
		t.Errorf("%d %.300s", code, body)
	}
}

// A failed read of the imported models isn't "no imported models".
func TestAFailedLibraryReadIsNotNoImportedModels(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	lib, err := library.Open(s.Root, s.Store, s.Catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Library = lib
	c := readFailureClient{t, ts.URL}
	if code, header, _ := c.get("/v1/models"); code != 200 || header.Get("X-Localbase-Warning") != "" {
		t.Fatalf("before: %d %q", code, header.Get("X-Localbase-Warning"))
	}
	if _, err := s.Store.DB.Exec(`DROP TABLE imported_models`); err != nil {
		t.Fatal(err)
	}
	code, header, body := c.get("/v1/models")
	if code != 200 || header.Get("X-Localbase-Warning") != store.ReadLibrary || !strings.Contains(body, ChatModel) {
		t.Errorf("%d %q %.200s", code, header.Get("X-Localbase-Warning"), body)
	}
	if !strings.Contains(logs.String(), "reading the imported models") {
		t.Errorf("not logged: %s", logs.String())
	}
	// One it can't find might be one it couldn't read, so "not found" would be a guess.
	code, header, body = c.get("/v1/models/some-imported-model")
	if code != 503 || header.Get("Retry-After") != "5" || !strings.Contains(body, "reading the imported models") || !strings.Contains(body, "this model may exist") {
		t.Errorf("one: %d, Retry-After %q: %.200s", code, header.Get("Retry-After"), body)
	}
}

// A filter on fit can't place a model with no figure, so the list says which it left out, and the
// header says something is missing, rather than quietly being shorter.
func TestAFilteredListDoesNotShrinkSilently(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c := readFailureClient{t, ts.URL}
	ids := func(body string) (data, unknown []string, degraded bool) {
		t.Helper()
		var out struct {
			Degraded bool     `json:"degraded"`
			Unknown  []string `json:"unknown"`
			Data     []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Data {
			data = append(data, m.ID)
		}
		return data, out.Unknown, out.Degraded
	}
	_, _, before := c.get("/_localbase/models?fit=fits,tight,wont-fit")
	was, _, degraded := ids(before)
	if degraded || !slices.Contains(was, ChatModel) {
		t.Fatalf("before: %v %v", was, degraded)
	}
	if _, err := s.Store.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	code, header, body := c.get("/_localbase/models?fit=fits,tight,wont-fit")
	data, unknown, degraded := ids(body)
	if code != 200 || !degraded || header.Get("X-Localbase-Warning") != store.ReadPeaks {
		t.Errorf("%d %v %q", code, degraded, header.Get("X-Localbase-Warning"))
	}
	if slices.Contains(data, ChatModel) || !slices.Contains(unknown, ChatModel) {
		t.Errorf("the model is in neither place properly: data %v, unknown %v", data, unknown)
	}
	for _, id := range was {
		if !slices.Contains(data, id) && !slices.Contains(unknown, id) {
			t.Errorf("%s went missing", id)
		}
	}
	// The other filters still apply to it.
	_, _, body = c.get("/_localbase/models?fit=fits&kind=stt")
	if _, unknown, _ := ids(body); slices.Contains(unknown, ChatModel) {
		t.Errorf("listed despite another filter: %v", unknown)
	}
}
