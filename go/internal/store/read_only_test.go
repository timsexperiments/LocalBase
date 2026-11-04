package store

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// rootFiles says what's in a directory, to the byte.
func rootFiles(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %x", e.Name(), sha256.Sum256(b)))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// makeReadOnly makes a directory and its files read-only, as a mount or a person might.
func makeReadOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("modes don't bind root")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(dir, e.Name()), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// aRoot makes a stopped root holding a known setting, in the mode asked for.
func aRoot(t *testing.T, mode string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetSetting(ctx, "known", "stored"); err != nil {
		t.Fatal(err)
	}
	if mode == "delete" {
		if _, err := st.DB.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// aCrashedRoot copies a root while a write is only in its -wal, as a kill leaves it, with or without
// its -shm.
func aCrashedRoot(t *testing.T, withSHM bool) (dir, path string) {
	t.Helper()
	live := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "known", "only in the wal"); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	path = filepath.Join(dir, "local-base.db")
	suffixes := []string{"", "-wal"}
	if withSHM {
		suffixes = append(suffixes, "-shm")
	}
	for _, suffix := range suffixes {
		b, err := os.ReadFile(live + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, path
}

func wantSetting(t *testing.T, st *Store, want string) {
	t.Helper()
	got, ok, err := st.Setting(context.Background(), "known")
	if err != nil || !ok || got != want {
		t.Errorf("read %q, %v, %v, want %q", got, ok, err, want)
	}
}

// A root that can't be written still reads, by either way of opening, whichever mode it's in, and
// nothing about it changes.
func TestAReadOnlyRootStillReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T) (string, string)
		want string
	}{
		{"stopped, rollback", func(t *testing.T) (string, string) { return aRoot(t, "delete") }, "stored"},
		{"stopped, WAL", func(t *testing.T) (string, string) { return aRoot(t, "wal") }, "stored"},
		{"killed, -wal and -shm", func(t *testing.T) (string, string) { return aCrashedRoot(t, true) }, "only in the wal"},
		{"killed, -wal alone", func(t *testing.T) (string, string) { return aCrashedRoot(t, false) }, "only in the wal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := tc.make(t)
			makeReadOnly(t, dir)
			before := rootFiles(t, dir)
			for name, open := range map[string]func(string) (*Store, error){"Open": Open, "OpenReadOnly": OpenReadOnly} {
				st, err := open(path)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				wantSetting(t, st, tc.want)
				if err := st.Close(); err != nil {
					t.Errorf("%s: closing: %v", name, err)
				}
				if after := rootFiles(t, dir); after != before {
					t.Errorf("%s changed the root:\n%s\nwas\n%s", name, after, before)
				}
			}
		})
	}
}

// A read-only open leaves a stopped root as it found it, sees a write only in the -wal, such as a key
// revoked a moment before, and takes its private copy away with it.
func TestReadingAStoppedRootLeavesNoTrace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for name, make := range map[string]func(t *testing.T) (string, string, string){
		"clean": func(t *testing.T) (string, string, string) { d, p := aRoot(t, "wal"); return d, p, "stored" },
		"a write only in -wal": func(t *testing.T) (string, string, string) {
			d, p := aCrashedRoot(t, false)
			return d, p, "only in the wal"
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, path, want := make(t)
			before := rootFiles(t, dir)
			st, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			wantSetting(t, st, want)
			if st.stage == "" {
				t.Error("no private copy, so reading this root would leave sidecars")
			}
			stage := st.stage
			if err := st.Close(); err != nil {
				t.Error(err)
			}
			if after := rootFiles(t, dir); after != before {
				t.Errorf("the root changed:\n%s\nwas\n%s", after, before)
			}
			if _, err := os.Stat(stage); !os.IsNotExist(err) {
				t.Errorf("the private copy stayed: %v", err)
			}
		})
	}
}

