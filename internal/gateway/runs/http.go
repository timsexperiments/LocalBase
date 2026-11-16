package runs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// --- routes ------------------------------------------------------------------------

// lookup returns the caller's run, or nil: someone else's run is as
// unknown as a missing one.
func (m *Manager) lookup(req *http.Request, id string) *run {
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil || r.Owner != m.opts.Owner(req) {
		return nil
	}
	return r
}

func runNotFound(w http.ResponseWriter, req *http.Request) {
	httpx.Error(w, req, 404, "invalid_request_error", "run_not_found", "No such run.", nil)
}

// List is GET /_localbase/runs: the caller's runs for the
// "Generating…" indicator, newest first, without output or request text.
func (m *Manager) List(w http.ResponseWriter, req *http.Request) {
	m.sweepExpired()
	owner := m.opts.Owner(req)
	m.mu.Lock()
	var mine []*run
	for _, r := range m.runs {
		if r.Owner == owner {
			mine = append(mine, r)
		}
	}
	m.mu.Unlock()
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt.After(mine[j].CreatedAt) })
	data := []any{}
	for _, r := range mine {
		r.mu.Lock()
		item := map[string]any{"id": r.ID, "target": r.Target, "label": r.Label, "model": r.Model, "status": r.Status,
			"createdAt": r.CreatedAt.UTC().Format(time.RFC3339Nano), "startedAt": stamp(r.StartedAt), "endedAt": stamp(r.EndedAt)}
		if r.Reason != "" {
			item["reason"] = r.Reason
		}
		if r.Error != nil { // a run that stopped says why, as its own read does
			item["error"] = listError(r.Error)
		}
		r.mu.Unlock()
		data = append(data, item)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, map[string]any{"object": "list", "data": data})
}

// Route serves /_localbase/runs/{rest...}: v1/<route> creates; {id},
// {id}/events and {id}/cancel act on one run.
func (m *Manager) Route(w http.ResponseWriter, req *http.Request) {
	rest := req.PathValue("rest")
	if target, ok := strings.CutPrefix(rest, "v1/"); ok {
		m.create(w, req, "/v1/"+target)
		return
	}
	m.sweepExpired()
	id, action, _ := strings.Cut(rest, "/")
	r := m.lookup(req, id)
	if r == nil {
		runNotFound(w, req)
		return
	}
	switch action {
	case "":
		switch req.Method {
		case http.MethodGet:
			r.mu.Lock()
			v := r.view(true)
			r.mu.Unlock()
			w.Header().Set("Cache-Control", "no-store")
			httpx.JSON(w, 200, v)
		case http.MethodDelete:
			m.stop(r, runCancelled, "stopped")
			select {
			case <-r.done:
			case <-req.Context().Done():
				return // the caller left: the run is stopped, and stays until it expires
			case <-time.After(m.stopWait):
			}
			m.dismiss(r)
			httpx.JSON(w, 200, map[string]any{"id": r.ID, "object": "localbase.run.deleted", "deleted": true})
		default:
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, req, "GET, DELETE")
		}
	case "cancel":
		if req.Method != http.MethodPost {
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, req, "POST")
			return
		}
		m.stop(r, runCancelled, "stopped")
		select {
		case <-r.done:
		case <-time.After(m.stopWait):
		}
		r.mu.Lock()
		v := r.view(true)
		r.mu.Unlock()
		httpx.JSON(w, 200, v)
	case "result":
		if req.Method != http.MethodGet {
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, req, "GET")
			return
		}
		r.mu.Lock()
		body, ctype := r.result, r.resultType
		r.mu.Unlock()
		if body == nil {
			httpx.Error(w, req, 409, "invalid_request_error", "run_result_unavailable",
				"This run has no result to fetch: it has not completed, or it is not a media run.", nil)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	case "events":
		if req.Method != http.MethodGet {
			// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
			httpx.MethodNotAllowed(w, req, "GET")
			return
		}
		m.events(w, req, r)
	default:
		runNotFound(w, req)
	}
}

// eventCursor reads where a stream resumes: Last-Event-ID, or else ?after=. No
// cursor is the start.
func eventCursor(req *http.Request) (int, bool) {
	raw := req.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = req.URL.Query().Get("after")
	}
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 0
}

// eventsAfter is what a stream has yet to send. The lock is released however
// this ends: a run whose lock stays taken wedges every later caller.
func (r *run) eventsAfter(after int) (pending []runEvent, notify chan struct{}, ended bool, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if after < len(r.events) {
		pending = append(pending, r.events[after:]...)
	}
	return pending, r.notify, r.terminal() && r.EndedAt != nil, len(r.events)
}

// events streams a run: everything after Last-Event-ID (or ?after=), then
// live, with a comment keep-alive, until the end event (DESIGN §21.3).
func (m *Manager) events(w http.ResponseWriter, req *http.Request, r *run) {
	after, ok := eventCursor(req)
	if !ok {
		httpx.Error(w, req, 400, "invalid_request_error", "invalid_event_cursor",
			"The event cursor must be a whole number, 0 or more.", nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	write := func(e runEvent) bool {
		_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.id, e.name, e.data)
		return err == nil
	}
	if r.restored {
		// The event buffer did not survive the restart, and a resuming page's
		// last id may be past the last checkpoint: whatever it sends, it gets
		// the snapshot and end, numbered after the checkpointed events.
		r.mu.Lock()
		snap, _ := json.Marshal(r.view(true))
		end, _ := json.Marshal(r.view(false))
		base := r.Events
		r.mu.Unlock()
		for _, e := range []runEvent{{base + 1, "snapshot", snap}, {base + 2, "end", end}} {
			write(e)
		}
		flush()
		return
	}
	w.Write([]byte(": connected\n\n"))
	flush()
	ping := time.NewTicker(m.ping)
	defer ping.Stop()
	for {
		pending, notify, ended, total := r.eventsAfter(after)
		for _, e := range pending {
			if !write(e) {
				return
			}
			after = e.id
		}
		if len(pending) > 0 {
			flush()
		}
		if ended && after >= total {
			return
		}
		select {
		case <-notify:
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flush()
		case <-req.Context().Done():
			return
		case <-m.quit:
			return
		}
	}
}

// requestModel is the body's "model" (JSON or a multipart field), for the
// indicator; "" means the route's active model.
func requestModel(ctype string, body []byte) string {
	mt, params, _ := mime.ParseMediaType(ctype)
	if mt != "multipart/form-data" {
		var probe struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &probe)
		return probe.Model
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for range httpx.MaxFormParts {
		p, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if p.FormName() == "model" && p.FileName() == "" {
			b, _ := io.ReadAll(io.LimitReader(p, 256))
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
