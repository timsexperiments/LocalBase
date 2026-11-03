package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
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

// A -wal with commits in it beside a different file isn't replayed: it would damage that file.
func TestAWALFromAnotherFileIsRefused(t *testing.T) {
	_, crashed := aCrashedRoot(t, false)
	wal, err := os.ReadFile(crashed + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	dir, path := aRoot(t, "wal")
	// Replaced by a copy, as a restore or a hand does: another file with the same name.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
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
	wantSetting(t, st, "stored")
	st.Close()
}

// What must keep working: a whole root copied after a clean stop, and a root moved after a crash,
// whose -wal is still its own.
func TestALegitimateRootStillOpens(t *testing.T) {
	t.Run("copied after a clean stop", func(t *testing.T) {
		dir, path := aRoot(t, "wal")
		to := t.TempDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		st, err := Open(filepath.Join(to, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		wantSetting(t, st, "stored")
		st.Close()
	})
	t.Run("moved after a crash", func(t *testing.T) {
		// Its files as a kill leaves them, under a recorded owner.
		dir, path := aCrashedRoot(t, false)
		recordWALOwner(path)
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
	t.Run("copied after a crash: refused, and the way out works", func(t *testing.T) {
		dir, path := aCrashedRoot(t, false)
		recordWALOwner(path)
		to := t.TempDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		copied := filepath.Join(to, filepath.Base(path))
		if st, err := Open(copied); err == nil {
			st.Close()
			t.Fatal("it opened: a copied file can't be told from a replaced one")
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

// A copy of files someone may be writing can be torn, so with the root held there's no copy.
func TestAHeldRootIsNotCopied(t *testing.T) {
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
	before, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("nobody holds it: %v", err)
	}
	before.Close()
	t.Run("a gateway", func(t *testing.T) {
		release, err := r.Lock("127.0.0.1", 1)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if st, err := OpenReadOnly(path); !errors.Is(err, ErrInUse) {
			if st != nil {
				st.Close()
			}
			t.Errorf("%v", err)
		}
	})
	t.Run("a command with the lease", func(t *testing.T) {
		lease, err := root.AcquireOperation(r.Dir, "restore")
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if st, err := OpenReadOnly(path); !errors.Is(err, ErrInUse) {
			if st != nil {
				st.Close()
			}
			t.Errorf("%v", err)
		}
	})
	if after, err := OpenReadOnly(path); err != nil {
		t.Errorf("released: %v", err)
	} else {
		after.Close()
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
