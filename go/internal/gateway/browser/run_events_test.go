package browser

import "testing"

// A run's event stream is a run's id then /events, and a create path named alike isn't one.
func TestIsRunEvents(t *testing.T) {
	if isRunEvents("/_localbase/runs/v1/chat/completions") || !isRunEvents("/_localbase/runs/x/events") {
		t.Error("isRunEvents")
	}
}
