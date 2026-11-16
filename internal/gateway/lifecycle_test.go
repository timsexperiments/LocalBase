package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// New starts the run janitor, and Close stops it: a gateway that is built and dropped leaves the
// janitor running until it is closed, and a closed one leaves nothing of its own running. Until
// 4d-3 added Close, this test pinned the leak itself. It is also the proof that testutil's leak
// check sees a real one.
func TestADroppedGatewayLeaksTheJanitorUntilClose(t *testing.T) {
	before := testutil.Snapshot()
	s := New(Config{})
	left := before.Leaked(100 * time.Millisecond)
	if len(left) != 1 || !strings.Contains(left[0], "runs.(*Manager).Janitor") {
		t.Fatalf("want the run janitor running, and nothing else; got %d:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("Close left %d running:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
	// Close again, and after StopRuns, as serve calls it: nothing more to stop, and no panic.
	s.StopRuns(0)
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("a second Close left %d running:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
}

// A gateway stopped the way serve stops it, StopRuns and then Close, leaves nothing running.
func TestStopRunsThenCloseLeavesNothing(t *testing.T) {
	before := testutil.Snapshot()
	s := New(Config{})
	s.StopRuns(time.Second)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("%d left running:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
}
