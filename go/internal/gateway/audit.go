package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// audited logs each successful change through an admin route (a Go
// addition): the action, who made it (kind and id) and, for keys, the key
// id. Never emails, names, secrets or request bodies.
func (s *Server) audited(area string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxManagementBody+1))
		if err != nil {
			body = nil
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var head struct {
			Action string `json:"action"`
			KeyID  string `json:"keyId"`
		}
		json.Unmarshal(body, &head)
		rec := &recorder{ResponseWriter: w}
		next(rec, r)
		if rec.status >= 300 || head.Action == "" || head.Action == "test-policy" || head.Action == "test-email-delivery" {
			return
		}
		// The principal the handler resolved (never resolved twice).
		var p *auth.Principal
		if bp, ok := r.Context().Value(browserPrincipalKey{}).(*auth.Principal); ok {
			p = bp
		} else if outer, ok := r.Context().Value(recKey{}).(*recorder); ok {
			p = outer.auth
		}
		if p == nil && s.NoAuth {
			p = &auth.Principal{Kind: "environment"}
		}
		attrs := []any{"event", "audit." + area + "-changed", "action", head.Action, "actor_kind", "", "actor_id", ""}
		if p != nil {
			attrs[5], attrs[7] = p.Kind, p.ID
		}
		if head.KeyID != "" {
			attrs = append(attrs, "key_id", head.KeyID)
		}
		s.Log.Info("Administration change.", attrs...)
	}
}
