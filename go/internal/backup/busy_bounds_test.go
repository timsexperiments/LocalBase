package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A busy database is waited for 5 s by one read, and 60 s by a step that keeps trying.
func TestTheBusyWaitsAreFiveAndSixtySeconds(t *testing.T) {
	if readBusyMillis != 5000 || busyWait != 60*time.Second {
		t.Fatalf("a read waits %d ms and a step %v, want 5000 ms and 60s", readBusyMillis, busyWait)
	}
}

// A database that stays busy is given up on at the waits a test sets, on each path that reads one, and not
// at the 60 s a step waits for itself or the 5 s of one read. The bounds are loose, for a loaded machine:
// within ten times the test's wait, and under a quarter of the 60 s. Without them, a test whose short waits
// stopped reaching a path would only take longer, and still pass.
func TestABusyDatabaseIsGivenUpOnAtItsWait(t *testing.T) {
	const wait = 200 * time.Millisecond
	within := func(t *testing.T, took time.Duration) {
		t.Helper()
		t.Logf("gave up after %v", took)
		if took > 10*wait || took > 60*time.Second/4 {
			t.Errorf("gave up after %v, want within %v", took, 10*wait)
		}
	}
	t.Run("a backup", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		holdLock(t, r.DB())
		shortBusyWaits(t, wait)
		start := time.Now()
		_, err := Create(context.Background(), Options{Root: r, Output: filepath.Join(dir(t), "x"+Extension), DryRun: true})
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "is busy") {
			t.Fatalf("backup: %v", err)
		}
		within(t, took)
	})
	t.Run("a restore's stage", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		res := create(t, r, false)
		holdLock(t, r.DB())
		shortBusyWaits(t, wait)
		start := time.Now()
		_, err := Stage(context.Background(), res.Output, r.Dir, true)
		took := time.Since(start)
		var refused *Refused
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "is busy") {
			t.Fatalf("stage: %v", err)
		}
		within(t, took)
	})
	t.Run("a restore's comparison", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		res := create(t, r, false)
		// A -shm is there, so the comparison reads the root's own file. A private copy can't turn busy.
		if err := readableDatabase(context.Background(), r.DB()); err != nil {
			t.Fatal(err)
		}
		shortBusyWaits(t, wait)
		start := time.Now()
		_, err := stage(context.Background(), res.Output, r.Dir, true, nil, options{targetRead: func() { holdLock(t, r.DB()) }})
		took := time.Since(start)
		var refused *Refused
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "is busy") {
			t.Fatalf("stage: %v", err)
		}
		within(t, took)
	})
	t.Run("a restore's hold", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		s := stageOver(t, r)
		shortBusyWaits(t, wait)
		release := holdLock(t, r.DB())
		defer release()
		start := time.Now()
		_, err := s.Commit()
		took := time.Since(start)
		var refused *Refused
		if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
			t.Fatalf("commit: %v", err)
		}
		within(t, took)
	})
	t.Run("a copy", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		holdLock(t, r.DB())
		shortBusyWaits(t, wait)
		stage, to := dir(t), filepath.Join(dir(t), "copy.db")
		start := time.Now()
		err := copyDatabase(context.Background(), r.DB(), to, stage, false)
		took := time.Since(start)
		if !isBusy(err) {
			t.Fatalf("copy: %v", err)
		}
		within(t, took)
	})
}
