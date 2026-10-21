package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// audited logs each successful change through an admin route (a Go
// addition): the action, who made it (actor kind and id) and what it changed
// (target kind and id). Ids only: never emails, names, secrets, key material
// or request bodies.
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
		var in struct {
			Action         string `json:"action"`
			KeyID          string `json:"keyId"`
			RegistrationID string `json:"registrationId"`
			Registration   struct {
				ID string `json:"id"`
			} `json:"registration"`
		}
		json.Unmarshal(body, &in)
		rec := &auditRecorder{recorder: recorder{ResponseWriter: w}}
		next(rec, r)
		if rec.status >= 300 || in.Action == "" || in.Action == "test-policy" || in.Action == "test-email-delivery" {
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
		kind, id := auditTarget(in.Action, in.KeyID, in.RegistrationID, in.Registration.ID, rec.body.Bytes())
		attrs := []any{"event", "audit." + area + "-changed", "action", in.Action, "actor_kind", "", "actor_id", "", "target_kind", kind, "target_id", id}
		if p != nil {
			attrs[5], attrs[7] = p.Kind, p.ID
		}
		s.Log.Info("Administration change.", attrs...)
	}
}

// auditTarget names what an admin change changed, by id.
func auditTarget(action, keyID, registrationID, upsertID string, response []byte) (string, string) {
	var out struct {
		User *struct {
			ID string `json:"id"`
		} `json:"user"`
		Key *struct {
			ID string `json:"id"`
		} `json:"key"`
		PolicyRevision *string `json:"policyRevision"`
	}
	json.Unmarshal(response, &out)
	switch {
	case out.Key != nil:
		return "api-key", out.Key.ID
	case keyID != "":
		return "api-key", keyID
	case out.User != nil:
		return "user", out.User.ID
	}
	switch action {
	case "apply-policy", "clear-policy":
		rev := ""
		if out.PolicyRevision != nil {
			rev = *out.PolicyRevision
		}
		return "policy", rev
	case "remove-registration":
		return "registration", registrationID
	case "upsert-oidc", "upsert-github", "upsert-magic-link":
		return "registration", upsertID
	case "configure-cloudflare", "disable":
		return "provider", "browser-access"
	case "configure-email-delivery", "disable-email-delivery":
		return "email-delivery", "smtp"
	}
	return "", ""
}

// auditRecorder keeps the start of the response to name the target.
type auditRecorder struct {
	recorder
	body bytes.Buffer
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if a.body.Len() < 64<<10 {
		a.body.Write(b)
	}
	return a.recorder.Write(b)
}
