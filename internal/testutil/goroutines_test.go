package testutil

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// parkedForTheTest closes started from inside itself, then blocks until stop closes; its name is
// what the checks look for.
func parkedForTheTest(started, stop chan struct{}) {
	close(started)
	<-stop
}

// park starts f as a goroutine and returns once f is running, so the goroutine's stack names f
// from then on: a check made after park can't see it before it has started. Closing the stop it
// returns ends the goroutine.
func park(f func(started, stop chan struct{})) (stop chan struct{}) {
	started, stop := make(chan struct{}), make(chan struct{})
	go f(started, stop)
	<-started
	return stop
}

func TestAGoroutineStillRunningIsLeaked(t *testing.T) {
	before := Snapshot()
	stop := park(parkedForTheTest)
	defer close(stop)
	left := before.Leaked(50 * time.Millisecond)
	if len(left) != 1 || !strings.Contains(left[0], "parkedForTheTest") {
		t.Fatalf("want the parked goroutine, got %d: %q", len(left), left)
	}
}

func TestAGoroutineThatEndedIsNot(t *testing.T) {
	before := Snapshot()
	stop := park(parkedForTheTest)
	close(stop)
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("an ended goroutine was counted: %q", left)
	}
}

// One that is stopping gets the wait to end in.
func TestAGoroutineThatEndsWithinTheWaitIsNot(t *testing.T) {
	before := Snapshot()
	stop := park(parkedForTheTest)
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	if left := before.Leaked(2 * time.Second); len(left) != 0 {
		t.Fatalf("a goroutine that ended within the wait was counted: %q", left)
	}
}

func TestAGoroutineTheCallerIgnoresIsNot(t *testing.T) {
	before := Snapshot()
	stop := park(parkedForTheTest)
	defer close(stop)
	if left := before.Leaked(50*time.Millisecond, "testutil.parkedForTheTest"); len(left) != 0 {
		t.Fatalf("an ignored goroutine was counted: %q", left)
	}
}

// Goroutines running before the snapshot aren't the test's.
func TestAGoroutineFromBeforeIsNot(t *testing.T) {
	stop := park(parkedForTheTest) // running before the snapshot is taken
	defer close(stop)
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
	stop := park(parkedForTheTest)
	defer close(stop)
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
	stop := park(parkedForTheTest)
	f.Cleanup(func() { close(stop) })
	f.end()
	if len(f.errors) != 0 {
		t.Fatalf("a goroutine a later Cleanup stopped was reported: %q", f.errors)
	}
}

// parkedElsewhere is parkedForTheTest under another name: code outside what LeakedIn looks within.
func parkedElsewhere(started, stop chan struct{}) {
	close(started)
	<-stop
}

func TestLeakedInCountsOnlyWhatRunsWithin(t *testing.T) {
	before := Snapshot()
	stop := park(parkedForTheTest)
	defer close(stop)
	elsewhere := park(parkedElsewhere)
	defer close(elsewhere)
	left := before.LeakedIn(50*time.Millisecond, "testutil.parkedForTheTest")
	if len(left) != 1 || !strings.Contains(left[0], "parkedForTheTest") {
		t.Fatalf("want only the goroutine within, got %d: %q", len(left), left)
	}
	if left := before.LeakedIn(50*time.Millisecond, "testutil.noSuchFunction"); len(left) != 0 {
		t.Fatalf("goroutines outside were counted: %q", left)
	}
}

func TestNoLeaksInReportsWhatRunsWithin(t *testing.T) {
	f := &cleanupT{}
	NoLeaksIn(f, "testutil.parkedForTheTest")
	stop := park(parkedForTheTest)
	defer close(stop)
	elsewhere := park(parkedElsewhere)
	defer close(elsewhere)
	f.end()
	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "1 goroutines started by the test are still running testutil.parkedForTheTest code") {
		t.Fatalf("want one report of the goroutine within, got %q", f.errors)
	}
}
