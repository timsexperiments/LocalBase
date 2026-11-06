package web

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A model's page, and the lists, say a read failed with a plain notice, not an error page.
func TestPagesReportAFailedPeakRead(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, s := range cat.All() {
		if s.Primary().SHA256 != "" {
			spec = s
			break
		}
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	models := filepath.Join(dir, "models", spec.Kind)
	if err := os.MkdirAll(models, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, a := range spec.Artifacts {
		f, err := os.Create(filepath.Join(models, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(a.ExpectedSizeBytes); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	cfg := &store.Config{Active: map[string]string{}, Selected: map[string][]string{}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	u := New(Deps{NoAuth: true, Catalog: cat, Store: st, RootDir: dir, LLM: llm})
	if w := get(u, "/app/models"); w.Code != 200 {
		t.Fatalf("before: %d %.200s", w.Code, w.Body.String())
	}
	if _, err := st.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	w := get(u, "/app/models")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Some memory figures couldn&#39;t be read") {
		t.Errorf("the list: %d %.300s", w.Code, w.Body.String())
	}
	if spec.Qualified() && !spec.ExperimentalRuntime {
		if _, _, missing := u.catalogRows("all"); !missing {
			t.Error("catalogRows doesn't say anything is missing")
		}
	}
	w = get(u, "/app/models/m/"+spec.ModelID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "so this is its estimate") {
		t.Errorf("details: %d %.300s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "no such table") {
		t.Errorf("shows the raw error")
	}
}
