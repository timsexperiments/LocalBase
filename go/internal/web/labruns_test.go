package web

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// fakeRuns answers the runs API the way the gateway does (DESIGN §21).
type fakeRuns struct {
	status string // the run's status on GET
	reason string
	calls  []string
}

func (f *fakeRuns) call(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
	f.calls = append(f.calls, method+" "+path)
	run := `{"id":"run_1","target":"/v1/images/generations","status":"` + f.status + `","reason":"` + f.reason + `","label":"Image"}`
	switch {
	case method == "GET" && path == "/_localbase/runs":
		return APIResult{Status: 200, Body: []byte(`{"object":"list","data":[` + run + `,{"id":"run_c","target":"/v1/chat/completions","status":"in_progress"}]}`)}
	case method == "POST" && path == "/_localbase/runs/v1/images/generations":
		return APIResult{Status: 202, Body: []byte(`{"id":"run_1","target":"/v1/images/generations","status":"queued"}`)}
	case method == "GET" && path == "/_localbase/runs/run_1":
		return APIResult{Status: 200, Body: []byte(run)}
	case method == "GET" && path == "/_localbase/runs/run_1/result":
		png := base64.StdEncoding.EncodeToString([]byte("\x89PNG"))
		return APIResult{Status: 200, Body: []byte(`{"data":[{"b64_json":"` + png + `"}]}`)}
	case path == "/_localbase/runs/run_1/cancel" || method == "DELETE":
		f.status = "cancelled"
		return APIResult{Status: 200, Body: []byte(`{}`)}
	}
	return APIResult{Status: 404, Body: []byte(`{"error":{"message":"not found"}}`)}
}

// With runs, a Lab image is a run: the form answers with a tray item that
// polls; the item shows the image once the run completes, captioned with
// the prompt; the tray is rebuilt from the person's runs (chat runs left
// out); Stop and Remove go to the run.
func TestLabImageRun(t *testing.T) {
	f := &fakeRuns{status: "in_progress"}
	u := New(Deps{NoAuth: true, Call: f.call})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/image", url.Values{"prompt": {"a paper   boat"}, "size": {"512x512"}}))
	item := w.Body.String()
	if w.Header().Get("HX-Retarget") != "#lab-items" || w.Header().Get("HX-Reswap") != "afterbegin" {
		t.Fatalf("headers: %v", w.Header())
	}
	if !strings.Contains(item, `<li id="run-run_1"`) || !strings.Contains(item, `hx-get="/app/lab/runs/run_1" hx-trigger="every 2s"`) ||
		!strings.Contains(item, "a paper boat") || !strings.Contains(item, "/app/lab/runs/run_1/stop") || strings.Contains(item, "<html") {
		t.Fatalf("started item: %s", item)
	}
	f.status = "completed"
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/lab/runs/run_1", nil))
	done := w.Body.String()
	if !strings.Contains(done, `alt="a paper boat"`) || !strings.Contains(done, `src="/app/lab/results/`) || strings.Contains(done, "every 2s") ||
		!strings.Contains(done, `aria-label="Save image"`) || !strings.Contains(done, `data-share="/app/lab/results/`) || strings.Contains(done, "Try again") {
		t.Fatalf("completed item: %s", done)
	}
	// The result is collected once, however often the item is drawn.
	before := strings.Count(strings.Join(f.calls, "\n"), "/result")
	u.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/app/lab/runs/run_1", nil))
	if strings.Count(strings.Join(f.calls, "\n"), "/result") != before {
		t.Error("the result was fetched again")
	}
	page := get(u, "/app/lab?tab=speech").Body.String()
	if !strings.Contains(page, `<li id="run-run_1"`) || strings.Contains(page, "run-run_c") {
		t.Errorf("tray: %s", page)
	}
	// Interrupted: Try again puts the whole prompt back.
	f.status, f.reason = "interrupted", "gateway_restart"
	lost := get(u, "/app/lab/runs/run_1").Body.String()
	if !strings.Contains(lost, `data-fill="a paper   boat" data-fill-tab="image">Try again</button>`) {
		t.Errorf("interrupted item: %s", lost)
	}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/runs/run_1/remove", url.Values{}))
	if w.Code != 200 || w.Body.Len() != 0 || f.calls[len(f.calls)-1] != "DELETE /_localbase/runs/run_1" {
		t.Errorf("remove: %d %q %v", w.Code, w.Body.String(), f.calls[len(f.calls)-1])
	}
}

// A media run lost to a gateway restart says so and asks to try again.
func TestLabRunInterrupted(t *testing.T) {
	f := &fakeRuns{status: "interrupted", reason: "gateway_restart"}
	u := New(Deps{NoAuth: true, Call: f.call})
	out := get(u, "/app/lab/runs/run_1").Body.String()
	if !strings.Contains(out, "LocalBase restarted before this finished") || strings.Contains(out, "every 2s") {
		t.Errorf("interrupted: %s", out)
	}
	w := httptest.NewRecorder()
	f.status = "in_progress"
	u.ServeHTTP(w, htmxPost("/app/lab/runs/run_1/stop", url.Values{}))
	if !strings.Contains(strings.Join(f.calls, "\n"), "POST /_localbase/runs/run_1/cancel") || !strings.Contains(w.Body.String(), "Stopped.") {
		t.Errorf("stop: %s", w.Body.String())
	}
}

// The Lab page: one switch, results first, a docked prompt with chips that
// open sheets, and no form card.
func TestLabDock(t *testing.T) {
	cat, _ := catalog.Load("")
	cfg := &store.Config{Active: map[string]string{"image": "dreamshaper-8-lcm"}, Selected: map[string][]string{"image": {"dreamshaper-8-lcm"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	u := New(Deps{NoAuth: true, Catalog: cat, LLM: llm, Call: (&fakeRuns{status: "completed"}).call})
	main := mainRe.FindString(get(u, "/app/lab?tab=image").Body.String())
	for _, want := range []string{`<nav aria-label="Lab tools" class="segmented`, `id="lab-items"`, `class="lab-dock" data-docked`,
		`data-open-sheet="lab-model"`, `data-open-sheet="lab-size"`, `<dialog id="lab-size" class="sheet`, `aria-label="Generate"`, `class="composer-box lab-box"`, `data-fill="A paper boat`} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Index(main, `id="lab-items"`) > strings.Index(main, `class="lab-dock"`) {
		t.Error("the prompt comes before the results")
	}
}

// A request that reached a model while it was being unloaded says so in
// plain words, on runs and on direct calls.
func TestModelUnloadingMessage(t *testing.T) {
	f := &fakeRuns{status: "failed"}
	u := New(Deps{NoAuth: true, Call: func(ctx context.Context, p *auth.Principal, method, path, ct string, body []byte) APIResult {
		if path == "/_localbase/runs/run_1" {
			return APIResult{Status: 200, Body: []byte(`{"id":"run_1","target":"/v1/images/generations","status":"failed","error":{"code":"model_unloading","message":"Model is unloading."}}`)}
		}
		return f.call(ctx, p, method, path, ct, body)
	}})
	if out := get(u, "/app/lab/runs/run_1").Body.String(); !strings.Contains(out, unloadingMessage) {
		t.Errorf("run: %s", out)
	}
	if got := (APIResult{Status: 503, Body: []byte(`{"error":{"code":"model_unloading","message":"x"}}`)}).apiError(); got != unloadingMessage {
		t.Errorf("direct: %q", got)
	}
}
