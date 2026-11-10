package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// New starts the run janitor, and only StopRuns stops it: a gateway that is built and dropped
// leaves it running. This pins that leak until 4d-3, which adds Close and rewrites this test to
// "Close leaves nothing running". It is also the proof that testutil's leak check sees a real one.
func TestADroppedGatewayLeaksTheJanitorUntilClose(t *testing.T) {
	before := testutil.Snapshot()
	s := New(Config{})
	left := before.Leaked(100 * time.Millisecond)
	if len(left) != 1 || !strings.Contains(left[0], "runs.(*Manager).Janitor") {
		t.Fatalf("want the run janitor still running, and nothing else; got %d:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
	s.StopRuns(0) // so it doesn't outlive this test
	if left := before.Leaked(time.Second); len(left) != 0 {
		t.Fatalf("StopRuns left %d running:\n\n%s", len(left), strings.Join(left, "\n\n"))
	}
}
