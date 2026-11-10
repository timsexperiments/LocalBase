package httpx

import (
	"net/http"
)

// NotConfigured answers 501 for a feature this gateway runs without (route_disabled).
func NotConfigured(w http.ResponseWriter, r *http.Request, feature string) {
	Error(w, r, 501, "invalid_request_error", "route_disabled", feature+" route is disabled.", nil)
}
