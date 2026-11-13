package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// An added definition is stored as an import; deleting an import takes its definition and its files.
func TestAddAndDeleteADefinition(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := &Library{Store: st}
	m := &models.Model{ID: "added.model", Name: "Added", Origin: "elsewhere"}
	if err := l.AddDefinition(ctx, m); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImportedModelByID(ctx, "added.model")
	if err != nil || row == nil {
		t.Fatalf("stored: %v, %v", row, err)
	}
	var stored models.Model
	if err := json.Unmarshal(row.Definition, &stored); err != nil || stored.Origin != "import" || stored.Name != "Added" {
		t.Errorf("stored as %+v (%v)", stored, err)
	}

	dir := filepath.Join(t.TempDir(), "added.model")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteImported(ctx, &Entry{Model: models.Model{ID: "added.model"}, Dir: dir, Imported: true}); err != nil {
		t.Fatal(err)
	}
	if row, err := st.ImportedModelByID(ctx, "added.model"); err != nil || row != nil {
		t.Errorf("the definition is still there: %v, %v", row, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("its files are still there: %v", err)
	}
}
