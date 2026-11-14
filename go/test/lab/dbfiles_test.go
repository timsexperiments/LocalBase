package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Counted for real, on this process, on each platform: lsof on macOS, /proc on Linux.
func TestDatabaseFilesAreCountedByKind(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "local-base.db")
	open := func(name string, n int) {
		t.Helper()
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for range n {
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
		}
	}
	open(db, 3)
	open(db+"-wal", 2)
	open(db+"-shm", 1)
	open(db+"-journal", 4)
	open(db+"-other", 5) // near the name, and not one of them
	open(filepath.Join(dir, "elsewhere.db"), 5)
	got, err := countDatabaseFiles(os.Getpid(), db)
	if err != nil {
		t.Fatal(err)
	}
	if want := (DatabaseFiles{DB: 3, WAL: 2, SHM: 1, Journal: 4}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.Total() != 10 {
		t.Errorf("total %d", got.Total())
	}
}

func TestDescriptorProblemHasTwoParts(t *testing.T) {
	stats := func(fds, db, wal, shm int) Stats {
		return Stats{FDs: fds, Files: DatabaseFiles{DB: db, WAL: wal, SHM: shm}}
	}
	first := stats(15, 1, 1, 1)
	for _, c := range []struct {
		name string
		last Stats
		says string
	}{
		{"nothing changed", stats(15, 1, 1, 1), ""},
		{"the pool filled, and nothing else moved", stats(33, 16, 4, 1), ""},
		{"within the slack, with the pool full", stats(37, 16, 4, 1), ""},
		{"one past the slack, with the pool full", stats(38, 16, 4, 1), "other than the database's went from 12 to 17"},
		{"one past the slack, with the pool as it was", stats(20, 1, 1, 1), "other than the database's went from 12 to 17"},
		{"past the ceiling, and nothing else moved", stats(34, 17, 4, 1), "22 descriptors on the database's files"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := DescriptorProblem(first, c.last, 4, 21)
			if (c.says == "") != (got == "") || !strings.Contains(got, c.says) {
				t.Errorf("got %q, want %q", got, c.says)
			}
		})
	}
}

func TestDescriptorSummarySaysWhichPartMoved(t *testing.T) {
	first := Stats{FDs: 18, Files: DatabaseFiles{DB: 1, WAL: 1, SHM: 1}}
	last := Stats{FDs: 25, Files: DatabaseFiles{DB: 4, WAL: 4, SHM: 1}}
	want := "descriptors 18 to 25: other than the database's 15 to 16; on the database's files 3 to 9 (db 1 to 4, -wal 1 to 4, -shm 1 to 1, -journal 0 to 0)"
	if got := DescriptorSummary(first, last); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
