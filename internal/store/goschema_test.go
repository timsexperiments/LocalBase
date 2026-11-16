package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackendChoicesPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := st.BackendChoice(ctx, "m", "dev-a"); c != nil {
		t.Fatal("automatic mode is the absence of a row")
	}
	st.SetBackendChoice(ctx, "m", "dev-a", "llama.cpp-cpu", "")
	st.SetBackendChoice(ctx, "m", "dev-b", "llama.cpp-vulkan", "q4")
	st.Close()

	st, err = Open(path) // restart
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.BackendChoice(ctx, "m", "dev-a")
	b, _ := st.BackendChoice(ctx, "m", "dev-b")
	if a == nil || a.BackendID != "llama.cpp-cpu" || a.VariantID != "" || b == nil || b.VariantID != "q4" {
		t.Fatalf("choices after reopen: %+v %+v", a, b)
	}
	if ok, _ := st.ResetBackendChoice(ctx, "m", "dev-a"); !ok {
		t.Fatal("reset")
	}
	if c, _ := st.BackendChoice(ctx, "m", "dev-a"); c != nil {
		t.Fatal("reset did not return to automatic")
	}
	if c, _ := st.BackendChoice(ctx, "m", "dev-b"); c == nil {
		t.Fatal("reset touched another device")
	}
	// A newer extension schema is refused rather than misread.
	st.DB.Exec(`UPDATE go_schema SET version = 99`)
	st.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a database with a newer extension schema")
	}
}
