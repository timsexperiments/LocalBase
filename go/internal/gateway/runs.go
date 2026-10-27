package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// Detached runs (DESIGN §21): a UI inference request that runs on the
// server independent of any browser connection. A run dispatches its body
// to the real /v1 handler on a detached context, so validation, scopes,
// admission, the queue and memory pressure are the API's own. Pages follow
// it over SSE, resume with Last-Event-ID, and only an explicit Stop cancels
// it. Nothing about /v1 itself changes.

// runTargets are the /v1 routes a run may wrap, with the indicator label.
var runTargets = map[string]string{
	"/v1/chat/completions": "Chat",
}

const (
	runActivePerOwner   = 4
	runActiveTotal      = 32
	runRetainedPerOwner = 32
	runMaxDuration      = 30 * time.Minute
	runMaxOutput        = 8 << 20
	runRetention        = 30 * time.Minute
	runPing             = 15 * time.Second
	runCheckpointEvery  = time.Second
)

// Run statuses.
const (
	runQueued      = "queued"
	runInProgress  = "in_progress"
	runCompleted   = "completed"
	runFailed      = "failed"
	runCancelled   = "cancelled"
	runInterrupted = "interrupted"
)

// runHooks tell a run how its inner request progresses: queue() before
// admission (a queued run holds no memory admission), admit() once
// admitted, finish() with the inference's terminal source. A nil *runHooks
// (every ordinary request) does nothing.
type runHooks struct {
	queued, admitted func()
	finished         func(source string)
}

type runHooksKey struct{}

func runHooksFrom(ctx context.Context) *runHooks {
	h, _ := ctx.Value(runHooksKey{}).(*runHooks)
	return h
}

func (h *runHooks) queue() {
	if h != nil && h.queued != nil {
		h.queued()
	}
}

func (h *runHooks) admit() {
	if h != nil && h.admitted != nil {
		h.admitted()
	}
}

func (h *runHooks) finish(source string) {
	if h != nil && h.finished != nil {
		h.finished(source)
	}
}

// runOutput is a chat run's result so far, accumulated from its chunks.
type runOutput struct {
	Content      string           `json:"content"`
	Reasoning    string           `json:"reasoning,omitempty"`
	ToolCalls    []map[string]any `json:"toolCalls,omitempty"`
	FinishReason string           `json:"finishReason,omitempty"`
	Usage        map[string]any   `json:"usage,omitempty"`
}

type runEvent struct {
	id   int
	name string
	data []byte
}

