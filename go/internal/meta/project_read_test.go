package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestProjectReportsAFailedPeakRead(t *testing.T) {
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
	in := Input{Store: st, RootDir: dir, Catalog: cat, MemoryGB: 64, CLI: true, Config: &store.Config{Active: map[string]string{}, Selected: map[string][]string{}}}
	if _, err := Project(context.Background(), in, spec); err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := st.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	_, err = Project(context.Background(), in, spec)
	var failed *store.ReadError
	if !errors.As(err, &failed) {
		t.Errorf("%v", err)
	}
}
