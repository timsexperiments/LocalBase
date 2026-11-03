package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	if page := get(u, "/app/studio").Body.String(); !strings.Contains(page, `<a href="/app/studio/image#run-run_1" class="studio-tile">`) || strings.Contains(page, "run_c") {
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
	u := New(Deps{NoAuth: true, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, "dreamshaper-8-lcm"), Call: (&fakeRuns{status: "completed"}).call})
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
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read", "models:manage", "inference:image", "inference:speech", "inference:video"}}
	call := func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
		return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
	}
	// Selected by default but not on disk is not usable: a fresh install starts like this.
	fresh := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm, RootDir: t.TempDir(), Call: call})
	if page := mainRe.FindString(get(fresh, "/app/studio").Body.String()); !strings.Contains(page, `<a href="/app/models/get?filter=images" class="studio-card">`) ||
		!strings.Contains(page, "Studio needs a model") || !strings.Contains(page, `<span class="sr-only">Needs a model</span>`) || strings.Contains(page, `<span class="tool-state">Needs a model`) {
		t.Errorf("a selected model with no files reads as ready, or nothing says so once: %s", page)
	}
	// And its page says the same: no prompt for a model that isn't there, and a way to get one.
	if page := mainRe.FindString(get(fresh, "/app/studio/image").Body.String()); !strings.Contains(page, "No picture model yet") || !strings.Contains(page, "/app/models/get?filter=images") || strings.Contains(page, `class="lab-dock"`) {
		t.Errorf("the image page offers a model that isn't on disk: %s", page)
	}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, "dreamshaper-8-lcm"), Call: call})
	main := mainRe.FindString(get(u, "/app/studio").Body.String())
	for _, want := range []string{`<h1 id="studio-title" class="page-title">Studio</h1>`, `<a href="/app/studio/image" class="studio-card">`,
		`<a href="/app/models/get?filter=speech" class="studio-card">`, `<span class="tool-state">Needs a model</span>`, "Not on your account", "What you make stays here"} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(main, `href="/app/studio/transcribe"`) {
		t.Error("a tool the account may not use is a link")
	}
	if strings.Contains(main, "Studio needs a model") || strings.Contains(main, `id="recent-title"`) {
		t.Errorf("one tool works, so no banner, and nothing made yet, so no Recent: %s", main)
	}
	// Someone who can't add models isn't sent to a page they can't use.
	p.Permissions = []string{"models:read", "inference:image", "inference:speech"}
	ask := mainRe.FindString(get(fresh, "/app/studio").Body.String())
	if !strings.Contains(ask, "No models here yet") || strings.Contains(ask, "/app/models/get") {
		t.Errorf("without models:manage: %s", ask)
	}
	if page := mainRe.FindString(get(fresh, "/app/studio/image").Body.String()); !strings.Contains(page, "Ask whoever runs this computer") || strings.Contains(page, "/app/models/get") {
		t.Errorf("a tool's page, without models:manage: %s", page)
	}
	p.Permissions = []string{"models:read", "models:manage", "inference:image", "inference:speech", "inference:video"}
	// A tool without a model has no prompt or examples, only the way to get one.
	tool := mainRe.FindString(get(u, "/app/studio/speech").Body.String())
	if !strings.Contains(tool, "No speech model yet") || strings.Contains(tool, "data-lab-empty") || strings.Contains(tool, `class="lab-dock"`) {
		t.Errorf("speech without a model: %s", tool)
	}
}

