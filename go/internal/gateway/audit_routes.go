package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

const (
	changeAuditEvery = time.Minute
	changeAuditBurst = 20
)

// What a change's target can be in the log. A refused request's is whatever the caller sent,
// so it also has to name something this gateway knows.
var auditTargetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// auditedChange is what a request changes, in ids only.
type auditedChange struct{ action, targetKind, targetID string }

// changeDescriber names a request's change from its path and these bounded copies of its body and
// response, or says it changes nothing. It reads named fields only, never anything else in the body.
type changeDescriber func(r *http.Request, body, response []byte) (auditedChange, bool)

// auditedRoute logs each change through a route, up to changeAuditBurst a minute across all
// such routes, and counts the rest by area, and each one refused as audited does. The line is
// written when the handler returns: a long job's line says it was accepted, not how it ended.
func (s *Server) auditedRoute(area string, describe changeDescriber, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			next(w, r)
			return
		}
		var seen bytes.Buffer
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, &limitedWriter{&seen, httpx.MaxManagementBody + 1}), r.Body}
		rec := &auditRecorder{Recorder: httpx.Recorder{ResponseWriter: w}}
		next(rec, r)
		// A method the route doesn't have asked to change nothing; an abandoned request wasn't refused.
		refused := rec.Status >= 400 && rec.Status < 500 && rec.Status != http.StatusMethodNotAllowed &&
			rec.Status != http.StatusRequestTimeout && rec.Status != 499
		if rec.Status >= 300 && !refused {
			return
		}
		change, ok := describe(r, seen.Bytes(), rec.body.Bytes())
		if !ok {
			return
		}
		if !auditTargetRe.MatchString(change.targetID) {
			change.targetID = ""
		}
		if !auditActionRe.MatchString(change.action) {
			change.action = ""
		}
		actorKind, actorID := s.auditActor(r)
		if refused {
			if change.targetKind != "model" || s.cfg.Catalog == nil || s.cfg.Catalog.ByID(change.targetID) == nil {
				change.targetID = ""
			}
			s.auditRefused(area, change.action, actorKind, actorID, change.targetKind, change.targetID, rec.Status, rec.body.Bytes())
			return
		}
		now := time.Now()
		if !s.changeAudit.allow(now, area, auditActorLabel(actorKind, actorID)) {
			return
		}
		s.changeSummary(s.changeAudit.flush(now))
		s.cfg.Log.Info("Administration change.", "event", "audit."+area+"-changed", "action", change.action, "actor_kind", actorKind, "actor_id", actorID,
			"target_kind", change.targetKind, "target_id", change.targetID)
	}
}

// changeSummary says what was counted rather than logged, and whose. Nothing is dropped unmentioned.
func (s *Server) changeSummary(held heldChanges) {
	if held.count > 0 {
		s.cfg.Log.Warn("Changes were counted rather than logged.", "event", "audit.changes-summary", "count", held.count, "areas", held.areas,
			"actors", held.actors, "other_actors_count", held.others, "interval_seconds", int(changeAuditEvery.Seconds()))
	}
}

// auditActorLabel is how a summary names who made a change: the principal's id, or its kind when
// it has none, as for the command line. Anything of another shape is counted among the others.
func auditActorLabel(kind, id string) string {
	if id == "" {
		id = kind
	}
	if !auditTargetRe.MatchString(id) {
		return ""
	}
	return id
}

// heldChanges is what a summary says.
type heldChanges struct {
	count, others int
	areas, actors string
}

// changeAuditActors is how many actors a summary names. A change by anyone after those is in
// other_actors_count, a count of changes and not of people.
const changeAuditActors = 5

// changeLimiter is one budget for every limited area, with a count of what was held back in each
// and by whom. Its size can't depend on a request: its area keys are the names written in New, a
// target id is never one, and it names at most changeAuditActors actors between summaries.
type changeLimiter struct {
	sampler *obs.Sampler
	mu      sync.Mutex
	held    map[string]int
	actors  map[string]int
	others  int
}

func newChangeLimiter() *changeLimiter {
	return &changeLimiter{sampler: &obs.Sampler{Every: changeAuditEvery, Burst: changeAuditBurst}, held: map[string]int{}, actors: map[string]int{}}
}

