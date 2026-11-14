package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// What one import may cost is the owner's to set, and --help says so, including what a
// larger model needs.
func TestConfigureImportLimits(t *testing.T) {
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	help := e.cli(nil, "configure", "--help")
	text := help.stdout + help.stderr
	for _, want := range []string{"--import-deadline-hours", "default 48", "--import-max-unknown-gib", "default 256", "limited only by free disk"} {
		if !strings.Contains(text, want) {
			t.Errorf("configure --help lacks %q", want)
		}
	}
	setting := func(key string) string {
		t.Helper()
		st, err := store.Open(filepath.Join(e.root, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		v, _, err := st.Setting(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if r := e.cli(nil, "--non-interactive", "configure", "--defaults", "--no-create-key", "--import-deadline-hours", "12", "--import-max-unknown-gib", "512"); r.code != 0 {
		t.Fatalf("configure: %s %s", r.stdout, r.stderr)
	}
	if got := setting(store.ImportDeadlineKey); got != "12" {
		t.Errorf("deadline %q", got)
	}
	if got := setting(store.ImportMaxUnknownKey); got != "512" {
		t.Errorf("limit %q", got)
	}
	for _, bad := range [][]string{{"--import-deadline-hours", "0"}, {"--import-max-unknown-gib", "0"}, {"--import-deadline-hours", "soon"}} {
		args := append([]string{"--non-interactive", "configure", "--defaults", "--no-create-key"}, bad...)
		if r := e.cli(nil, args...); r.code == 0 {
			t.Errorf("%v was accepted", bad)
		}
	}
	if got := setting(store.ImportDeadlineKey); got != "12" {
		t.Errorf("a refused value changed the deadline: %q", got)
	}
}

// A crash can leave files under a model's directory that nothing records. Importing that
// model again through the CLI clears them, and a model that is recorded stays refused.
func TestCLIImportAfterACrash(t *testing.T) {
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	model := filepath.Join(tmp, "m.gguf")
	fsguard.WriteFile(model, ggufFile(t, "qwen3", nil, 4096), 0o600)
	leftBehind := filepath.Join(e.root, "models", "imported", "crashed", "left-behind")
	fsguard.MkdirAll(filepath.Dir(leftBehind), 0o700)
	fsguard.WriteFile(leftBehind, []byte("x"), 0o600)
	args := []string{"--non-interactive", "models", "import", model, "--id", "crashed", "--task", "chat", "--yes"}
	if r := e.cli(nil, args...); r.code != 0 {
		t.Fatalf("importing after a crash: %s %s", r.stdout, r.stderr)
	}
	if _, err := os.Stat(leftBehind); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("what the crash left is still there: %v", err)
	}
	r := e.cli(nil, args...)
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "already") {
		t.Errorf("importing a recorded model again: %d %s %s", r.code, r.stdout, r.stderr)
	}
}
