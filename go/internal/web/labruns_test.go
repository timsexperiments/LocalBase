package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
		!strings.Contains(done, `download="image.png"`) || !strings.Contains(done, `data-share="/app/lab/results/`) || strings.Contains(done, "Try again") {
		t.Fatalf("completed item: %s", done)
	}
	// The result is collected once, however often the item is drawn.
	before := strings.Count(strings.Join(f.calls, "\n"), "/result")
	u.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/app/lab/runs/run_1", nil))
	if strings.Count(strings.Join(f.calls, "\n"), "/result") != before {
		t.Error("the result was fetched again")
	}
	// Each tool's tray holds its own results; the landing shows the latest.
	page := get(u, "/app/studio/image").Body.String()
	if !strings.Contains(page, `<li id="run-run_1"`) || strings.Contains(page, "run-run_c") {
		t.Errorf("tray: %s", page)
	}
	if page := get(u, "/app/studio/speech").Body.String(); strings.Contains(page, `<li id="run-run_1"`) {
		t.Error("an image result in the Speech tray")
	}
	if page := get(u, "/app/studio").Body.String(); !strings.Contains(page, `class="studio-tile" aria-label="Image: a paper boat"`) || strings.Contains(page, "run_c") {
		t.Errorf("recent: %s", page)
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
	main := mainRe.FindString(get(u, "/app/studio/image").Body.String())
	for _, want := range []string{`aria-label="Back to Studio"`, `<h1 id="lab-title" class="page-title">Image</h1>`, `id="lab-items"`, `class="lab-dock" data-docked`,
		`data-open-sheet="image-shape"`, `data-open-sheet="image-look"`, `data-open-sheet="image-count"`, `<dialog id="image-shape" class="sheet`, `aria-label="Generate"`, `class="composer-box lab-box"`, `data-fill="A paper boat`} {
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

// The landing: a card per tool. Ready tools open their page; one without a
// model says so and the whole card goes to Get models for that task; one the
// account may not use is not a link. An empty Recent says what will appear.
func TestStudioLanding(t *testing.T) {
	cat, _ := catalog.Load("")
	cfg := &store.Config{Selected: map[string][]string{"image": {"dreamshaper-8-lcm"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read", "inference:image", "inference:speech", "inference:video"}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm,
		Call: func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
			return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
		}})
	main := mainRe.FindString(get(u, "/app/studio").Body.String())
	for _, want := range []string{`<h1 id="studio-title" class="page-title">Studio</h1>`, `<a href="/app/studio/image" class="studio-card">`,
		`<a href="/app/models/get?filter=speech" class="studio-card">`, "Needs a model", "Not on your account", "What you make appears here"} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(main, `href="/app/studio/transcribe"`) {
		t.Error("a tool the account may not use is a link")
	}
	// A tool without a model has no prompt or examples, only the way to get one.
	tool := mainRe.FindString(get(u, "/app/studio/speech").Body.String())
	if !strings.Contains(tool, "No speech model yet") || strings.Contains(tool, "data-lab-empty") || strings.Contains(tool, `class="lab-dock"`) {
		t.Errorf("speech without a model: %s", tool)
	}
}

// Old Lab links land on the new pages; an unknown tool is not found.
func TestLabRedirects(t *testing.T) {
	u := New(Deps{NoAuth: true})
	for from, to := range map[string]string{"/app/lab": "/app/studio", "/app/lab?tab=speech": "/app/studio/speech", "/app/lab?tab=bogus": "/app/studio"} {
		if w := get(u, from); w.Code != http.StatusFound || w.Header().Get("Location") != to {
			t.Errorf("%s: %d %s", from, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get(u, "/app/studio/bogus"); w.Code != http.StatusNotFound {
		t.Errorf("unknown tool: %d", w.Code)
	}
}

// A generated picture carries its fallback (drawing and "Expired"), hidden
// until shell.js finds the file gone; the times are in words.
func TestResultImageFallback(t *testing.T) {
	f := &fakeRuns{status: "completed"}
	u := New(Deps{NoAuth: true, Call: f.call})
	out := get(u, "/app/lab/runs/run_1").Body.String()
	if !strings.Contains(out, `data-result-img>`) || !strings.Contains(out, `data-result-gone hidden>`) || !strings.Contains(out, "<b>Expired</b>") {
		t.Errorf("no fallback: %s", out)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{10 * time.Second: "just now", 2 * time.Minute: "2 min ago", 3 * time.Hour: "3 h ago", 50 * time.Hour: "2 d ago"} {
		if got := agoText(now.Add(-d), now); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
	if agoText(time.Time{}, now) != "" {
		t.Error("no time should say nothing")
	}
}

// imageCalls records what the Image studio asks the runs API for.
type imageCalls struct {
	fakeRuns
	bodies []map[string]any
	seeds  []int64 // what the finished run's images report
}

func (f *imageCalls) call(ctx context.Context, p *auth.Principal, method, path, ct string, body []byte) APIResult {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG"))
	switch {
	case method == "POST" && path == "/_localbase/runs/v1/images/generations":
		var in map[string]any
		json.Unmarshal(body, &in)
		f.bodies = append(f.bodies, in)
		return APIResult{Status: 202, Body: []byte(`{"id":"run_1","target":"/v1/images/generations","status":"queued"}`)}
	case path == "/_localbase/runs/run_1/result":
		var data []string
		for _, s := range f.seeds {
			data = append(data, fmt.Sprintf(`{"b64_json":%q,"seed":%d}`, png, s))
		}
		return APIResult{Status: 200, Body: []byte(`{"data":[` + strings.Join(data, ",") + `]}`)}
	}
	return f.fakeRuns.call(ctx, p, method, path, ct, body)
}

// A batch is one run with n; shape and resolution pick the size; a look adds
// its words, and the caption stays the person's own prompt.
func TestImageBatch(t *testing.T) {
	f := &imageCalls{fakeRuns: fakeRuns{status: "in_progress"}}
	u := New(Deps{NoAuth: true, Call: f.call})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/image", url.Values{"prompt": {"a boat"}, "shape": {"portrait"}, "resolution": {"large"},
		"look": {"watercolor"}, "count": {"4"}, "seed": {"100"}, "negative_prompt": {"people"}}))
	body := w.Body.String()
	if len(f.bodies) != 1 || strings.Count(body, "image-tile image-portrait") != 4 {
		t.Fatalf("%d runs: %s", len(f.bodies), body)
	}
	b := f.bodies[0]
	if b["n"] != float64(4) || b["seed"] != float64(100) || b["size"] != "768x1024" ||
		!strings.HasPrefix(b["prompt"].(string), "a boat, watercolor painting") || !strings.HasPrefix(b["negative_prompt"].(string), "people, ") {
		t.Errorf("request: %v", b)
	}
	if strings.Contains(body, "watercolor painting") || !strings.Contains(body, ">a boat<") {
		t.Error("the caption is not the person's own prompt")
	}
	// An unknown count is one picture, with a seed the page chose, with room
	// under the API's limit for a batch.
	f.bodies = nil
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/image", url.Values{"prompt": {"x"}, "count": {"99"}, "size": {"512x512"}}))
	if len(f.bodies) != 1 || f.bodies[0]["n"] != nil || f.bodies[0]["seed"] == nil || f.bodies[0]["size"] != "512x512" {
		t.Errorf("default: %v", f.bodies)
	}
	for range 1000 {
		if s := randomSeed(); s < 0 || s > 2147483647-10 {
			t.Fatalf("seed %d", s)
		}
	}
}

// Each picture of a batch has the seed the API reports, its own viewer and its
// own Delete. Regenerate repeats that picture's seed, Vary picks a new one;
// both make one picture. The model's name appears only in the details.
func TestImageAgain(t *testing.T) {
	f := &imageCalls{fakeRuns: fakeRuns{status: "completed"}, seeds: []int64{7, 8}}
	u := New(Deps{NoAuth: true, Call: f.call})
	u.ServeHTTP(httptest.NewRecorder(), htmxPost("/app/lab/image", url.Values{"prompt": {"a boat"}, "shape": {"landscape"}, "seed": {"7"}, "count": {"2"}, "model": {"some-model"}}))
	out := get(u, "/app/lab/runs/run_1").Body.String()
	for _, want := range []string{`<dialog id="view-run_1-0" class="viewer"`, `<dialog id="view-run_1-1" class="viewer"`, "<dt>Seed</dt><dd>7</dd>", "<dt>Seed</dt><dd>8</dd>",
		"<span>Regenerate</span>", "<span>Vary</span>", "<dt>Model</dt><dd>some-model</dd>", `data-copy-text="a boat"`, `data-seed="8"`, "A saved copy includes its prompt."} {
		if !strings.Contains(out, want) {
			t.Errorf("viewer: missing %q", want)
		}
	}
	if strings.Count(out, "some-model") != 2 { // once per viewer, under Details
		t.Errorf("the model name appears outside the details: %d", strings.Count(out, "some-model"))
	}
	for _, c := range []struct {
		mode string
		same bool
	}{{"same", true}, {"vary", false}} {
		mode, same := c.mode, c.same
		f.bodies = nil
		w := httptest.NewRecorder()
		u.ServeHTTP(w, htmxPost("/app/lab/runs/run_1/again", url.Values{"mode": {mode}, "picture": {"1"}}))
		if len(f.bodies) != 1 || f.bodies[0]["prompt"] != "a boat" || f.bodies[0]["size"] != "768x512" || f.bodies[0]["n"] != nil {
			t.Fatalf("%s: %v %s", mode, f.bodies, w.Body.String())
		}
		if (f.bodies[0]["seed"] == float64(8)) != same {
			t.Errorf("%s: seed %v", mode, f.bodies[0]["seed"])
		}
	}
	// A result whose settings are gone says so.
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/runs/run_nope/again", url.Values{"mode": {"vary"}}))
	if !strings.Contains(w.Body.String(), "no longer available") {
		t.Errorf("gone: %s", w.Body.String())
	}
}

// Delete takes one picture of a batch; the last one takes the run with it.
func TestImageDelete(t *testing.T) {
	f := &imageCalls{fakeRuns: fakeRuns{status: "completed"}, seeds: []int64{1, 2}}
	u := New(Deps{NoAuth: true, Call: f.call})
	u.ServeHTTP(httptest.NewRecorder(), htmxPost("/app/lab/image", url.Values{"prompt": {"a"}, "count": {"2"}}))
	get(u, "/app/lab/runs/run_1")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/runs/run_1/remove", url.Values{"picture": {"0"}}))
	if strings.Contains(w.Body.String(), "view-run_1-0") || !strings.Contains(w.Body.String(), "view-run_1-1") {
		t.Errorf("after one: %s", w.Body.String())
	}
	if strings.Contains(strings.Join(f.calls, " "), "DELETE") {
		t.Error("the run was dismissed with a picture left")
	}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/runs/run_1/remove", url.Values{"picture": {"1"}}))
	if w.Body.Len() != 0 || !strings.Contains(strings.Join(f.calls, " "), "DELETE /_localbase/runs/run_1") {
		t.Errorf("after the last: %q %v", w.Body.String(), f.calls)
	}
}

// Decisions has a landing card and its three states; its words and odds are
// drawn in the browser (tools/jstest/decisions.test.ts, chat-runs-check).
func TestDecisionsPage(t *testing.T) {
	cat, _ := catalog.Load("")
	cfg := &store.Config{Selected: map[string][]string{"llm": {"qwen3.5-4b-q4_k_m", "clef-flash-q4_k_m"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	perms := []string{"models:read", "inference:decision"}
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm,
		Call: func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
			return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
		}})
	if main := mainRe.FindString(get(u, "/app/studio").Body.String()); !strings.Contains(main, `<a href="/app/studio/decisions" class="studio-card">`) {
		t.Errorf("no landing card: %s", main)
	}
	body := get(u, "/app/studio/decisions").Body.String()
	main := mainRe.FindString(body)
	for _, want := range []string{`id="decide"`, `name="decide-type" value="choice"`, `name="decide-type" value="noul"`, `name="decide-type" value="score"`,
		`data-open-sheet="decide-template"`, `name="question"`, `name="model" value="clef-flash-q4_k_m"`, `id="decide-code"`, `aria-label="Decide"`} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Only decision models answer here, and one model needs no choosing.
	if strings.Contains(main, "qwen3.5") || strings.Contains(main, `data-open-sheet="lab-model"`) {
		t.Errorf("a chat model or a one-way choice: %s", main)
	}
	if !strings.Contains(body, "decisions.js") {
		t.Error("the page's script is missing")
	}

	p.Permissions = []string{"models:read"}
	denied := mainRe.FindString(get(u, "/app/studio/decisions").Body.String())
	if !strings.Contains(denied, "Your access does not include decisions.") || strings.Contains(denied, `id="decide"`) {
		t.Errorf("denied: %s", denied)
	}
	p.Permissions = perms
	cfg.Selected["llm"] = []string{"qwen3.5-4b-q4_k_m"}
	none := mainRe.FindString(get(u, "/app/studio/decisions").Body.String())
	if !strings.Contains(none, "No decision model yet") || strings.Contains(none, `id="decide"`) {
		t.Errorf("no model: %s", none)
	}
	if main := mainRe.FindString(get(u, "/app/studio").Body.String()); !strings.Contains(main, `href="/app/models/get?filter=decisions"`) {
		t.Errorf("no way to get a model: %s", main)
	}
}
