package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/placement"
)

// A retry cannot help a model that cannot fit, so nothing may tell a client to retry.
func TestPlacementTooLargeIsNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		retry  bool
	}{
		{"model_too_large", 422, false},
		{"placement_does_not_fit", 503, true},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(""))
		RuntimeError(w, r, &placement.Error{Code: tc.code, Message: "m"}, "x", "LLM")
		if w.Code != tc.status || (w.Header().Get("Retry-After") != "") != tc.retry || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s: %d retry=%q %s", tc.code, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
}
