package httpx

import (
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// CredentialLookupFailed answers for a request whose credential couldn't be looked up.
func CredentialLookupFailed(w http.ResponseWriter, r *http.Request) bool {
	if !auth.LookupFailed(r.Context()) {
		return false
	}
	ServiceUnavailable(w, r, "Authentication")
	return true
}
