package store

import (
	"path/filepath"
	"testing"
)

// The deployed Go binary refuses a database whose go_schema version is
// newer than its own, so adding tables must not raise the version
// (DESIGN §20.8). Raise it only together with a rollback plan.
func TestGoSchemaVersionForRollback(t *testing.T) {
	if goSchemaVersion != 1 {
		t.Fatalf("goSchemaVersion is %d: the deployed binary (version 1) would refuse this database; see DESIGN §20.8", goSchemaVersion)
	}
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	if err := st.DB.QueryRow(`SELECT version FROM go_schema WHERE id = 1`).Scan(&v); err != nil || v != 1 {
		t.Errorf("stored version %d %v", v, err)
	}
}

func TestPlacementStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if p, err := st.Placement(ctx, "m", "dev"); p != nil || err != nil {
		t.Fatalf("empty: %v %v", p, err)
	}
	four := 4
	if err := st.SetPlacement(ctx, "m", "dev", Placement{Mode: "custom", CPUMoELayers: &four}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Reopening applies the schema again (idempotent) and keeps the row.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.Placement(ctx, "m", "dev")
	if err != nil || p == nil || p.Mode != "custom" || p.CPUMoELayers == nil || *p.CPUMoELayers != 4 || p.GPULayers != nil {
		t.Fatalf("read back %+v %v", p, err)
	}
	if other, _ := st.Placement(ctx, "m", "other-device"); other != nil {
		t.Error("a placement leaked to another device")
	}
	if ok, err := st.ResetPlacement(ctx, "m", "dev"); !ok || err != nil {
		t.Errorf("reset %v %v", ok, err)
	}
	if p, _ := st.Placement(ctx, "m", "dev"); p != nil {
		t.Error("reset kept the row")
	}
}