// While a result is being made its tile asks after itself and says how it stands in words,
// and once it is done it stops asking. A result that has gone answers with nothing, so the tile
// leaves the strip.
func TestStudioTile(t *testing.T) {
	f := &fakeRuns{status: "in_progress"}
	u := New(Deps{NoAuth: true, Call: f.call})
	active := get(u, "/app/studio/tile/run_1").Body.String()
	if !strings.Contains(active, `hx-get="/app/studio/tile/run_1"`) || !strings.Contains(active, "Making the image") || !strings.Contains(active, `href="/app/studio/image#run-run_1"`) {
		t.Errorf("active: %s", active)
	}
	f.status = "completed"
	done := get(u, "/app/studio/tile/run_1").Body.String()
	if strings.Contains(done, "hx-get") || !strings.Contains(done, "data-result-img") {
		t.Errorf("done: %s", done)
	}
	if gone := get(u, "/app/studio/tile/nope"); gone.Code != http.StatusOK || strings.TrimSpace(gone.Body.String()) != "" {
		t.Errorf("gone: %d %s", gone.Code, gone.Body.String())
	}
}

// After a restart a run has lost the note that held its words, so it has nothing to show. The tool
// says so once, with one way to clear them, and the landing leaves them out. A run that kept its
// words stays a row of its own, with its Try again.
func TestLostRuns(t *testing.T) {
	calls := []string{}
	lost := func(id string) string {
		return `{"id":"` + id + `","target":"/v1/images/generations","status":"interrupted","reason":"gateway_restart"}`
	}
	cat, _ := catalog.Load("")
	cfg := &store.Config{Selected: map[string][]string{"image": {"dreamshaper-8-lcm"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	u := New(Deps{NoAuth: true, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, "dreamshaper-8-lcm"), Call: func(_ context.Context, _ *auth.Principal, method, path, _ string, _ []byte) APIResult {
		if method == "DELETE" {
			calls = append(calls, path)
		}
		return APIResult{Status: 200, Body: []byte(`{"data":[` + lost("run_a") + `,` + lost("run_b") + `]}`)}
	}})
	page := mainRe.FindString(get(u, "/app/studio/image").Body.String())
	if !strings.Contains(page, "2 earlier results didn’t finish") || strings.Contains(page, `id="run-run_a"`) || strings.Contains(page, "Generate it again") {
		t.Errorf("lost runs should be one line, not a row each: %s", page)
	}
	if other := mainRe.FindString(get(u, "/app/studio/speech").Body.String()); strings.Contains(other, "earlier result") {
		t.Errorf("another tool's page shouldn't mention them: %s", other)
	}
	if landing := mainRe.FindString(get(u, "/app/studio").Body.String()); strings.Contains(landing, "tile-run_a") || strings.Contains(landing, `id="recent-title"`) {
		t.Errorf("the landing has nothing to show for them: %s", landing)
	}
	r := htmxPost("/app/lab/runs/clear-lost", url.Values{"tool": {"image"}})
	withCSRF(u, r)
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "" || strings.Join(calls, ",") != "/_localbase/runs/run_a,/_localbase/runs/run_b" {
		t.Errorf("clear: %d %q %v", w.Code, w.Body.String(), calls)
	}
	calls = nil
	r = htmxPost("/app/lab/runs/clear-lost", url.Values{"tool": {"speech"}})
	withCSRF(u, r)
	u.ServeHTTP(httptest.NewRecorder(), r)
	if len(calls) != 0 {
		t.Errorf("clearing one tool removed another's: %v", calls)
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
		"title=\"The same prompt and seed\">Regenerate</button>", "<span>Edit</span>", "<span>Vary</span>", "<dt>Model</dt><dd>some-model</dd>", `data-copy-text="a boat"`, `data-seed="8"`, "A saved copy includes its prompt."} {
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
	perms := []string{"models:read", "models:manage", "inference:decision"}
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, "qwen3.5-4b-q4_k_m", "clef-flash-q4_k_m"),
		Call: func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
			return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
		}})
	if main := mainRe.FindString(get(u, "/app/studio").Body.String()); !strings.Contains(main, `<a href="/app/studio/decisions" class="studio-card">`) {
		t.Errorf("no landing card: %s", main)
	}
	body := get(u, "/app/studio/decisions").Body.String()
	main := mainRe.FindString(body)
	for _, want := range []string{`id="decide"`, `name="decide-type" value="choice"`, `name="decide-type" value="noul"`, `name="decide-type" value="score"`,
		`data-open-sheet="decide-template"`, `name="question"`, `name="model" value="clef-flash-q4_k_m"`, `id="decide-code"`, `>Decide</button>`} {
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

// A real picture, so an edit can read its size.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Edit (DESIGN §27.5): a result goes to the edit route as a file, keeping its size, and only with a
// model that can edit. A model that can't, a picture that has gone and another person's picture never
// reach the API.
func TestImageEdit(t *testing.T) {
	cat, _ := catalog.Load("")
	const plain, capable = "dreamshaper-8-lcm", "juggernaut-xl-v9"
	cfg := &store.Config{Selected: map[string][]string{"image": {plain, capable}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	var posts []string
	var form *multipart.Form
	status := "completed"
	call := func(_ context.Context, _ *auth.Principal, method, path, ctype string, body []byte) APIResult {
		switch {
		case method == "GET" && path == "/_localbase/runs/run_e":
			return APIResult{Status: 200, Body: []byte(`{"id":"run_e","target":"/v1/images/edits","status":"` + status + `"}`)}
		case method == "GET" && path == "/_localbase/runs/run_e/result":
			img := base64.StdEncoding.EncodeToString(testPNG(t, 8, 8))
			return APIResult{Status: 200, Body: []byte(`{"data":[{"b64_json":"` + img + `"}]}`)}
		case method == "GET":
			return APIResult{Status: 200, Body: []byte(`{"object":"list","data":[]}`)}
		}
		posts = append(posts, path)
		_, params, _ := mime.ParseMediaType(ctype)
		form, _ = multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(1 << 20)
		return APIResult{Status: 202, Body: []byte(`{"id":"run_e","target":"/v1/images/edits","status":"queued"}`)}
	}
	u := New(Deps{NoAuth: true, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, plain, capable), Call: call})

	page := mainRe.FindString(get(u, "/app/studio/image").Body.String())
	if strings.Contains(page, `id="image-needs-edit"`) {
		t.Error("a model here can edit, so nothing should say one is needed")
	}
	for id, want := range map[string]bool{plain: false, capable: true} {
		radio := regexp.MustCompile(`<input type="radio" name="model" value="` + id + `"[^>]*>`).FindString(page)
		if radio == "" || strings.Contains(radio, "data-edits") != want {
			t.Errorf("%s: %q, want data-edits %v", id, radio, want)
		}
	}

	owner := (&auth.Principal{Kind: "browser-session", ID: "no-auth"}).OwnerID()
	picture := testPNG(t, 512, 768)
	source := u.results.put(owner, "image/png", picture)
	send := func(path string, v url.Values) string {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		r.Header.Set("X-Localbase-UI", "1")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		withCSRF(u, r)
		w := httptest.NewRecorder()
		u.ServeHTTP(w, r)
		return w.Body.String()
	}
	post := func(v url.Values) string { t.Helper(); return send("/app/lab/image", v) }
	item := post(url.Values{"prompt": {"the same boat"}, "model": {capable}, "source": {source}, "change": {"little"}, "look": {"watercolor"}, "shape": {"square"}, "count": {"2"}, "seed": {"7"}})
	if len(posts) != 1 || posts[0] != "/_localbase/runs/v1/images/edits" || form == nil {
		t.Fatalf("posts %v, form %v: %s", posts, form, item)
	}
	value := func(name string) string {
		if v := form.Value[name]; len(v) == 1 {
			return v[0]
		}
		return ""
	}
	if value("model") != capable || value("strength") != "0.35" || value("size") != "512x768" || value("seed") != "7" || value("n") != "2" || value("response_format") != "b64_json" || !strings.HasPrefix(value("prompt"), "the same boat, watercolor") {
		t.Errorf("the edit's fields: %v", form.Value)
	}
	if files := form.File["image"]; len(files) != 1 {
		t.Errorf("want one image, got %d", len(files))
	} else {
		f, _ := files[0].Open()
		got, _ := io.ReadAll(f)
		if !bytes.Equal(got, picture) || files[0].Header.Get("Content-Type") != "image/png" {
			t.Errorf("the file sent isn't the picture: %d bytes, %q", len(got), files[0].Header.Get("Content-Type"))
		}
	}
	if !strings.Contains(item, "id=\"run-run_e\"") {
		t.Errorf("no tray item: %s", item)
	}

	// Vary edits the same picture again.
	posts = nil
	out := send("/app/lab/runs/run_e/again", url.Values{"mode": {"vary"}})
	if len(posts) != 1 || posts[0] != "/_localbase/runs/v1/images/edits" {
		t.Errorf("Vary on an edit: %v: %s", posts, out)
	}

	// Everything that repeats an edit starts from the same picture while it is kept.
	item = func() string {
		status = "failed"
		out := get(u, "/app/lab/runs/run_e").Body.String()
		status = "completed"
		return out
	}()
	if !strings.Contains(item, `data-edit-picture="/app/lab/results/`+source+`"`) {
		t.Errorf("Try again on an edit should start from its picture: %s", item)
	}
	done := get(u, "/app/lab/runs/run_e").Body.String()
	if !strings.Contains(done, `data-from="/app/lab/results/`+source+`"`) || !strings.Contains(done, "viewer-from") || !strings.Contains(done, "Edited") {
		t.Errorf("a finished edit should say what it started from: %s", done)
	}

	// None of these should reach the API.
	posts = nil
	for name, c := range map[string]struct {
		v    url.Values
		want string
	}{
		"a model that can't":       {url.Values{"prompt": {"x"}, "model": {plain}, "source": {source}}, "can’t edit pictures"},
		"a picture that's gone":    {url.Values{"prompt": {"x"}, "model": {capable}, "source": {"00000000000000000000000000000000"}}, "no longer here"},
		"another person's picture": {url.Values{"prompt": {"x"}, "model": {capable}, "source": {u.results.put("someone-else", "image/png", picture)}}, "no longer here"},
	} {
		if out := post(c.v); !strings.Contains(out, c.want) || len(posts) != 0 {
			t.Errorf("%s: posts %v: %s", name, posts, out)
		}
	}

	// Once the picture has gone, nothing that repeats the edit quietly makes a new picture instead.
	u.results.mu.Lock()
	delete(u.results.items, source)
	u.results.mu.Unlock()
	if out := send("/app/lab/runs/run_e/again", url.Values{"mode": {"vary"}}); !strings.Contains(out, "no longer here") || len(posts) != 0 {
		t.Errorf("Vary with the picture gone: posts %v: %s", posts, out)
	}
	gone := get(u, "/app/lab/runs/run_e").Body.String()
	if strings.Contains(gone, "data-reuse") || strings.Contains(gone, "viewer-from") || !strings.Contains(gone, "Edited") {
		t.Errorf("a finished edit whose picture has gone offers Reuse settings: %s", gone)
	}
	status = "failed"
	if out := get(u, "/app/lab/runs/run_e").Body.String(); strings.Contains(out, "Try again") {
		t.Errorf("a failed edit whose picture has gone offers Try again: %s", out)
	}

	// With no model that can, the page says so, and how to get one.
	alone := New(Deps{NoAuth: true, Catalog: cat, LLM: runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &store.Config{Selected: map[string][]string{"image": {plain}}}), RootDir: installedRoot(t, cat, plain), Call: call})
	none := mainRe.FindString(get(alone, "/app/studio/image").Body.String())
	if !strings.Contains(none, `id="image-needs-edit"`) || !strings.Contains(none, "/app/models/get?filter=images") || strings.Contains(none, "data-edits") {
		t.Errorf("no model that can edit: %s", none)
	}
}
