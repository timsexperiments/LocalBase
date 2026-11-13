package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Search in Studio (DESIGN §35): its card comes last on the landing, it offers only selected embedding
// models, and the starter policy's member role, which lacks inference:embeddings, gets the denied state.
func TestSearchPage(t *testing.T) {
	const chat, embed, embed2 = "qwen3.5-4b-q4_k_m", "qwen3-embedding-0.6b-q8_0", "qwen3-embedding-4b-q4_k_m"
	cat, _ := catalog.Load("")
	for _, id := range []string{embed, embed2} {
		if s := cat.ByID(id); s == nil || !s.IsEmbedding() {
			t.Fatalf("%s should be an embedding model in the catalog", id)
		}
	}
	starter := auth.StarterPolicy("owner@example.com", "https://issuer.example")
	perms := func(role string) []string {
		i := slices.IndexFunc(starter.Roles, func(r auth.Role) bool { return r.Name == role })
		return starter.Roles[i].Permissions
	}
	cfg := &store.Config{Selected: map[string][]string{"llm": {chat, embed}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms("admin")}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, chat, embed, embed2),
		Call: func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
			return APIResult{Status: 200, Body: []byte(`{"data":[]}`)}
		}})

	landing := mainRe.FindString(get(u, "/app/studio").Body.String())
	card := `<a href="/app/studio/search" class="studio-card">`
	if !strings.Contains(landing, card) || !strings.Contains(landing, "Your own text, searched by meaning.") {
		t.Errorf("no Search card for the admin role: %s", landing)
	}
	if strings.Index(landing, card) < strings.Index(landing, `<a href="/app/studio/decisions" class="studio-card">`) {
		t.Error("Search should come last on the landing, after Decisions")
	}

	body := get(u, "/app/studio/search").Body.String()
	main := mainRe.FindString(body)
	for _, want := range []string{
		`<h1 id="lab-title" class="page-title">Search</h1>`,
		`data-search`,
		"What would you like to search?",
		">Paste text<", ">Choose a file<",
		"Up to 2,000 passages.",
		"Your text is sent only to your LocalBase to be searched, and nothing is saved. Leaving the page forgets it.",
		"Search and chat share a place on this computer, so the next chat message may take a moment to start.",
		`placeholder="Ask about your text"`,
		`data-open-sheet="search-text"`,
		`id="search-text-title"`, ">Your text<", ">Clear text<",
		`accept=".txt,.md,text/plain,text/markdown"`,
		`name="model" value="` + embed + `"`,
		`<h2 id="results-title" class="sr-only">Passages</h2>`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Only embedding models are offered, and one needs no chooser.
	if strings.Contains(main, chat) || strings.Contains(main, `data-open-sheet="lab-model"`) {
		t.Errorf("a chat model, or a chooser for one model: %s", main)
	}
	if !strings.Contains(body, "js/search.js") {
		t.Error("the page's script is missing")
	}
	// TestSizeBudgets measures this page without a model; with one, the whole tool is drawn.
	if n := gzipSize([]byte(body)); n > budgetPageHTML {
		t.Errorf("the page is %d bytes gzipped, budget %d", n, budgetPageHTML)
	}
	// Each tool loads its own code (§27.2f): no other tool's script, and Search's nowhere else.
	for _, other := range []string{"js/image.js", "js/video.js", "js/speech.js", "js/transcribe.js", "js/decisions.js"} {
		if strings.Contains(body, other) {
			t.Errorf("Search loads %s", other)
		}
	}
	if strings.Contains(get(u, "/app/studio/image").Body.String(), "js/search.js") {
		t.Error("Image loads Search's script")
	}

	cfg.Selected["llm"] = []string{chat, embed, embed2}
	two := mainRe.FindString(get(u, "/app/studio/search").Body.String())
	if !strings.Contains(two, `data-open-sheet="lab-model"`) || !strings.Contains(two, embed2) || strings.Contains(two, chat) {
		t.Errorf("two embedding models should be chosen between, and only those: %s", two)
	}

	p.Permissions = perms("member")
	member := mainRe.FindString(get(u, "/app/studio").Body.String())
	if strings.Contains(member, card) || !strings.Contains(member, "Not on your account") {
		t.Errorf("the member role should see Search as not on their account: %s", member)
	}
	denied := mainRe.FindString(get(u, "/app/studio/search").Body.String())
	if !strings.Contains(denied, "Your access does not include search.") || strings.Contains(denied, "Ask about your text") {
		t.Errorf("denied: %s", denied)
	}

	p.Permissions = perms("admin")
	cfg.Selected["llm"] = []string{chat}
	none := mainRe.FindString(get(u, "/app/studio/search").Body.String())
	if !strings.Contains(none, "No search model yet") || strings.Contains(none, "Ask about your text") {
		t.Errorf("no model: %s", none)
	}
	if main := mainRe.FindString(get(u, "/app/studio").Body.String()); !strings.Contains(main, `href="/app/models/get?filter=embeddings"`) {
		t.Errorf("no way to get a model: %s", main)
	}
}
