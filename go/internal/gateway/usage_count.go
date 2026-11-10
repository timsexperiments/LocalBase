package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// Usage and activity (DESIGN §32): hourly totals, never content. Requests are counted
// here, at the end of ServeHTTP, so a request refused anywhere counts once, and a detached
// run counts as its inner request.

// usagePrincipal says who to count a request under. A browser identity is its opaque owner
// hash and nothing else: never an email or a name.
func (s *Server) usagePrincipal(p *auth.Principal) (kind, id string, ok bool) {
	if s.NoAuth {
		return "no-auth", "no-auth", true
	}
	if p == nil || !p.Authenticated() {
		return "", "", false
	}
	switch p.Kind {
	case "api-key":
		return "api-key", p.ID, true
	case "environment":
		return "environment", "environment", true
	case "browser-session":
		if p.ID == auth.LocalOwnerID {
			return "local", "local", true
		}
		if strings.HasPrefix(p.ID, "browser:") {
			return "browser", p.ID, true
		}
		sum := sha256.Sum256([]byte(p.ID))
		return "browser", "browser:" + hex.EncodeToString(sum[:]), true
	}
	return "", "", false
}

// knownUsageModel: only an id the gateway knows is stored, so a string a client typed can
// never land in the table.
func (s *Server) knownUsageModel(id string) bool {
	if id == "" {
		return false
	}
	if s.Catalog != nil && s.Catalog.ByID(id) != nil {
		return true
	}
	// Counted after the answer, so a failed read only leaves the model out of the row.
	t, err := s.importedTask(context.Background(), id)
	if err != nil {
		s.Log.Error("Could not read the imported models.", "event", "models.read-failed", "step", "reading the imported models", "error", err.Error())
	}
	return t != ""
}

// recordUsage counts a finished request. It never fails one.
func (s *Server) recordUsage(rec *httpx.Recorder, label string, status int, d time.Duration) {
	if s.Usage == nil {
		return
	}
	route, counted := usageRoutes()[label]
	if !counted {
		return
	}
	kind, id, ok := s.usagePrincipal(rec.Auth)
	if !ok {
		return
	}
	if route == "video" && rec.Status == http.StatusAccepted { // what was sent, whether or not it arrived
		return // the job counts once, when it ends
	}
	n := rec.Usage
	switch n.Outcome {
	case "error": // a stream that fails after it started has already sent 200
		status = n.Status
		if status < 400 {
			status = http.StatusBadGateway
		}
	case "cancelled":
		status = 499
	}
	model := n.Model
	if !n.Trusted && !s.knownUsageModel(model) {
		model = ""
	}
	s.Usage.Add(usage.Event{Kind: kind, Principal: id, Route: route, Model: model, Status: status, Duration: d, QueueWait: n.Queue,
		InputTokens: n.In, OutputTokens: n.Out, HasTokens: n.HasTokens})
}

// recordVideoJob counts a job when it ends: the only place a job is counted.
func (s *Server) recordVideoJob(p *auth.Principal, model string, status int, d time.Duration) {
	if s.Usage == nil {
		return
	}
	kind, id, ok := s.usagePrincipal(p)
	if !ok {
		return
	}
	s.Usage.Add(usage.Event{Kind: kind, Principal: id, Route: "video", Model: model, Status: status, Duration: d})
}
