package httpx

import (
	"net/http"
)

// UpstreamFailure answers 502 upstream_error with message, for a backend that failed or answered
// what can't be relayed.
func UpstreamFailure(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, 502, "server_error", "upstream_error", message, nil)
}
