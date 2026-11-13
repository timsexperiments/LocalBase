package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// `models add --id` takes the one model id rule (models.ValidID): lowercase letters, digits, '.',
// '_' and '-', starting with a letter or digit, at most 128 characters. Its own check had no length
// cap. A refused id is refused before the Hub is asked.
func TestModelsAddIDIsTheOneRule(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var asked atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hub.Close()
	t.Setenv("HF_ENDPOINT", hub.URL)
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const refused = "id: use lowercase letters, digits, '.', '_' and '-'"
	for _, id := range []string{strings.Repeat("a", 129), "Upper", "-lead", "has space"} {
		before := asked.Load()
		if code, out := runApp(t, "--root", root, "models", "add", "hf:org/repo-GGUF", "--id="+id); code != 2 || !strings.Contains(out, refused) || asked.Load() != before {
			t.Errorf("%.20q: exit %d, the hub asked %d times, %s", id, code, asked.Load()-before, out)
		}
	}
	// 128 characters pass the id check and go on to the Hub (here a 404).
	before := asked.Load()
	if _, out := runApp(t, "--root", root, "models", "add", "hf:org/repo-GGUF", "--id", strings.Repeat("a", 128)); strings.Contains(out, refused) || asked.Load() == before {
		t.Errorf("a 128-character id: the hub asked %d times, %s", asked.Load()-before, out)
	}
}
