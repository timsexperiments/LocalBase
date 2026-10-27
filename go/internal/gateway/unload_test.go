package gateway

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func unload(t *testing.T, c runClient, id, query string) (int, map[string]any) {
	t.Helper()
	return c.do("POST", "/_localbase/models/"+id+"/unload"+query, "")
}

// An idle model unloads at once: the backend stops, the admission ledger
// drops its reservation, runtime.stopped says "unload", and the next
// request loads it again.
func TestUnloadIdle(t *testing.T) {
	logs := &syncBuffer{}
	testLog = logs
	ts, llm, mem := newTestGateway(t, host{})
	testLog = nil
	c := runClient{t: t, base: ts.URL}
	if r := post(t, ts, "/v1/chat/completions", chatBody("hi")); r.status != 200 {
		t.Fatalf("chat: %d %s", r.status, r.body)
	}
	reserved := mem.Reserved()["llm"]
	if reserved <= 0 {
		t.Fatalf("no llm reservation: %v", mem.Reserved())
	}
	code, m := unload(t, c, chatModel, "")
	if code != 200 || m["unloaded"] != true || m["runtime"] != "llm" || m["memoryFreedBytes"] != float64(reserved) {
		t.Fatalf("unload: %d %v (reserved %d)", code, m, reserved)
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("%s still running", id)
	}
	if _, held := mem.Reserved()["llm"]; held {
		t.Errorf("the reservation outlived the unload: %v", mem.Reserved())
	}
	if !waitFor(2*time.Second, func() bool {
		return strings.Contains(logs.String(), `"event":"runtime.stopped"`) && strings.Contains(logs.String(), `"reason":"unload"`)
	}) {
		t.Errorf("no runtime.stopped/unload event:\n%s", logs.String())
	}
	if r := post(t, ts, "/v1/chat/completions", chatBody("hi")); r.status != 200 {
		t.Errorf("reload on demand: %d %s", r.status, r.body)
	}
}

// A busy model drains: in-flight work (a detached run here) finishes, new
// requests for the model are refused meanwhile, then the model stops.
func TestUnloadBusyDrains(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 20") // about 1 s
	c.waitStatus(id, "in_progress")
	// One request queued before the unload (the slot is busy) ...
	queued := make(chan result, 1)
	go func() { queued <- post(t, ts, "/v1/chat/completions", chatBody("hi")) }()
	waitFor(2*time.Second, func() bool { return llm.Snapshot().Waiting == 1 })
	done := make(chan map[string]any, 1)
	go func() {
		code, m := unload(t, c, chatModel, "")
		m["status"] = code
		done <- m
	}()
	refused := func(r result) bool {
		return r.status == 503 && r.code() == "model_unloading" && r.header.Get("Retry-After") != ""
	}
	if r := <-queued; !refused(r) {
		t.Errorf("a queued request during the drain: %d %s", r.status, r.body)
	}
	// ... and one sent mid-drain: both refused at once, not held.
	began := time.Now()
	if r := post(t, ts, "/v1/chat/completions", chatBody("hi")); !refused(r) || time.Since(began) > 300*time.Millisecond {
		t.Errorf("a new request during the drain: %d %s after %v", r.status, r.body, time.Since(began))
	}
	m := <-done
	if m["status"] != 200 || m["drainedMs"].(float64) < 200 {
		t.Fatalf("unload: %v", m)
	}
	if run := c.get(id); run["status"] != "completed" || content(run) != words(20) {
		t.Errorf("the in-flight run did not finish: %v", run)
	}
	if running, _, _ := llm.Running(); running != "" {
		t.Errorf("%s still running", running)
	}
}

// Busy and not waiting, or busy past the bound: 409 model_busy with the
// in-flight count, and the model keeps running its work.
func TestUnloadBusyRefused(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 200")
	c.waitStatus(id, "in_progress")
	busy := func(code int, m map[string]any) {
		t.Helper()
		e, _ := m["error"].(map[string]any)
		if code != 409 || e["code"] != "model_busy" || e["in_flight"] != 1.0 {
			t.Errorf("busy: %d %v", code, m)
		}
	}
	busy(unload(t, c, chatModel, "?wait=false"))
	old := unloadWait
	unloadWait = 300 * time.Millisecond
	defer func() { unloadWait = old }()
	began := time.Now()
	busy(unload(t, c, chatModel, ""))
	if d := time.Since(began); d < 250*time.Millisecond || d > 3*time.Second {
		t.Errorf("the bounded wait took %v", d)
	}
	if running, _, _ := llm.Running(); running != chatModel {
		t.Errorf("a refused unload stopped the model: %q", running)
	}
	if run := c.get(id); run["status"] != "in_progress" {
		t.Errorf("the run was disturbed: %v", run)
	}
	c.do("POST", "/_localbase/runs/"+id+"/cancel", "")
	if r := post(t, ts, "/v1/chat/completions", chatBody("fake:reply after")); r.status != 200 {
		t.Errorf("requests after a refused unload: %d %s", r.status, r.body)
	}
	if code, m := unload(t, c, chatModel, "?wait=maybe"); code != 400 {
		t.Errorf("bad wait: %d %v", code, m)
	}
}

