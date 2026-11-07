package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A configure refused for one bad option writes nothing: the configuration and every setting stay
// as they were (review 01-M1). Before, the configuration was saved and audited first, and the
// setting options were checked after.
func TestARefusedConfigureWritesNothing(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	read := func() (int, string, bool) {
		t.Helper()
		st, err := store.Open(filepath.Join(root, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		cfg, err := st.LoadConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		v, ok, err := st.Setting(context.Background(), store.PrivateImportsKey)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Port, v, ok
	}
	port, private, had := read()
	for _, bad := range [][]string{{"--import-deadline-hours", "0"}, {"--max-connections", "abc"}, {"--import-max-unknown-gib", "0"}} {
		args := append([]string{"--root", root, "configure", "--no-create-key", "--port", "12345", "--allow-private-imports"}, bad...)
		if code, out := runApp(t, args...); code == 0 {
			t.Fatalf("%v was accepted: %s", bad, out)
		}
		if p, v, ok := read(); p != port || v != private || ok != had {
			t.Errorf("%v refused, yet port %d (was %d) and private imports %q %v (was %q %v)", bad, p, port, v, ok, private, had)
		}
	}
	// The same options without the bad one are all written.
	if code, out := runApp(t, "--root", root, "configure", "--no-create-key", "--port", "12345", "--allow-private-imports"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	if p, v, ok := read(); p != 12345 || v != "on" || !ok {
		t.Errorf("accepted, yet port %d and private imports %q %v", p, v, ok)
	}
}
