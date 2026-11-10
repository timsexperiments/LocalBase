package gateway

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/video"
)

// A finished job counts in usage under the status of how it ended. TestUsageVideoJobCountsOnce
// counts one over HTTP.
func TestAVideoJobCountsByHowItEnded(t *testing.T) {
	for state, want := range map[video.State]int{video.Completed: 200, video.Failed: 500, video.Cancelled: 499} {
		if got := videoJobStatus(state); got != want {
			t.Errorf("%s: %d, want %d", state, got, want)
		}
	}
}
