package store

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const lockProbeEnv = "LOCALBASE_TEST_LOCK_PROBE"

// exclusiveFromAnotherProcess asks a real second process for what a restore asks for. A lock in this
// process can only be seen from another: within a process SQLite answers from its own records.
func exclusiveFromAnotherProcess(t *testing.T, path string) string {
	t.Helper()
	return inAnotherProcess(t, path, "")
}

func inAnotherProcess(t *testing.T, path, do string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAProcessKeepsItsLockOnTheDatabase$")
	cmd.Env = append(os.Environ(), lockProbeEnv+"="+path, lockProbeEnv+"_DO="+do)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	result := ""
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(scanner.Text(), "RESULT "); ok {
			result = rest
		}
	}
	_ = cmd.Wait()
	return result
}

// A store attached to the database stays visibly attached to other processes, whatever else this process
// reads. Without that, a restore's exclusive lock succeeds under a running gateway. The locks differ per
// OS, so this runs on each: nothing here is macOS-only.
func TestAProcessKeepsItsLockOnTheDatabase(t *testing.T) {
	if path := os.Getenv(lockProbeEnv); path != "" {
		switch os.Getenv(lockProbeEnv + "_DO") {
		case "open and close":
			st, err := Open(path)
			if err != nil {
				fmt.Println("RESULT open:", err)
				return
			}
			fmt.Println("RESULT close:", st.Close())
			return
		case "read only and close":
			st, err := OpenReadOnly(path)
			if err != nil {
				fmt.Println("RESULT open:", err)
				return
			}
			fmt.Println("RESULT close:", st.Close())
			return
		case "read the setting":
			st, err := Open(path)
			if err != nil {
				fmt.Println("RESULT open:", err)
				return
			}
			defer st.Close()
			value, ok, err := st.Setting(context.Background(), "after")
			fmt.Println("RESULT setting:", value, ok, err)
			return
		}
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
		if err != nil {
			fmt.Println("RESULT open:", err)
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
			fmt.Println("RESULT pragma:", err)
			return
		}
		if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
			fmt.Println("RESULT refused:", err)
			return
		}
		fmt.Println("RESULT granted")
		return
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	refused := func(when, path string) {
		t.Helper()
		if got := exclusiveFromAnotherProcess(t, path); !strings.HasPrefix(got, "refused:") {
			t.Errorf("%s: another process's exclusive lock: %q", when, got)
		}
	}
	for name, reopened := range map[string]bool{"a new database": false, "a root with a token already": true} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local-base.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if reopened {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				if st, err = Open(path); err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			refused("after Open", path)
			if err := st.SetSetting(context.Background(), "x", "1"); err != nil {
				t.Fatal(err)
			}
			refused("after a write", path)
			read, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := read.Close(); err != nil {
				t.Fatal(err)
			}
			refused("after an OpenReadOnly and Close", path)
			second, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			refused("after a second Open and Close", path)
			if _, err := fileToken(path); err != nil {
				t.Fatal(err)
			}
			if !inWAL(path) {
				t.Error("not in write-ahead logging")
			}
			if err := checkWALBelongs(path); err != nil {
				t.Fatal(err)
			}
			refused("after raw reads", path)
		})
	}
	// The control: with nothing attached, it's granted, so "refused" above means something.
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := exclusiveFromAnotherProcess(t, path); got != "granted" {
		t.Errorf("with nothing attached: %q", got)
	}
}

// A command opening and closing beside a running gateway mustn't take the gateway's -wal with it: if it
// thought it was the last connection, it would delete the file the gateway is still committing to, and
// those commits would be lost with nothing reporting it. This is the everyday shape: no restore involved.
func TestACommandClosingBesideAStoreLeavesItsWrites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, do := range []string{"open and close", "read only and close"} {
		t.Run(do, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local-base.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if err := st.SetSetting(ctx, "before", "1"); err != nil {
				t.Fatal(err)
			}
			walBefore, err := fileID(path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			if got := inAnotherProcess(t, path, do); got != "close: <nil>" {
				t.Fatalf("the other command: %q", got)
			}
			walAfter, err := fileID(path + "-wal")
			if err != nil || walAfter != walBefore {
				t.Errorf("the -wal went from under a live store: %v, %q then %q", err, walBefore, walAfter)
			}
			if err := st.SetSetting(ctx, "after", "kept"); err != nil {
				t.Fatal(err)
			}
			if got := inAnotherProcess(t, path, "read the setting"); got != "setting: kept true <nil>" {
				t.Errorf("a third process reads the write as %q", got)
			}
		})
	}
}

