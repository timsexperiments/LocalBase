package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// What a refused request can put in the log. It is whatever the caller sent, so only
// what can't hold anything else gets through: an action word and a key id.
var (
	auditActionRe = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)
	auditKeyIDRe  = regexp.MustCompile(`^key_[0-9a-f-]{36}$`)
)

// audited logs each change through an admin route, and each one refused: the action,
// who made it (actor kind and id), what it changed (target kind and id) and, for a
// key, the permissions it now has; for a refusal, the status and the reason. Ids and
// permission names only: never emails, names, secrets, key material or request bodies.
func (s *Server) audited(area string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		// Only what the handler itself reads, once it has authenticated the caller.
		var seen bytes.Buffer
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, &limitedWriter{&seen, maxManagementBody + 1}), r.Body}
		rec := &auditRecorder{recorder: recorder{ResponseWriter: w}}
		next(rec, r)
		// A request the client abandoned or never finished was not refused.
		refused := rec.status >= 400 && rec.status < 500 && rec.status != http.StatusRequestTimeout && rec.status != 499
		if rec.status >= 300 && !refused {
			return
		}
		var in struct {
			Action         string `json:"action"`
			KeyID          string `json:"keyId"`
			RegistrationID string `json:"registrationId"`
			Registration   struct {
				ID string `json:"id"`
			} `json:"registration"`
		}
		json.Unmarshal(seen.Bytes(), &in)
		if !refused && (in.Action == "" || in.Action == "test-policy" || in.Action == "test-email-delivery") {
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
		actorKind, actorID := "", ""
		if p != nil {
			actorKind, actorID = p.Kind, p.ID
		}
		if refused && (actorKind == "" || actorKind == "anonymous") {
			s.anonAdminRefused.Add(1)
			return // anyone can send these, so they're counted, not audited
		}
		if refused {
			now := time.Now()
			if !s.refusedAudit.Allow(now) {
				return
			}
			action := in.Action
			if !auditActionRe.MatchString(action) {
				action = ""
			}
			if !auditKeyIDRe.MatchString(id) {
				id = ""
			}
			s.Log.Warn("Administration change refused.", "event", "audit."+area+"-refused", "action", action, "actor_kind", actorKind, "actor_id", actorID,
				"target_kind", kind, "target_id", id, "status", rec.status, "reason", auditReason(rec.body.Bytes()),
				"held_back", s.refusedAudit.Flush(now))
			return
		}
		attrs := []any{"event", "audit." + area + "-changed", "action", in.Action, "actor_kind", actorKind, "actor_id", actorID, "target_kind", kind, "target_id", id}
		if scopes, ok := auditScopes(rec.body.Bytes()); ok {
			attrs = append(attrs, "scopes", scopes)
		}
		s.Log.Info("Administration change.", attrs...)
	}
}

// auditReason is the error code of a refusal: this gateway's own words.
func auditReason(response []byte) string {
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(response, &out)
	return out.Error.Code
}

// auditScopes are the permissions a key has after a change. They come from this
// gateway's own response, which has already validated them.
func auditScopes(response []byte) (string, bool) {
	var out struct {
		Key *struct {
			Scopes []string `json:"scopes"`
		} `json:"key"`
	}
	if json.Unmarshal(response, &out) != nil || out.Key == nil {
		return "", false
	}
	return strings.Join(out.Key.Scopes, ","), true
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

// limitedWriter keeps the first n bytes written to it and accepts the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if keep := min(l.n, len(b)); keep > 0 {
		_, _ = l.w.Write(b[:keep]) // a bounded copy for the audit record; the response goes on regardless
		l.n -= keep
	}
	return len(b), nil
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
