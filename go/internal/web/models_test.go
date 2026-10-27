package web

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// installSparse puts a qualified chat model's files in the UI's root at
// their expected sizes (sparse, so nothing real is written) and returns
// its id.
func installSparse(t *testing.T, u *UI) string {
	t.Helper()
	for _, s := range u.d.Catalog.All() {
		if t, chat := task(s); !chat || t != "Chat" || !s.Qualified() || s.ExperimentalRuntime {
			continue
		}
		dir := filepath.Join(u.d.RootDir, "models", s.Kind)
		os.MkdirAll(dir, 0o755)
		for _, a := range s.Artifacts {
			f, err := os.Create(filepath.Join(dir, a.Filename))
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(a.ExpectedSizeBytes)
			f.Close()
		}
		return s.ModelID
	}
	t.Fatal("no qualified chat model in the catalog")
	return ""
}

var mainRe = regexp.MustCompile(`(?s)<main id="main".*?</main>`)

// The Library leads with the models: one title, one "get" action, no stats
// card or second heading, and each row is one link with its state in words (not enabled: Off).
func TestLibraryModelsFirst(t *testing.T) {
	u := shellUI(t, nil, "")
	id := installSparse(t, u)
	main := mainRe.FindString(get(u, "/app/models").Body.String())
	if !strings.Contains(main, `<h1 id="models-title" class="page-title">Models</h1>`) || strings.Count(main, "<h1") != 1 {
		t.Errorf("title: %s", main)
	}
	for _, gone := range []string{"eyebrow", "Your library", "Memory available now"} {
		if strings.Contains(main, gone) {
			t.Errorf("still shows %q", gone)
		}
	}
	if n := strings.Count(main, `href="/app/models/get"`); n != 2 {
		t.Errorf("%d links to Get models, want the + and the switch", n)
	}
	if !strings.Contains(main, `aria-label="Get a model"`) {
		t.Error("no + action")
	}
	if !regexp.MustCompile(`<a href="/app/models/m/` + regexp.QuoteMeta(id) + `" class="model-row">[\s\S]*?<span class="pill">Off</span>`).MatchString(main) {
		t.Errorf("row: %s", main)
	}
	if !strings.Contains(main, "1 installed · ") || !strings.Contains(main, `href="/app/models/running"`) {
		t.Error("the summary line is missing")
	}
}

// With nothing installed, the empty state holds the one way forward.
func TestLibraryEmpty(t *testing.T) {
	main := mainRe.FindString(get(shellUI(t, nil, ""), "/app/models").Body.String())
	if !strings.Contains(main, `class="empty-state"`) || !strings.Contains(main, `class="btn btn-primary mt-4">Get a model</a>`) {
		t.Errorf("empty: %s", main)
	}
	if strings.Contains(main, `aria-label="Filter by task"`) {
		t.Error("task chips shown with no models")
	}
}

func TestTaskFilter(t *testing.T) {
	for raw, want := range map[string]string{"": "all", "chat": "chat", "images": "images", "other": "other", "bogus": "all", "Chat": "all"} {
		r, _ := http.NewRequest("GET", "/app/models?filter="+url.QueryEscape(raw), nil)
		if got := taskFilter(r); got != want {
			t.Errorf("taskFilter(%q) = %q, want %q", raw, got, want)
		}
	}
	if !matchesTask("other", "Images") || matchesTask("other", "Chat") || !matchesTask("images", "Images") || matchesTask("chat", "Speech") {
		t.Error("matchesTask")
	}
	if got := presentTasks([]string{"Video", "Chat", "Chat"}); strings.Join(got, ",") != "Chat,Video" {
		t.Errorf("presentTasks: %v", got)
	}
}

// Get models opens on the qualified catalog: one action per row (Get, or
// Installed), fit in words with no numbers beyond download sizes, and task
// chips for the tasks it has. A search replaces it with search results.
func TestGetModelsCatalog(t *testing.T) {
	u := shellUI(t, nil, "")
	id := installSparse(t, u)
	main := mainRe.FindString(get(u, "/app/models/get").Body.String())
	if !strings.Contains(main, "From the LocalBase catalog") || !strings.Contains(main, `aria-label="Filter by task"`) {
		t.Fatalf("catalog: %s", main)
	}
	if !regexp.MustCompile(`id="cat-` + regexp.QuoteMeta(id) + `"[\s\S]*?<span class="pill pill-ok">Installed</span>`).MatchString(main) {
		t.Error("the installed model is not marked Installed")
	}
	gets := regexp.MustCompile(`hx-post="/app/models/get/recipe"`).FindAllString(main, -1)
	if len(gets) == 0 {
		t.Error("no Get actions")
	}
	if strings.Contains(main, "tokens/s") || strings.Contains(main, "downloads") {
		t.Error("performance or popularity numbers in the catalog")
	}
	// Only images.
	main = mainRe.FindString(get(u, "/app/models/get?filter=images").Body.String())
	if strings.Contains(main, "· Chat ·") || strings.Contains(main, ">Chat · ") || !strings.Contains(main, "Images · ") {
		t.Error("the images filter shows other tasks")
	}
}

// A Get from a catalog row answers with a word that fits the row.
func TestRecipeCompact(t *testing.T) {
	u, f := discoverUI(t, "models:read", "models:manage")
	f.reply["install-recipe"] = `{"modelId":"rec","displayName":"Recipe Model","operation":{"state":"running"}}`
	out := discoverPost(u, "/app/models/get/recipe", url.Values{"modelId": {"rec"}, "compact": {"1"}})
	if !strings.Contains(out, `<span class="pill pill-ok" role="status">Installing…</span>`) || strings.Contains(out, "Recipe Model") {
		t.Errorf("compact: %s", out)
	}
}