// This tests SQLite, not this code. The fix keeps A's lock, so nothing else here sees what happens when a
// lock is lost: a guard in SQLite (unixIsSharingShmNode) still keeps an ordinary close from deleting A's
// -wal, and older versions don't have it. If this fails, a change of modernc.org/sqlite has taken that
// guard away, and a lost lock can again lose commits in ordinary use, so look at what's changed before
// taking that version. It isn't a version check, because what matters is how it behaves.
func TestSQLitesGuardKeepsAWALWhenALockIsLost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	loseLock := func(path string) {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close() // the bug's shape: this close is what ends the lock
	}
	open := func(path string) *Store {
		t.Helper()
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	// The lock really is gone. This is on a file of its own, because this check can delete a -wal.
	probe := filepath.Join(t.TempDir(), "local-base.db")
	held := open(probe)
	defer held.Close()
	loseLock(probe)
	if got := exclusiveFromAnotherProcess(t, probe); got != "granted" {
		t.Fatalf("a raw close should end the lock: %q", got)
	}

	for _, do := range []string{"open and close", "read only and close"} {
		t.Run(do, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local-base.db")
			st := open(path)
			defer st.Close()
			ctx := context.Background()
			if err := st.SetSetting(ctx, "before", "1"); err != nil {
				t.Fatal(err)
			}
			loseLock(path)
			wal, err := fileID(path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			if got := inAnotherProcess(t, path, do); got != "close: <nil>" {
				t.Fatalf("the other command: %q", got)
			}
			if after, err := fileID(path + "-wal"); err != nil || after != wal {
				t.Errorf("an ordinary close deleted a -wal in use: %v, %q then %q", err, wal, after)
			}
			if err := st.SetSetting(ctx, "after", "kept"); err != nil {
				t.Fatal(err)
			}
			if got := inAnotherProcess(t, path, "read the setting"); got != "setting: kept true <nil>" {
				t.Errorf("a third process reads the write as %q", got)
			}
		})
	}
}

// Descriptors are let go with the last store, and a file put at the same path gets its own.
func TestHeldFilesFollowTheFileAndAreLetGo(t *testing.T) {
	held := func() int {
		heldFilesMu.Lock()
		defer heldFilesMu.Unlock()
		return len(heldFiles)
	}
	before := held()
	path := filepath.Join(t.TempDir(), "local-base.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if held() != before+1 {
		t.Errorf("two stores on one file hold %d", held()-before)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if held() != before+1 {
		t.Errorf("after one Close, %d", held()-before)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if held() != before {
		t.Errorf("after the last Close, %d stay", held()-before)
	}

	moved := path + ".aside"
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.held == b.held || held() != before+2 {
		t.Errorf("a new file at the path shares a descriptor: %d held", held()-before)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if held() != before+1 {
		t.Errorf("after closing the new file's store, %d", held()-before)
	}

	bad := filepath.Join(t.TempDir(), "local-base.db")
	if err := os.WriteFile(bad, []byte("not a database, long enough to be read as one and refused"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := held()
	if st, err := Open(bad); err == nil {
		st.Close()
		t.Error("it opened")
	}
	if held() != now {
		t.Errorf("a failed Open kept %d", held()-now)
	}
}

// opensAllowed names every function in this package that opens a file directly, and why it isn't
// a database, or is the one place that may. A new one fails until someone has read this comment.
var opensAllowed = map[string]string{
	"held_file.go:holdFile":    "the shared descriptor",
	"held_file.go:fileToRead":  "the shared descriptor's fallback, with no store holding the file",
	"store.go:makePrivate":     "O_EXCL: it opens only a file that didn't exist",
	"wal_files.go:readOwner":   "the owner file",
	"wal_files.go:writeOwner":  "the owner file",
	"wal_files.go:copyPrivate": "the copy being written, in a private folder",
}

// One close of a descriptor on the database ends this process's locks, so raw opens are read for here.
// Every other package's opens are read for anything naming the database. That's by name, so an open of a
// path that reached the database under another name isn't seen; for the gateway, the walk from serve in
// cmd/localbase covers the one package known to do that.
func TestOnlyThisFileOpensADatabase(t *testing.T) {
	seen := map[string]bool{}
	opens := func(n ast.Node) []*ast.CallExpr {
		var calls []*ast.CallExpr
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "os" && (sel.Sel.Name == "Open" || sel.Sel.Name == "OpenFile" || sel.Sel.Name == "ReadFile") {
				calls = append(calls, call)
			}
			return true
		})
		return calls
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if len(opens(fn)) > 0 {
				where := file + ":" + fn.Name.Name
				seen[where] = true
				if opensAllowed[where] == "" {
					t.Errorf("%s opens a file directly. If it can be a database, use readFile; otherwise name it in opensAllowed", where)
				}
			}
		}
	}
	for where := range opensAllowed {
		if !seen[where] {
			t.Errorf("opensAllowed names %s, which no longer opens a file", where)
		}
	}
	// Every package, since the gateway links most of them; internal/backup's reading of the files it
	// hashes is kept out of the gateway by TestServeNeverReachesInternalBackup in cmd/localbase.
	dirs, err := filepath.Glob("../*")
	if err != nil {
		t.Fatal(err)
	}
	dirs = append(dirs, "../../cmd/localbase")
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || dir == "../store" {
			continue
		}
		found, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range found {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range opens(parsed) {
				for _, arg := range call.Args {
					src := ""
					ast.Inspect(arg, func(n ast.Node) bool {
						switch x := n.(type) {
						case *ast.Ident:
							src += x.Name + " "
						case *ast.BasicLit:
							src += x.Value + " "
						}
						return true
					})
					if strings.Contains(src, "dbPath") || strings.Contains(src, "DB ") || strings.Contains(src, "local-base.db") {
						t.Errorf("%s: opens what looks like the database (%s). Closing that ends this process's locks on it", fset.Position(call.Pos()), src)
					}
				}
			}
		}
	}
}
