package gateway

import (
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// ownerOf is the run owner for a request's principal.
func (s *Server) runOwner(r *http.Request) string {
	if s.cfg.NoAuth {
		return "local"
	}
	return httpx.PrincipalFrom(r.Context()).OwnerID()
}
