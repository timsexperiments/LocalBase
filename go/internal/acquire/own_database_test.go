package acquire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// Everything here is a valid model file, so the only thing that can refuse it is what it is.
func TestAPartOfTheRootsDatabaseIsNotImported(t *testing.T) {
	ctx := context.Background()
	plant := func(t *testing.T, path string) {
		t.Helper()
		if err := testutil.GGUF(path, map[string]any{"general.architecture": "qwen3"}, 1024); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(root, source string) error {
		_, err := Resolve(ctx, root, Request{Source: source, AllowLocal: true, Task: "chat", ID: "x"})
		return err
	}
	// The whole line, and name is the file's name alone: a refusal never shows a folder.
	refused := func(t *testing.T, root, source, name string) {
		t.Helper()
		err := resolve(root, source)
		if err == nil || err.Error() != name+" is part of LocalBase's own database, so it can't be imported." {
			t.Errorf("%s: %v", source, err)
		}
		if strings.ContainsRune(name, filepath.Separator) {
			t.Errorf("the test expects a folder in the name: %s", name)
		}
	}
	// A root with its database in place, and a folder that isn't the root.
	setup := func(t *testing.T) (root, db, elsewhere string) {
		t.Helper()
		root, elsewhere = t.TempDir(), t.TempDir()
		db = filepath.Join(root, "local-base.db")
		plant(t, db)
		return root, db, elsewhere
	}

	for _, suffix := range append([]string{""}, store.Sidecars...) {
		t.Run("local-base.db"+suffix, func(t *testing.T) {
			root, db, _ := setup(t)
			if suffix != "" {
				plant(t, db+suffix)
			}
			refused(t, root, db+suffix, "local-base.db"+suffix)
		})
	}
	t.Run("a link to it", func(t *testing.T) {
		root, db, elsewhere := setup(t)
		link := filepath.Join(elsewhere, "m.gguf")
		if err := os.Symlink(db, link); err != nil {
			t.Fatal(err)
		}
		refused(t, root, link, "m.gguf")
	})
	t.Run("another name for it", func(t *testing.T) {
		root, db, elsewhere := setup(t)
		link := filepath.Join(elsewhere, "m.gguf")
		if err := os.Link(db, link); err != nil {
			t.Fatal(err)
		}
		refused(t, root, link, "m.gguf")
	})
	t.Run("a relative path", func(t *testing.T) {
		root, _, _ := setup(t)
		t.Chdir(root)
		refused(t, root, "./local-base.db", "local-base.db")
	})
	for _, c := range []struct{ name, asked, linked string }{
		{"the part after", "big-00001-of-00002.gguf", "big-00002-of-00002.gguf"},
		{"the part before", "big-00002-of-00002.gguf", "big-00001-of-00002.gguf"},
	} {
		t.Run("a split set with it as "+c.name, func(t *testing.T) {
			root, db, elsewhere := setup(t)
			plant(t, filepath.Join(elsewhere, c.asked))
			if err := os.Symlink(db, filepath.Join(elsewhere, c.linked)); err != nil {
				t.Fatal(err)
			}
			refused(t, root, filepath.Join(elsewhere, c.asked), c.linked)
		})
	}
	// Not the root's: the same name somewhere else, and a copy of its bytes.
	t.Run("the same name elsewhere", func(t *testing.T) {
		root, _, elsewhere := setup(t)
		same := filepath.Join(elsewhere, "local-base.db")
		plant(t, same)
		if err := resolve(root, same); err != nil {
			t.Errorf("refused a file that isn't this root's: %v", err)
		}
	})
	t.Run("another root's database", func(t *testing.T) {
		root, _, _ := setup(t)
		_, other, _ := setup(t)
		if err := resolve(root, other); err != nil {
			t.Errorf("refused a file that isn't this root's: %v", err)
		}
	})
}
