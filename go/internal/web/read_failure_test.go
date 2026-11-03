package web

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A model's page, and the lists, say a read failed rather than showing no peaks.
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
	if w := get(u, "/app/models"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "reading the memory peaks seen for "+spec.ModelID) {
		t.Errorf("the list: %d %.200s", w.Code, w.Body.String())
	}
	if spec.Qualified() && !spec.ExperimentalRuntime {
		if _, _, err := u.catalogRows("all"); err == nil {
			t.Error("catalogRows went on without the peaks")
		}
	}
	w := get(u, "/app/models/m/"+spec.ModelID)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "reading the memory peaks seen for "+spec.ModelID) || w.Header().Get("Retry-After") != "5" {
		t.Errorf("details: %d %.200s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "no such table") {
		t.Errorf("shows the raw error: %s", w.Body.String())
	}
}