// runRecord is a run's public state; it is also the checkpoint file's
// shape (plus the owner).
type runRecord struct {
	ID        string         `json:"id"`
	Object    string         `json:"object"`
	Owner     string         `json:"owner,omitempty"`
	Target    string         `json:"target"`
	Label     string         `json:"label"`
	Model     string         `json:"model"`
	Status    string         `json:"status"`
	Reason    string         `json:"reason,omitempty"`
	Error     map[string]any `json:"error"`
	RequestID string         `json:"requestId,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	StartedAt *time.Time     `json:"startedAt"`
	EndedAt   *time.Time     `json:"endedAt"`
	ExpiresAt *time.Time     `json:"expiresAt"`
	Output    *runOutput     `json:"output,omitempty"`
	// Events is the checkpoint's event count; a restored run numbers its
	// snapshot and end after it.
	Events int `json:"events,omitempty"`
}

type run struct {
	m  *runManager
	mu sync.Mutex
	runRecord
	output     runOutput
	events     []runEvent
	bytes      int
	notify     chan struct{} // closed and replaced on every event
	done       chan struct{} // closed when the run ends
	cancel     context.CancelFunc
	stopStatus string
	stopReason string
	source     string // the inference's terminal source
	queuedSeen bool
	restored   bool // read back from a checkpoint after a restart
	saved      time.Time
	span       *otlp.Span
}

func (r *run) terminal() bool {
	switch r.Status {
	case runCompleted, runFailed, runCancelled, runInterrupted:
		return true
	}
	return false
}

type runManager struct {
	s   *Server
	dir string // "" keeps runs in memory only
	now func() time.Time

	retention, maxDuration, ping time.Duration
	maxOutput                    int

	mu      sync.Mutex
	runs    map[string]*run
	closing bool
	quit    chan struct{}
	wg      sync.WaitGroup
}

func newRunManager(s *Server, dir string) *runManager {
	m := &runManager{s: s, dir: dir, now: time.Now, retention: runRetention, maxDuration: runMaxDuration,
		ping: runPing, maxOutput: runMaxOutput, runs: map[string]*run{}, quit: make(chan struct{})}
	m.sweepStart()
	return m
}

// --- checkpoints ------------------------------------------------------------------

func (m *runManager) path(id string) string { return filepath.Join(m.dir, id+".json") }

// checkpoint writes the run's state, output included, for a restart to
// report (DESIGN §21.6). Called with r.mu held.
func (m *runManager) checkpoint(r *run) {
	if m.dir == "" {
		return
	}
	rec := r.runRecord
	out := r.output
	rec.Output = &out
	if !r.restored {
		rec.Events = len(r.events)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if os.MkdirAll(m.dir, 0o700) != nil {
		return
	}
	tmp := m.path(r.ID) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, m.path(r.ID))
	}
	r.saved = m.now()
}

func (m *runManager) remove(id string) {
	if m.dir != "" {
		os.Remove(m.path(id))
		os.Remove(m.path(id) + ".tmp")
	}
}

// sweepStart reads the checkpoints a previous gateway left: unfinished runs
// become interrupted (gateway_restart), expired and unreadable files go.
func (m *runManager) sweepStart() {
	if m.dir == "" {
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	now := m.now()
	for _, e := range entries {
		p := filepath.Join(m.dir, e.Name())
		id, ok := strings.CutSuffix(e.Name(), ".json")
		b, err := os.ReadFile(p)
		var rec runRecord
		if !ok || !strings.HasPrefix(id, "run_") || err != nil || json.Unmarshal(b, &rec) != nil || rec.ID != id || rec.Owner == "" {
			os.Remove(p)
			continue
		}
		r := &run{m: m, runRecord: rec, restored: true, notify: make(chan struct{}), done: make(chan struct{})}
		close(r.done)
		if rec.Output != nil {
			r.output = *rec.Output
		}
		r.Output = nil
		if !r.terminal() {
			r.Status, r.Reason = runInterrupted, "gateway_restart"
			r.EndedAt = &now
			exp := now.Add(m.retention)
			r.ExpiresAt = &exp
			m.checkpoint(r)
			m.log(r, "run.interrupted")
		}
		if r.ExpiresAt == nil || !now.Before(*r.ExpiresAt) {
			os.Remove(p)
			continue
		}
		m.runs[id] = r
	}
}

// sweepExpired deletes finished runs past their retention.
func (m *runManager) sweepExpired() {
	now := m.now()
	m.mu.Lock()
	var gone []*run
	for id, r := range m.runs {
		r.mu.Lock()
		if r.terminal() && r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
			delete(m.runs, id)
			gone = append(gone, r)
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	for _, r := range gone {
		m.remove(r.ID)
		m.log(r, "run.expired")
	}
}

func (m *runManager) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.sweepExpired()
		case <-m.quit:
			return
		}
	}
}

// --- telemetry ---------------------------------------------------------------------

// log records a lifecycle event: ids and outcomes, never content.
func (m *runManager) log(r *run, event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	attrs := []any{"event", event, "category", "run", "run_id", r.ID, "target", r.Target, "model_id", r.Model,
		"owner", r.Owner, "status", r.Status}
	if r.RequestID != "" {
		attrs = append(attrs, "request_id", r.RequestID)
	}
	if r.Reason != "" {
		attrs = append(attrs, "reason", r.Reason)
	}
	if code, ok := r.Error["code"].(string); ok && code != "" {
		attrs = append(attrs, "error_code", code)
	}
	if r.EndedAt != nil {
		attrs = append(attrs, "duration_ms", round2(ms(r.EndedAt.Sub(r.CreatedAt))), "output_bytes", r.bytes, "events", len(r.events))
	}
	level := slog.LevelInfo
	switch event {
	case "run.failed":
		level = slog.LevelWarn
	case "run.interrupted":
		level = slog.LevelWarn
	}
	msg := map[string]string{"run.started": "Run started.", "run.completed": "Run completed.", "run.failed": "Run failed.",
		"run.cancelled": "Run stopped.", "run.interrupted": "Run interrupted.", "run.expired": "Run expired.",
		"run.dismissed": "Run dismissed."}[event]
	m.s.Log.Log(context.Background(), level, msg, attrs...)
}

// --- events ------------------------------------------------------------------------

// emit appends an event and wakes the readers. Called with r.mu held.
func (r *run) emit(name string, data []byte) {
	r.events = append(r.events, runEvent{id: len(r.events) + 1, name: name, data: data})
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *run) setStatus(status, reason string) {
	r.Status, r.Reason = status, reason
	b, _ := json.Marshal(map[string]any{"status": status, "reason": reason})
	if reason == "" {
		b, _ = json.Marshal(map[string]any{"status": status})
	}
	r.emit("status", b)
}

// view is the public run object. Called with r.mu held.
func (r *run) view(withOutput bool) map[string]any {
	v := map[string]any{"id": r.ID, "object": "localbase.run", "target": r.Target, "label": r.Label, "model": r.Model,
		"status": r.Status, "error": r.Error, "createdAt": r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"startedAt": stamp(r.StartedAt), "endedAt": stamp(r.EndedAt), "expiresAt": stamp(r.ExpiresAt),
		"lastEventId": r.lastEventID(), "resumable": false}
	if r.Reason != "" {
		v["reason"] = r.Reason
	}
	if r.RequestID != "" {
		v["requestId"] = r.RequestID
	}
	if r.Status == runInterrupted {
		v["continuation"] = map[string]any{"by": "client", "message": "LocalBase does not keep the request, so it cannot " +
			"resume this run. To continue, send the conversation again with the partial reply."}
	}
	if withOutput {
		out := r.output
		v["output"] = &out
	}
	return v
}

func (r *run) lastEventID() int {
	if r.restored {
		return r.Events + 2 // snapshot, end
	}
	return len(r.events)
}

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// --- execution ---------------------------------------------------------------------

// runWriter receives the inner /v1 response: an event stream becomes run
// events and accumulated output; anything else is kept for the error.
type runWriter struct {
	r      *run
	header http.Header
	status int
	sse    bool
	buf    []byte
	body   bytes.Buffer
}

func (w *runWriter) Header() http.Header { return w.header }

func (w *runWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	ct := strings.TrimSpace(strings.SplitN(w.header.Get("Content-Type"), ";", 2)[0])
	w.sse = code >= 200 && code < 300 && strings.EqualFold(ct, "text/event-stream")
}

func (w *runWriter) Flush() {}

func (w *runWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if !w.sse {
		if w.body.Len() < 1<<20 {
			w.body.Write(b[:min(len(b), 1<<20-w.body.Len())])
		}
		return len(b), nil
	}
	w.buf = append(w.buf, b...)
	for {
		i := bytes.Index(w.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		block := string(w.buf[:i])
		w.buf = w.buf[i+2:]
		w.event(block)
	}
	return len(b), nil
}

// event takes one relayed SSE event: a chunk, [DONE], or an error.
func (w *runWriter) event(block string) {
	var data []string
	for _, line := range strings.Split(block, "\n") {
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(d, " "))
		}
	}
	payload := strings.Join(data, "\n")
	if payload == "" || payload == "[DONE]" {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	r := w.r
	m := r.m
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := obj["error"].(map[string]any); ok {
		r.Error = e
		r.emit("error", []byte(payload))
		return
	}
	r.bytes += len(payload)
	if r.bytes > m.maxOutput {
		if r.stopStatus == "" {
			r.stopStatus, r.stopReason = runFailed, "output_limit"
			r.cancel()
		}
		return
	}
	r.output.add(obj)
	r.emit("chunk", []byte(payload))
	if m.now().Sub(r.saved) >= runCheckpointEvery {
		m.checkpoint(r)
	}
}

// add folds one chat.completion.chunk into the output.
func (o *runOutput) add(chunk map[string]any) {
	if u, ok := chunk["usage"].(map[string]any); ok {
		o.Usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if idx, _ := ch["index"].(float64); idx != 0 {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok {
			o.FinishReason = fr
		}
		d, _ := ch["delta"].(map[string]any)
		if s, ok := d["content"].(string); ok {
			o.Content += s
		}
		if s, ok := d["reasoning"].(string); ok {
			o.Reasoning += s
		}
		calls, _ := d["tool_calls"].([]any)
		for _, tc := range calls {
			call, _ := tc.(map[string]any)
			i := 0
			if f, ok := call["index"].(float64); ok {
				i = int(f)
			}
			for len(o.ToolCalls) <= i {
				o.ToolCalls = append(o.ToolCalls, map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}})
			}
			dst := o.ToolCalls[i]
			if id, ok := call["id"].(string); ok && id != "" {
				dst["id"] = id
			}
			fn, _ := call["function"].(map[string]any)
			dfn := dst["function"].(map[string]any)
			if n, ok := fn["name"].(string); ok {
				dfn["name"] = dfn["name"].(string) + n
			}
			if a, ok := fn["arguments"].(string); ok {
				dfn["arguments"] = dfn["arguments"].(string) + a
			}
		}
	}
}

// newRunID is 128 random bits.
func newRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "run_" + hex.EncodeToString(b)
}

// ownerOf is the run owner for a request's principal.
func (s *Server) runOwner(r *http.Request) string {
	if s.NoAuth {
		return "local"
	}
	return PrincipalFrom(r.Context()).OwnerID()
}

// create starts a run of target with the request's body. A request the /v1
// handler refuses before queueing (validation, scope, unknown model) is
// answered here as /v1 would, and no run exists.
func (m *runManager) create(w http.ResponseWriter, req *http.Request, target string) {
	label, ok := runTargets[target]
	if !ok {
		routeDisabled(w, req)
		return
	}
	if req.Method != http.MethodPost {
		methodNotAllowed(w, req, "POST")
		return
	}
	body, ok := readRunBody(w, req, target)
	if !ok {
		return
	}
	owner := m.s.runOwner(req)
	now := m.now()
	r := &run{m: m, runRecord: runRecord{ID: newRunID(), Object: "localbase.run", Owner: owner, Target: target, Label: label,
		Status: runQueued, CreatedAt: now}, notify: make(chan struct{}), done: make(chan struct{})}
	var probe struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &probe)
	r.Model = probe.Model

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		serviceUnavailable(w, req, "Gateway")
		return
	}
	active, mine := 0, 0
	for _, o := range m.runs {
		o.mu.Lock()
		if !o.terminal() {
			active++
			if o.Owner == owner {
				mine++
			}
		}
		o.mu.Unlock()
	}
	if mine >= runActivePerOwner || active >= runActiveTotal {
		m.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		writeError(w, req, 429, "rate_limit_error", "too_many_runs",
			fmt.Sprintf("Too many generations are running (at most %d per account). Wait for one to finish or stop one.", runActivePerOwner), nil)
		return
	}
	m.runs[r.ID] = r
	m.trim(owner)
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.WithoutCancel(req.Context()))
	r.cancel = cancel
	ctx, r.span = m.s.Otel.Start(ctx, "localbase.run", otlp.KindInternal)
	r.span.SetAttr("localbase.run.id", r.ID)
	r.span.SetAttr("localbase.run.target", target)
	ready := make(chan struct{})
	var readyOnce sync.Once
	signal := func() { readyOnce.Do(func() { close(ready) }) }
	rw := &runWriter{r: r, header: http.Header{}}
	hooks := &runHooks{
		queued: func() {
			r.mu.Lock()
			r.queuedSeen = true
			r.RequestID = rw.header.Get("X-Localbase-Request-Id")
			r.setStatus(runQueued, "")
			m.checkpoint(r)
			r.mu.Unlock()
			m.log(r, "run.started")
			signal()
		},
		admitted: func() {
			r.mu.Lock()
			t := m.now()
			r.StartedAt = &t
			r.setStatus(runInProgress, "")
			m.checkpoint(r)
			r.mu.Unlock()
		},
		finished: func(source string) {
			r.mu.Lock()
			r.source = source
			r.mu.Unlock()
		},
	}
	ctx = context.WithValue(ctx, runHooksKey{}, hooks)
	// The inner request acts as the caller: the /v1 guard checks its scope.
	if p, ok := req.Context().Value(principalKey{}).(*auth.Principal); ok {
		ctx = context.WithValue(ctx, browserPrincipalKey{}, p)
	}
	inner, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	inner.Header.Set("Content-Type", req.Header.Get("Content-Type"))
	inner.Host, inner.RemoteAddr = req.Host, req.RemoteAddr
	timer := time.AfterFunc(m.maxDuration, func() { m.stop(r, runInterrupted, "max_duration") })

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer timer.Stop()
		m.s.ServeHTTP(rw, inner)
		m.finish(r, rw)
		signal()
	}()
	<-ready

	r.mu.Lock()
	if !r.queuedSeen {
		// Refused before the queue: answer as /v1 and forget the run.
		r.mu.Unlock()
		m.mu.Lock()
		delete(m.runs, r.ID)
		m.mu.Unlock()
		m.remove(r.ID)
		r.span.End()
		status := rw.status
		if status == 0 || status < 400 {
			status = 502
		}
		for k, v := range rw.header {
			if k != "X-Localbase-Request-Id" {
				w.Header()[k] = v
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		w.Write(rw.body.Bytes())
		return
	}
	v := r.view(true)
	r.mu.Unlock()
	w.Header().Set("Location", "/_localbase/runs/"+r.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 202, v)
}

// readRunBody reads the /v1 body; chat runs always stream upstream so that
// partial output exists at every moment (DESIGN §21.1).
func readRunBody(w http.ResponseWriter, req *http.Request, target string) ([]byte, bool) {
	if target != "/v1/chat/completions" {
		return nil, false
	}
	obj, ok := readJSON(w, req)
	if !ok {
		return nil, false
	}
	obj["stream"] = true
	opts, _ := obj["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	obj["stream_options"] = opts
	b, _ := json.Marshal(obj)
	return b, true
}

// finish settles a run once its inner request has returned.
func (m *runManager) finish(r *run, rw *runWriter) {
	r.mu.Lock()
	if !r.queuedSeen {
		r.mu.Unlock()
		return
	}
	status, reason := runCompleted, ""
	switch {
	case r.stopStatus != "":
		status, reason = r.stopStatus, r.stopReason
	case r.source == "memory_emergency":
		status, reason = runInterrupted, "memory_pressure"
	case r.Error != nil:
		status = runFailed
	case rw.status >= 400 || !rw.sse:
		status = runFailed
		var env struct {
			Error map[string]any `json:"error"`
		}
		if json.Unmarshal(rw.body.Bytes(), &env) == nil && env.Error != nil {
			r.Error = env.Error
		} else {
			r.Error = map[string]any{"type": "server_error", "code": "upstream_error", "message": "The run ended without a result."}
		}
	}
	now := m.now()
	exp := now.Add(m.retention)
	r.EndedAt, r.ExpiresAt = &now, &exp
	r.setStatus(status, reason)
	end, _ := json.Marshal(r.view(false))
	r.emit("end", end)
	m.checkpoint(r)
	close(r.done)
	r.mu.Unlock()
	r.span.SetAttr("localbase.run.status", status)
	if status == runFailed {
		r.span.SetError("")
	}
	r.span.End()
	m.log(r, map[string]string{runCompleted: "run.completed", runFailed: "run.failed", runCancelled: "run.cancelled",
		runInterrupted: "run.interrupted"}[status])
}

// stop cancels an active run with a final status. The first stop wins.
func (m *runManager) stop(r *run, status, reason string) {
	r.mu.Lock()
	if r.terminal() || r.stopStatus != "" || r.cancel == nil {
		r.mu.Unlock()
		return
	}
	r.stopStatus, r.stopReason = status, reason
	cancel := r.cancel
	r.mu.Unlock()
	cancel()
}

// trim drops the owner's oldest finished runs over the retained limit.
// Called with m.mu held.
func (m *runManager) trim(owner string) {
	var mine []*run
	for _, r := range m.runs {
		if r.Owner == owner {
			mine = append(mine, r)
		}
	}
	if len(mine) <= runRetainedPerOwner {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt.Before(mine[j].CreatedAt) })
	excess := len(mine) - runRetainedPerOwner
	for _, r := range mine {
		if excess == 0 {
			break
		}
		r.mu.Lock()
		done := r.terminal()
		r.mu.Unlock()
		if done {
			delete(m.runs, r.ID)
			m.remove(r.ID)
			m.log(r, "run.dismissed")
			excess--
		}
	}
}

// Shutdown interrupts active runs (gateway_restart) and ends every event
// stream, before the HTTP server drains and the runtimes stop.
func (m *runManager) Shutdown(wait time.Duration) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	m.closing = true
	runs := make([]*run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		m.stop(r, runInterrupted, "gateway_restart")
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
	}
	close(m.quit)
}

// --- routes ------------------------------------------------------------------------

// lookup returns the caller's run, or nil: someone else's run is as
// unknown as a missing one.
func (m *runManager) lookup(req *http.Request, id string) *run {
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil || r.Owner != m.s.runOwner(req) {
		return nil
	}
	return r
}

func runNotFound(w http.ResponseWriter, req *http.Request) {
	writeError(w, req, 404, "invalid_request_error", "run_not_found", "No such run.", nil)
}

// runsList is GET /_localbase/runs: the caller's runs for the
// "Generating…" indicator, newest first, without output or request text.
func (s *Server) runsList(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w, req, "GET")
		return
	}
	m := s.runs
	m.sweepExpired()
	owner := s.runOwner(req)
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
		r.mu.Unlock()
		data = append(data, item)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// runRoute serves /_localbase/runs/{rest...}: v1/<route> creates; {id},
// {id}/events and {id}/cancel act on one run.
func (s *Server) runRoute(w http.ResponseWriter, req *http.Request) {
	m := s.runs
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
			writeJSON(w, 200, v)
		case http.MethodDelete:
			m.stop(r, runCancelled, "stopped")
			<-r.done
			m.mu.Lock()
			delete(m.runs, r.ID)
			m.mu.Unlock()
			m.remove(r.ID)
			m.log(r, "run.dismissed")
			writeJSON(w, 200, map[string]any{"id": r.ID, "object": "localbase.run.deleted", "deleted": true})
		default:
			methodNotAllowed(w, req, "GET, DELETE")
		}
	case "cancel":
		if req.Method != http.MethodPost {
			methodNotAllowed(w, req, "POST")
			return
		}
		m.stop(r, runCancelled, "stopped")
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
		}
		r.mu.Lock()
		v := r.view(true)
		r.mu.Unlock()
		writeJSON(w, 200, v)
	case "events":
		if req.Method != http.MethodGet {
			methodNotAllowed(w, req, "GET")
			return
		}
		m.events(w, req, r)
	default:
		runNotFound(w, req)
	}
}

// events streams a run: everything after Last-Event-ID (or ?after=), then
// live, with a comment keep-alive, until the end event (DESIGN §21.3).
func (m *runManager) events(w http.ResponseWriter, req *http.Request, r *run) {
	after, _ := strconv.Atoi(req.Header.Get("Last-Event-ID"))
	if a, err := strconv.Atoi(req.URL.Query().Get("after")); err == nil && req.Header.Get("Last-Event-ID") == "" {
		after = a
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
		r.mu.Lock()
		var pending []runEvent
		if after < len(r.events) {
			pending = append(pending, r.events[after:]...)
		}
		notify := r.notify
		ended := r.terminal() && r.EndedAt != nil
		total := len(r.events)
		r.mu.Unlock()
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
