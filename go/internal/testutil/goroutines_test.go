package testutil

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// parkedForTheTest blocks until stop closes; its name is what the checks look for.
func parkedForTheTest(stop chan struct{}) { <-stop }

func TestAGoroutineStillRunningIsLeaked(t *testing.T) {
	before := Snapshot()
	stop := make(chan struct{})
	defer close(stop)
	go parkedForTheTest(stop)
	left := before.Leaked(50 * time.Millisecond)
	if len(left) != 1 || !strings.Contains(left[0], "parkedForTheTest") {
		t.Fatalf("want the parked goroutine, got %d: %q", len(left), left)
	}
}

func TestAGoroutineThatEndedIsNot(t *testing.T) {
	before := Snapshot()
	stop := make(chan struct{})
	go parkedForTheTest(stop)
	close(stop)
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("an ended goroutine was counted: %q", left)
	}
}

// One that is stopping gets the wait to end in.
func TestAGoroutineThatEndsWithinTheWaitIsNot(t *testing.T) {
	before := Snapshot()
	stop := make(chan struct{})
	go parkedForTheTest(stop)
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	if left := before.Leaked(2 * time.Second); len(left) != 0 {
		t.Fatalf("a goroutine that ended within the wait was counted: %q", left)
	}
}

func TestAGoroutineTheCallerIgnoresIsNot(t *testing.T) {
	before := Snapshot()
	stop := make(chan struct{})
	defer close(stop)
	go parkedForTheTest(stop)
	if left := before.Leaked(50*time.Millisecond, "testutil.parkedForTheTest"); len(left) != 0 {
		t.Fatalf("an ignored goroutine was counted: %q", left)
	}
}

// Goroutines running before the snapshot aren't the test's.
func TestAGoroutineFromBeforeIsNot(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	go parkedForTheTest(stop)
	time.Sleep(10 * time.Millisecond)
	if left := Snapshot().Leaked(50 * time.Millisecond); len(left) != 0 {
		t.Fatalf("a goroutine from before the snapshot was counted: %q", left)
	}
}

// cleanupT records what NoLeaks reports, and runs its Cleanups when the test would.
type cleanupT struct {
	cleanups []func()
	errors   []string
}

func (f *cleanupT) Helper()           {}
func (f *cleanupT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *cleanupT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *cleanupT) end() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func TestNoLeaksReportsWhatTheTestLeftRunning(t *testing.T) {
	f := &cleanupT{}
	NoLeaks(f)
	stop := make(chan struct{})
	defer close(stop)
	go parkedForTheTest(stop)
	f.end()
	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "1 goroutines started by the test are still running") ||
		!strings.Contains(f.errors[0], "parkedForTheTest") {
		t.Fatalf("want one report naming the goroutine, got %q", f.errors)
	}
}

// Cleanups registered after NoLeaks run before its check, so what they stop isn't a leak.
func TestNoLeaksChecksAfterTheOtherCleanups(t *testing.T) {
	f := &cleanupT{}
	NoLeaks(f)
	stop := make(chan struct{})
	go parkedForTheTest(stop)
	f.Cleanup(func() { close(stop) })
	f.end()
	if len(f.errors) != 0 {
		t.Fatalf("a goroutine a later Cleanup stopped was reported: %q", f.errors)
	}
}