// A -wal with commits in it isn't replayed into a database it didn't come from, however that database
// got there. Whatever inode the new file gets, this has to hold: the token is what's compared.
func TestAWALFromAnotherDatabaseIsRefused(t *testing.T) {
	foreignWAL := func(t *testing.T) []byte {
		t.Helper()
		_, crashed := aCrashedRoot(t, false)
		wal, err := os.ReadFile(crashed + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return wal
	}
	other := func(t *testing.T) []byte { // another database, with a token of its own
		t.Helper()
		_, p := aRoot(t, "wal")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, path string)
	}{
		{"deleted and written again", func(t *testing.T, path string) {
			body := other(t)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"overwritten in place", func(t *testing.T, path string) {
			if err := os.WriteFile(path, other(t), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced, keeping the old file", func(t *testing.T, path string) {
			body := other(t)
			if err := os.Rename(path, filepath.Join(t.TempDir(), "kept.db")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"the same file, with the -wal and owner file of another", func(t *testing.T, path string) {
			_, from := aCrashedRootWithOwner(t)
			owner, err := os.ReadFile(from + ownerSuffix)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+ownerSuffix, owner, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"nothing else: the same database, stopped cleanly, and a -wal put beside it", func(*testing.T, string) {}},
		{"its own recorded -wal replaced, after a crash", func(t *testing.T, path string) {
			token, err := fileToken(path)
			if err != nil {
				t.Fatal(err)
			}
			writeOwner(path, ownerRecord{token: token, wal: "1:1"}) // not the file about to be put there
		}},
		{"a database from before tokens", func(t *testing.T, path string) {
			body := other(t)
			copy(body[tokenOffset:tokenOffset+4], []byte{0, 0, 0, 0})
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path := aRoot(t, "wal")
			tc.replace(t, path)
			if err := os.WriteFile(path+"-wal", foreignWAL(t), 0o600); err != nil {
				t.Fatal(err)
			}
			before := rootFiles(t, dir)
			for name, open := range map[string]func(string) (*Store, error){"Open": Open, "OpenReadOnly": OpenReadOnly} {
				st, err := open(path)
				if err == nil {
					st.Close()
					t.Fatalf("%s opened it", name)
				}
				for _, want := range []string{path + "-wal", "move it somewhere safe", path + ownerSuffix} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: %q lacks %q", name, err, want)
					}
				}
			}
			if after := rootFiles(t, dir); after != before {
				t.Errorf("a refusal changed the root:\n%s\nwas\n%s", after, before)
			}
			// The way out it gives.
			if err := os.Rename(path+"-wal", filepath.Join(t.TempDir(), "kept-wal")); err != nil {
				t.Fatal(err)
			}
			st, err := Open(path)
			if err != nil {
				t.Fatalf("with it moved aside: %v", err)
			}
			st.Close()
		})
	}
}

// aCrashedRootWithOwner is aCrashedRoot, carrying the owner file a real root would have.
func aCrashedRootWithOwner(t *testing.T) (dir, path string) {
	t.Helper()
	dir, path = aCrashedRoot(t, false)
	token, err := fileToken(path)
	if err != nil || token == 0 {
		t.Fatalf("no token: %d %v", token, err)
	}
	id, err := fileID(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	writeOwner(path, ownerRecord{token: token, wal: id})
	return dir, path
}

// The token is in the file's own header, once, and stays there.
func TestADatabaseGetsOneToken(t *testing.T) {
	_, path := aRoot(t, "wal")
	first, err := fileToken(path)
	if err != nil || first == 0 {
		t.Fatalf("%d %v", first, err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if again, err := fileToken(path); err != nil || again != first {
		t.Errorf("it changed: %d, was %d: %v", again, first, err)
	}
	owner, err := readOwner(path)
	if err != nil || owner.token != first || owner.wal != ownerCleanStop {
		t.Errorf("owner file %+v, token %d: %v", owner, first, err)
	}
	_, otherPath := aRoot(t, "wal")
	if other, _ := fileToken(otherPath); other == first {
		t.Errorf("two databases share a token: %d", first)
	}
}

// A database from before tokens gets one at its next writable open, and one that arrives with a token
// and no owner file, as a restored one does, keeps it and gets an owner file.
func TestADatabaseWithoutARecordGetsOne(t *testing.T) {
	t.Run("from before tokens", func(t *testing.T) {
		_, path := aRoot(t, "delete")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copy(b[tokenOffset:tokenOffset+4], []byte{0, 0, 0, 0})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path + ownerSuffix); err != nil {
			t.Fatal(err)
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		wantSetting(t, st, "stored")
		st.Close()
		if token, err := fileToken(path); err != nil || token == 0 {
			t.Errorf("%d %v", token, err)
		}
		if _, err := os.Stat(path + ownerSuffix); err != nil {
			t.Error(err)
		}
	})
	t.Run("restored, with a token and no owner file", func(t *testing.T) {
		_, path := aRoot(t, "wal")
		token, err := fileToken(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path + ownerSuffix); err != nil {
			t.Fatal(err)
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		if after, _ := fileToken(path); after != token {
			t.Errorf("the token changed: %d, was %d", after, token)
		}
		if got, err := readOwner(path); err != nil || got.token != token {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("replaced cleanly: no -wal, so nothing to judge, and the record follows", func(t *testing.T) {
		_, path := aRoot(t, "wal")
		_, from := aRoot(t, "wal")
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		token, _ := fileToken(path)
		if got, err := readOwner(path); err != nil || got.token != token {
			t.Errorf("%+v, token %d: %v", got, token, err)
		}
	})
}

// What must keep working: a whole root copied after a clean stop, and one moved after a crash.
func TestALegitimateRootStillOpens(t *testing.T) {
	copyDir := func(t *testing.T, from string) string {
		t.Helper()
		to := t.TempDir()
		entries, err := os.ReadDir(from)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(from, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return to
	}
	t.Run("copied after a clean stop", func(t *testing.T) {
		dir, path := aRoot(t, "wal")
		st, err := Open(filepath.Join(copyDir(t, dir), filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		wantSetting(t, st, "stored")
		st.Close()
	})
	t.Run("moved after a crash", func(t *testing.T) {
		dir, path := aCrashedRootWithOwner(t)
		to := filepath.Join(t.TempDir(), "moved")
		if err := os.Rename(dir, to); err != nil {
			t.Fatal(err)
		}
		st, err := Open(filepath.Join(to, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		wantSetting(t, st, "only in the wal")
		st.Close()
	})
	t.Run("copied after a crash: refused, because a copied -wal can't be told from a foreign one, and the way out works", func(t *testing.T) {
		dir, path := aCrashedRootWithOwner(t)
		copied := filepath.Join(copyDir(t, dir), filepath.Base(path))
		if st, err := Open(copied); err == nil {
			st.Close()
			t.Fatal("it opened")
		}
		if err := os.Remove(copied + ownerSuffix); err != nil {
			t.Fatal(err)
		}
		st, err := Open(copied)
		if err != nil {
			t.Fatal(err)
		}
		wantSetting(t, st, "only in the wal")
		st.Close()
	})
}

// A copy of files someone may be writing can be torn, so with another process holding the root there's
// no copy. That process is this test binary run again, holding each kind of hold until it's told to stop.
func TestAHeldRootIsNotCopied(t *testing.T) {
	if dir := os.Getenv("LOCALBASE_TEST_HOLD_ROOT"); dir != "" {
		r, err := root.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		var release func()
		switch os.Getenv("LOCALBASE_TEST_HOLD_KIND") {
		case "gateway":
			if release, err = r.Lock("127.0.0.1", 1); err != nil {
				t.Fatal(err)
			}
		default:
			lease, err := root.AcquireOperation(r.Dir, "restore")
			if err != nil {
				t.Fatal(err)
			}
			release = lease.Release
		}
		defer release()
		fmt.Println("HOLDING")
		_, _ = io.Copy(io.Discard, os.Stdin) // until the parent closes it
		return
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	path := r.DB()
	made, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := made.Close(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"gateway", "lease"} {
		t.Run(kind, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAHeldRootIsNotCopied$")
			cmd.Env = append(os.Environ(), "LOCALBASE_TEST_HOLD_ROOT="+r.Dir, "LOCALBASE_TEST_HOLD_KIND="+kind)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := false
			finish := func() {
				if !done {
					done = true
					_ = stdin.Close()
					_ = cmd.Wait()
				}
			}
			defer finish()
			if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || !strings.Contains(line, "HOLDING") {
				t.Fatalf("the other process: %q %v", line, err)
			}
			if st, err := OpenReadOnly(path); !errors.Is(err, ErrInUse) {
				if st != nil {
					st.Close()
				}
				t.Errorf("held: %v", err)
			}
			finish()
			st, err := OpenReadOnly(path)
			if err != nil {
				t.Fatalf("released: %v", err)
			}
			st.Close()
		})
	}
}

// A read of a copy says so, so nobody takes it for a live one.
func TestReadingACopySaysSo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var said strings.Builder
	defer func(w io.Writer) { readNotice = w }(readNotice)
	readNotice = &said
	_, path := aRoot(t, "wal")
	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if said.String() != "Read a copy of this root's database, as it stood just now.\n" {
		t.Errorf("%q", said.String())
	}
	said.Reset()
	_, rollback := aRoot(t, "delete")
	st, err = OpenReadOnly(rollback)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if said.Len() != 0 {
		t.Errorf("no copy was read: %q", said.String())
	}
}

// Copies a crash left go, once old, if their process has gone. Anything else stays.
func TestLeftCopiesAreSwept(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * stageMaxAge)
	dead := 1 << 22 // past any pid here
	make := func(name string, at time.Time) string {
		p := filepath.Join(parent, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := make(fmt.Sprintf("%s%d-a", stagePrefix, dead), old)
	recent := make(fmt.Sprintf("%s%d-b", stagePrefix, dead), now)
	alive := make(fmt.Sprintf("%s%d-c", stagePrefix, os.Getpid()), old)
	other := make("something-else", old)
	sweepStages(parent, now)
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("an old copy with no process stayed: %v", err)
	}
	for _, p := range []string{recent, alive, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s went: %v", p, err)
		}
	}
}

// A command reading with its own lease isn't racing itself: it reads the copy.
func TestAProcessHoldingItsOwnLeaseCanRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	made, err := Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := made.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := root.AcquireOperation(r.Dir, "backup")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	st, err := OpenReadOnly(r.DB())
	if err != nil {
		t.Fatalf("with its own lease: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Error(err)
	}
}

// The token reaches the main file while the store is open, not only when it closes: a crash could come
// before then. And an open that finds commits in a -wal claims nothing: it can't know whose they are.
func TestATokenIsInTheFileOnceOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token, err := fileToken(path)
	if err != nil || token == 0 {
		t.Fatalf("%d %v", token, err)
	}
	if got, err := readOwner(path); err != nil || got.token != token || got.wal == "-" || got.wal == ownerCleanStop {
		t.Errorf("%+v %v", got, err)
	}
	// Once only, even asked again.
	if err := st.setToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, err := fileToken(path); err != nil || again != token {
		t.Errorf("it changed: %d, was %d: %v", again, token, err)
	}
}

func TestAnOpenWithCommitsInTheWALClaimsNothing(t *testing.T) {
	_, path := aCrashedRoot(t, false)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(path + ownerSuffix); !os.IsNotExist(err) {
		t.Errorf("it claimed a -wal it can't know: %v", err)
	}
	wantSetting(t, st, "only in the wal")
}

// One store closing while another has the database open isn't a clean stop: the -wal is still in use,
// and a crash of the one left must still find it its own.
func TestAnotherProcessClosingIsNotACleanStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	one, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := two.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readOwner(path)
	if err != nil || got.wal == ownerCleanStop || got.wal == "-" {
		t.Errorf("%+v %v", got, err)
	}
}

// A different database put under a crashed root's own -wal: the -wal is the recorded one, and only the
// token can tell.
func TestADifferentDatabaseUnderItsOwnWALIsRefused(t *testing.T) {
	_, path := aCrashedRootWithOwner(t)
	_, other := aRoot(t, "wal")
	b, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(path); err == nil {
		st.Close()
		t.Error("it opened")
	}
}
