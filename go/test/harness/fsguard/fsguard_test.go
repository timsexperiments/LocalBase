package fsguard

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
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

	// Every exported writer refuses outside a scratch root: the plain-named writes with a panic
	// carrying ErrGuard, the others with an error. TestFSGuardCoversEveryWriter keeps this whole.
	refusedPanic := func(label string, write func()) {
		t.Helper()
		defer func() {
			if err, _ := recover().(error); !errors.Is(err, ErrGuard) {
				t.Errorf("%s: no guard panic (%v)", label, err)
			}
		}()
		write()
	}
	refusedPanic("WriteFile outside", func() { _ = WriteFile(filepath.Join(outside, "w"), []byte("x"), 0o600) })
	refusedPanic("Create outside", func() { _, _ = Create(filepath.Join(outside, "c")) })
	refusedPanic("Append outside", func() { _, _ = Append(victim) })
	refusedPanic("OpenRW outside", func() { _, _ = OpenRW(victim) })
	refusedPanic("MkdirAll outside", func() { _ = MkdirAll(filepath.Join(outside, "m"), 0o700) })
	refusedPanic("Chmod outside", func() { _ = Chmod(victim, 0o777) })
	refusedPanic("Symlink outside", func() { _ = Symlink(victim, filepath.Join(outside, "l")) })
	refusedPanic("RemoveLink outside", func() { _ = RemoveLink(model) })
	refusedPanic("RemoveAll outside", func() { _ = RemoveAll(victim) })
	refusedPanic("Grow outside", func() { _ = Grow(victim, 1<<20) })
	refused("CopyDir outside", CopyDir(root, filepath.Join(outside, "copy")))
	t.Setenv(UpdateGoldenEnv, "")
	refused("WriteGolden without LOCALBASE_UPDATE_GOLDEN", WriteGolden("x.json", []byte("{}")))
	t.Setenv(UpdateGoldenEnv, "1")
	refused("WriteGolden out of testdata/golden", WriteGolden("../../escape.json", []byte("{}")))

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

// nonWriters are fsguard's exported functions that write nothing of a test's own: they register
// a scratch root or check one. Every other exported function must go through checkWrite.
var nonWriters = map[string]bool{"ScratchDir": true, "ScratchTemp": true, "AssertNoLinks": true}

// TestFSGuardCoversEveryWriter keeps TestFSGuard whole. It reads this package's non-test files:
// every exported function that reaches checkWrite (a writer) must be called inside one of
// TestFSGuard's refusal cases (refused or refusedPanic), and every other exported function must be
// a named non-writer. A writer added later without a case, or one that skips checkWrite, fails.
func TestFSGuardCoversEveryWriter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := map[string]map[string]bool{} // function -> the package functions it calls
	var exported []string
	var guardTest *ast.FuncDecl
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if strings.HasSuffix(name, "_test.go") {
				if fn.Name.Name == "TestFSGuard" {
					guardTest = fn
				}
				continue
			}
			called := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						called[id.Name] = true
					}
				}
				return true
			})
			calls[fn.Name.Name] = called
			if fn.Name.IsExported() {
				exported = append(exported, fn.Name.Name)
			}
		}
	}
	if guardTest == nil {
		t.Fatal("no TestFSGuard in this package's tests")
	}
	reachesCheck := func(fn string) bool {
		seen, todo := map[string]bool{}, []string{fn}
		for len(todo) > 0 {
			f := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if seen[f] {
				continue
			}
			seen[f] = true
			for g := range calls[f] {
				if g == "checkWrite" {
					return true
				}
				todo = append(todo, g)
			}
		}
		return false
	}
	covered := map[string]bool{}
	ast.Inspect(guardTest.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); !ok || (id.Name != "refused" && id.Name != "refusedPanic") {
			return true
		}
		for _, arg := range c.Args {
			ast.Inspect(arg, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						covered[id.Name] = true
					}
				}
				return true
			})
		}
		return true
	})
	var writers []string
	for _, fn := range exported {
		switch {
		case reachesCheck(fn):
			writers = append(writers, fn)
			if !covered[fn] {
				t.Errorf("%s writes through checkWrite, but no refusal case in TestFSGuard calls it", fn)
			}
		case !nonWriters[fn]:
			t.Errorf("%s is exported, doesn't reach checkWrite and isn't a named non-writer", fn)
		}
	}
	if len(writers) == 0 {
		t.Fatal("found no writers: the parse read nothing")
	}
	t.Logf("%d writers: %s", len(writers), strings.Join(writers, ", "))
}
