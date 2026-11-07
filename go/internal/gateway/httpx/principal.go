package httpx

import (
	"context"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

type PrincipalKey struct{}

// PrincipalFrom returns the request's resolved principal.
func PrincipalFrom(ctx context.Context) *auth.Principal {
	if p, ok := ctx.Value(PrincipalKey{}).(*auth.Principal); ok {
		return p
	}
	return auth.Anonymous
}

func SetAuth(r *http.Request, p *auth.Principal) {
	if rec, ok := r.Context().Value(RecKey{}).(*Recorder); ok {
		rec.Auth = p
		if p.Authenticated() {
			rec.Body.Authenticated()
		}
	}
}
