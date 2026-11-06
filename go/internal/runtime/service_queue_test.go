package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The finding: the request that started a load counted the load against the queue's deadline, so a
// load longer than that answered 429 while the backend went on starting. It isn't queueing: it is
// in the load.
func TestAMediaLoadLongerThanTheQueueTimeoutAnswersTheRequestThatStartedIt(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "900")
	s, _ := newTestService(t)
	s.d.QueueTimeout = 150 * time.Millisecond
	start := time.Now()
	l, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	defer l.Release()
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("took %s, so the load wasn't longer than the queue's deadline", took)
	}
}

// Others waiting are queueing, whatever they wait for, so the deadline is still theirs.
func TestOthersWaitingOnALoadStillTimeOut(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "1500")
	s, _ := newTestService(t)
	s.d.QueueTimeout = 150 * time.Millisecond
	first := make(chan error, 1)
	go func() {
		l, err := s.Acquire(context.Background(), "a")
		if err == nil {
			defer l.Release()
		}
		first <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		loading := s.switching
		s.mu.Unlock()
		if loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the load never began")
		}
	}
	start := time.Now()
	if _, err := s.Acquire(context.Background(), "a"); !errors.Is(err, ErrQueueTimeout) {
		t.Errorf("the one behind it: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the one behind it waited %s", took)
	}
	if err := <-first; err != nil {
		t.Errorf("the one that started the load: %v", err)
	}
}

// The start's own budget is what bounds it, and it answers with why the start failed, not a 429.
func TestAMediaLoadPastItsBudgetAnswersWithTheStartError(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "30000")
	s, _ := newTestService(t)
	s.d.QueueTimeout = 100 * time.Millisecond
	s.d.Startup = 700 * time.Millisecond
	start := time.Now()
	_, err := s.Acquire(context.Background(), "a")
	if !errors.Is(err, ErrStartupFailed) || errors.Is(err, ErrQueueTimeout) {
		t.Errorf("got %v, want the start's failure", err)
	}
	if took := time.Since(start); took < 700*time.Millisecond || took > 15*time.Second {
		t.Errorf("took %s, budget 700ms", took)
	}
}

// A start that fails at once answers with that too.
func TestAFailedMediaStartAfterTheDeadlineIsNotA429(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "500")
	failing := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(failing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FAIL_FILE", failing)
	s, _ := newTestService(t)
	s.d.QueueTimeout = 100 * time.Millisecond
	if _, err := s.Acquire(context.Background(), "a"); !errors.Is(err, ErrStartupFailed) {
		t.Errorf("got %v", err)
	}
}

// The person giving up still ends it for them, in or out of the load.
func TestACancelledRequestLeavesALoadAtOnce(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "2000")
	s, _ := newTestService(t)
	s.d.QueueTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.Acquire(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v", err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("took %s", took)
	}
}