func TestUnloadNotLoaded(t *testing.T) {
	ts := newTestServer(t)
	c := runClient{t: t, base: ts.URL}
	if code, m := unload(t, c, chatModel, ""); code != 404 || m["error"].(map[string]any)["code"] != "model_not_loaded" {
		t.Errorf("installed, not loaded: %d %v", code, m)
	}
	if code, m := unload(t, c, "no-such-model", ""); code != 404 || m["error"].(map[string]any)["code"] != "model_not_found" {
		t.Errorf("unknown: %d %v", code, m)
	}
	if code, _ := c.do("GET", "/_localbase/models/"+chatModel+"/unload", ""); code != 405 {
		t.Errorf("GET: %d", code)
	}
}

// Unload needs models:manage; it acts on the shared model, so another
// principal's in-flight requests count and are drained, not cancelled.
func TestUnloadScope(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	s.NoAuth = false
	ctx := t.Context()
	_, reader, _ := s.Store.CreateAPIKey(ctx, "reader", []string{"models:read", "inference:chat"})
	_, admin, _ := s.Store.CreateAPIKey(ctx, "admin", []string{"models:read", "models:manage"})
	r, a := runClient{t: t, base: ts.URL, key: reader}, runClient{t: t, base: ts.URL, key: admin}
	if code, m := r.do("POST", "/v1/chat/completions", chatBody("hi")); code != 200 {
		t.Fatalf("chat: %d %v", code, m)
	}
	if code, _ := unload(t, r, chatModel, ""); code != 403 {
		t.Errorf("models:read only: %d", code)
	}
	if code, _ := unload(t, runClient{t: t, base: ts.URL}, chatModel, ""); code != 401 {
		t.Errorf("no key: %d", code)
	}
	id := r.start("fake:slow 200") // the reader's run on the shared model
	r.waitStatus(id, "in_progress")
	if code, m := unload(t, a, chatModel, "?wait=false"); code != 409 || m["error"].(map[string]any)["in_flight"] != 1.0 {
		t.Errorf("another principal's run is in flight: %d %v", code, m)
	}
	r.do("POST", "/_localbase/runs/"+id+"/cancel", "")
	if code, m := unload(t, a, chatModel, ""); code != 200 {
		t.Errorf("models:manage: %d %v", code, m)
	}
}

// The browser reaches unload through /app/api, keeping only `wait`.
func TestUnloadBrowserRoute(t *testing.T) {
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"POST", "/_localbase/models/" + chatModel + "/unload", true}, {"GET", "/_localbase/models/" + chatModel + "/unload", false},
		{"POST", "/_localbase/models//unload", false}, {"POST", "/_localbase/models/a/b/unload", false},
	} {
		if got := allowedUIRoute(c.method, c.path); got != c.ok {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	req := &http.Request{URL: &url.URL{RawQuery: "wait=false&x=1"}}
	if q := forwardedQuery("/_localbase/models/m/unload", req); q != "wait=false" {
		t.Errorf("forwarded query %q", q)
	}
	if q := forwardedQuery("/_localbase/models/m", req); q != "" {
		t.Errorf("forwarded query elsewhere %q", q)
	}
	ts := newTestServer(t)
	post(t, ts, "/v1/chat/completions", chatBody("hi"))
	if code, m := (runClient{t: t, base: ts.URL}).do("POST", "/app/api/_localbase/models/"+chatModel+"/unload?wait=false", ""); code != 200 {
		t.Errorf("through /app/api: %d %v", code, m)
	}
}

// Media runtimes unload the same way.
func TestUnloadImageRuntime(t *testing.T) {
	ts := newImageGateway(t, imageModel)
	c := runClient{t: t, base: ts.URL}
	if code, m := c.do("POST", "/v1/images/generations", `{"prompt":"a lighthouse"}`); code != 200 {
		t.Fatalf("image: %d %v", code, m)
	}
	mem := runServer(ts).Memory
	reserved := mem.Reserved()["image"]
	code, m := unload(t, c, imageModel, "")
	if code != 200 || m["runtime"] != "image" || m["memoryFreedBytes"] != float64(reserved) || reserved <= 0 {
		t.Fatalf("unload: %d %v (reserved %d)", code, m, reserved)
	}
	if _, held := mem.Reserved()["image"]; held || runServer(ts).Image.Running() != "" {
		t.Errorf("still loaded: %v %q", mem.Reserved(), runServer(ts).Image.Running())
	}
	if code, _ := c.do("POST", "/v1/images/generations", `{"prompt":"again"}`); code != 200 {
		t.Errorf("reload on demand: %d", code)
	}
}
