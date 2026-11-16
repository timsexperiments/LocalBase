package main

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// configure saves the configuration and its settings in one transaction: when a setting can't be
// written, nothing of the command is saved, not the configuration before it and not the settings
// written before it, and nothing is audited.
func TestAConfigureThatFailsPartwaySavesNothing(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	db := filepath.Join(root, "local-base.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The database refuses this one setting, as a full disk or a damaged page would.
	if _, err := st.DB.Exec(`CREATE TRIGGER refuse_private BEFORE INSERT ON go_settings WHEN NEW.key = '` +
		store.PrivateImportsKey + `' BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	audited := len(auditLines(t, root))

	ctx := before.CtxSize/2 + 1024
	code, out := runApp(t, "--root", root, "configure", "--no-create-key", "--ctx-size", strconv.Itoa(ctx),
		"--max-connections", "7", "--allow-private-imports")
	if code == 0 {
		t.Fatalf("configure succeeded with a setting refused: %s", out)
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	after, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.CtxSize != before.CtxSize {
		t.Errorf("the configuration was saved: ctx-size %d, was %d", after.CtxSize, before.CtxSize)
	}
	if v, ok, _ := st.Setting(context.Background(), store.MaxConnectionsKey); ok {
		t.Errorf("max-connections was written before the refused setting: %q", v)
	}
	if lines := auditLines(t, root)[audited:]; len(lines) != 0 {
		t.Errorf("a command that saved nothing was audited: %+v", lines)
	}
}