func (l *changeLimiter) allow(now time.Time, area, actor string) bool {
	if l.sampler.Allow(now) {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[area]++
	if _, known := l.actors[actor]; actor != "" && (known || len(l.actors) < changeAuditActors) {
		l.actors[actor]++
	} else {
		l.others++
	}
	return false
}

// flush returns what has been held back, once an interval has ended.
func (l *changeLimiter) flush(now time.Time) heldChanges {
	if l.sampler.Flush(now) == 0 {
		return heldChanges{}
	}
	return l.take()
}

// drain returns everything held back, the open interval's too. It's for the end.
func (l *changeLimiter) drain(now time.Time) heldChanges {
	l.sampler.Drain(now)
	return l.take()
}

func (l *changeLimiter) take() heldChanges {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := heldChanges{others: l.others}
	list := func(m map[string]int) string {
		parts := make([]string, 0, len(m))
		for name, n := range m {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		}
		slices.Sort(parts)
		return strings.Join(parts, ",")
	}
	for _, n := range l.held {
		out.count += n
	}
	out.areas, out.actors = list(l.held), list(l.actors)
	clear(l.held)
	clear(l.actors)
	l.others = 0
	return out
}

// auditedModelRoute sends a model's unload and placement changes to their own areas.
func (s *Server) auditedModelRoute(next http.HandlerFunc) http.HandlerFunc {
	unload := s.auditedRoute("unload", func(r *http.Request, _, _ []byte) (auditedChange, bool) {
		return auditedChange{"unload", "model", modelIDFromPath(r, "/unload")}, r.Method == http.MethodPost
	}, next)
	placement := s.auditedRoute("placement", func(r *http.Request, _, _ []byte) (auditedChange, bool) {
		action := "set"
		if r.Method == http.MethodDelete {
			action = "clear"
		}
		return auditedChange{action, "model", modelIDFromPath(r, "/placement")}, r.Method == http.MethodPut || r.Method == http.MethodDelete
	}, next)
	return func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.EscapedPath(); {
		case strings.HasSuffix(path, "/unload"):
			unload(w, r)
		case strings.HasSuffix(path, "/placement"):
			placement(w, r)
		default:
			next(w, r)
		}
	}
}

func modelIDFromPath(r *http.Request, suffix string) string {
	raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), suffix)
	id, err := url.PathUnescape(raw)
	if err != nil {
		return ""
	}
	return id
}

func modelsChange(_ *http.Request, body, _ []byte) (auditedChange, bool) {
	var in struct {
		ModelID string `json:"modelId"`
		Action  string `json:"action"`
	}
	_ = json.Unmarshal(body, &in) // a body that doesn't parse is a refusal with no target
	return auditedChange{in.Action, "model", in.ModelID}, true
}

func defaultModelsChange(_ *http.Request, body, _ []byte) (auditedChange, bool) {
	var in struct {
		Models []string `json:"models"`
	}
	_ = json.Unmarshal(body, &in) // as modelsChange
	first := ""
	if len(in.Models) > 0 {
		first = in.Models[0]
	}
	return auditedChange{"set", "model", first}, true
}

func importsChange(_ *http.Request, body, response []byte) (auditedChange, bool) {
	var in struct {
		Action string `json:"action"`
		Job    string `json:"job"`
	}
	_ = json.Unmarshal(body, &in) // as modelsChange
	if in.Action == "resolve" {
		return auditedChange{}, false // only looks
	}
	if in.Action == "start" {
		var out struct {
			Job string `json:"job"`
		}
		_ = json.Unmarshal(response, &out) // a refusal has no job
		in.Job = out.Job
	}
	return auditedChange{in.Action, "import-job", in.Job}, true
}

func discoveryChange(_ *http.Request, body, response []byte) (auditedChange, bool) {
	var in struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &in) // as modelsChange
	if in.Action == "search" || in.Action == "preview" {
		return auditedChange{}, false // only look
	}
	var out struct {
		ModelID string `json:"modelId"`
	}
	_ = json.Unmarshal(response, &out) // a refusal has no model
	return auditedChange{in.Action, "model", out.ModelID}, true
}

func hfTokenChange(_ *http.Request, body, _ []byte) (auditedChange, bool) {
	var in struct {
		Action string `json:"action"` // and nothing else: the token is in this body
	}
	_ = json.Unmarshal(body, &in) // as modelsChange
	if in.Action == "validate" {
		return auditedChange{}, false
	}
	return auditedChange{in.Action, "hf-token", ""}, true
}

func voicesChange(r *http.Request, _, response []byte) (auditedChange, bool) {
	id := strings.TrimPrefix(r.URL.Path, "/_localbase/voices/")
	switch r.Method {
	case http.MethodPost:
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(response, &out) // a refusal has no voice
		return auditedChange{"create", "voice", out.ID}, true
	case http.MethodPatch:
		return auditedChange{"rename", "voice", id}, true
	case http.MethodDelete:
		return auditedChange{"delete", "voice", id}, true
	}
	return auditedChange{}, false
}

// computerNameChange doesn't name the name: it is free text.
func computerNameChange(*http.Request, []byte, []byte) (auditedChange, bool) {
	return auditedChange{"set", "computer-name", ""}, true
}
