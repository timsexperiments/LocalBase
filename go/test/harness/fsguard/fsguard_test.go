package fsguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- tests --------------------------------------------------------------------------------

// TestFSGuard tries every forbidden write and shows the guard refuses it.
func TestFSGuard(t *testing.T) {
	root := ScratchDir(t)
	outside := t.TempDir() // never registered
	victim := filepath.Join(outside, "victim.bin")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, ErrGuard) {
			t.Errorf("%s: not refused (%v)", label, err)
		}
	}

	// Outside every scratch root.
	refused("write outside", guardedWriteFile(filepath.Join(outside, "x"), []byte("x"), 0o600))
	refused("create outside", func() error { _, err := guardedCreate(filepath.Join(outside, "y")); return err }())
	refused("sparse outside", Sparse(filepath.Join(outside, "z"), 10))
	refused("mkdir outside", guardedMkdirAll(filepath.Join(outside, "d"), 0o700))
	refused("remove outside", guardedRemoveAll(victim))
	refused("dot-dot escape", guardedWriteFile(filepath.Join(root, "..", filepath.Base(outside), "victim.bin"), []byte("x"), 0o600))
	// A directory symlink inside the scratch root that points outside it.
	link := filepath.Join(root, "models")
	os.Symlink(outside, link)
	refused("through a directory symlink", guardedWriteFile(filepath.Join(link, "victim.bin"), []byte("x"), 0o600))
	refused("new file under a directory symlink", Sparse(filepath.Join(link, "new.bin"), 10))

	// A symlink to a file, even one inside the root.
	inner := filepath.Join(root, "inner.bin")
	os.WriteFile(inner, []byte("inner"), 0o600)
	fileLink := filepath.Join(root, "file-link.bin")
	os.Symlink(victim, fileLink)
	refused("write through a file symlink", guardedWriteFile(fileLink, []byte("x"), 0o600))
	refused("create through a file symlink", func() error { _, err := guardedCreate(fileLink); return err }())
	refused("append through a file symlink", func() error { _, err := guardedAppend(fileLink); return err }())
	innerLink := filepath.Join(root, "inner-link.bin")
	os.Symlink(inner, innerLink)
	refused("symlink to a file in the root", guardedWriteFile(innerLink, []byte("x"), 0o600))

	// A hard link: an existing file with more than one link.
	hard := filepath.Join(root, "hard.bin")
	if err := os.Link(inner, hard); err != nil {
		t.Fatal(err)
	}
	refused("write to a hard link", guardedWriteFile(hard, []byte("x"), 0o600))
	refused("write to the other name of a hard link", guardedWriteFile(inner, []byte("x"), 0o600))
	refused("append to a hard link", func() error { _, err := guardedAppend(hard); return err }())

	// The live root.
	if live := liveRoot(); live != "" {
		refused("write into the live root", checkWrite(filepath.Join(live, "models", "fsguard-probe")))
		if _, err := registerScratch(filepath.Join(live, "models")); err == nil {
			t.Error("the live root was accepted as a scratch root")
		}
	}

	// Clones only: a model already linked in, or an existing destination,
	// is refused; a clone or small copy is detached and read-only.
	refused("clone over an existing entry", CloneModel(victim, fileLink))
	model := filepath.Join(outside, "model.gguf")
	os.WriteFile(model, []byte("weights"), 0o600)
	dst := filepath.Join(root, "model.gguf")
	if err := CloneModel(model, dst); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o400 {
		t.Errorf("clone is %v, want read-only", fi.Mode().Perm())
	}
	refused("write to a read-only clone's link check", assertDetached(model, hard))
	refused("a symlink presented as a clone", assertDetached(victim, fileLink))

	// Nothing outside changed.
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("the victim file changed: %q", b)
	}
	if b, _ := os.ReadFile(model); string(b) != "weights" {
		t.Fatalf("the source model changed: %q", b)
	}
	// Allowed writes still work.
	if err := guardedWriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Errorf("an allowed write was refused: %v", err)
	}
}
