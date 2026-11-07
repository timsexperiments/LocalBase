package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// Detached runs (DESIGN §21): a UI inference request that runs on the
// server independent of any browser connection. A run dispatches its body
// to the real /v1 handler on a detached context, so validation, scopes,
// admission, the queue and memory pressure are the API's own. Pages follow
// it over SSE, resume with Last-Event-ID, and only an explicit Stop cancels
// it. Nothing about /v1 itself changes.

// chatTarget is the one target whose output streams and is checkpointed.
const chatTarget = "/v1/chat/completions"

// runTargets are the /v1 routes a run may wrap, with the indicator label.
var runTargets = map[string]string{
	"/v1/chat/completions":     "Chat",
	"/v1/images/generations":   "Image",
	"/v1/images/edits":         "Image edit",
	"/v1/audio/speech":         "Speech",
	"/v1/audio/transcriptions": "Transcription",
	"/v1/audio/translations":   "Translation",
	"/v1/systemone":            "Decision",
}

const (
	runActivePerOwner   = 4
	runActiveTotal      = 32
	runRetainedPerOwner = 32
	runMaxDuration      = 30 * time.Minute
	runMaxToolCalls     = 128
	runMaxOutput        = 8 << 20
	runMaxResult        = 64 << 20 // a media result, in memory only
	// What runs may hold in memory, overhead included: one chat run, and all runs together with
	// media results. Proposals, not measured against real traffic. runMaxOutput normally ends a
	// chat run first; this catches many tiny chunks, whose overhead it doesn't count.
	runMaxHeld         = 32 << 20
	runsMaxHeld        = 256 << 20
	runEventHeldFor    = 64       // a retained event beyond its payload
	runMaxBody         = 32 << 20 // a run's request body (the /v1 route applies its own limit)
	runRetention       = 30 * time.Minute
	runPing            = 15 * time.Second
	runCheckpointEvery = time.Second
	runStopWait        = 10 * time.Second // how long a cancel or a DELETE waits for a stopped run to end
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
	result     []byte // a media run's response body, never checkpointed
	resultType string
	notes      []string // facts about the result that a header carries on /v1
	events     []runEvent
	bytes      int
	held       int64         // what it counts towards runManager.held
	notify     chan struct{} // closed and replaced on every event
	done       chan struct{} // closed when the run ends
	cancel     context.CancelFunc
	stopStatus string
	stopReason string
	source     string // the inference's terminal source
	queuedSeen bool
	restored   bool  // read back from a checkpoint after a restart
	body       int64 // the request body, counted in runManager.held while the inner request holds it
	dismissed  bool  // deleted before it ended: finish gives back what it held since
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
	stopWait                     time.Duration
	maxOutput, maxResult         int
	maxHeld, maxHeldTotal        int64
	held                         atomic.Int64 // what all retained runs hold

	mu      sync.Mutex
	runs    map[string]*run
	closing bool
	quit    chan struct{}
	wg      sync.WaitGroup

	// afterPublish, if set, runs once a new run is visible to Shutdown (tests).
	afterPublish func()
}

func newRunManager(s *Server, dir string) *runManager {
	m := &runManager{s: s, dir: dir, now: time.Now, retention: runRetention, maxDuration: runMaxDuration,
		ping: runPing, stopWait: runStopWait, maxOutput: runMaxOutput, maxResult: runMaxResult, maxHeld: runMaxHeld, maxHeldTotal: runsMaxHeld, runs: map[string]*run{}, quit: make(chan struct{})}
	if s.RunMemoryLimit > 0 {
		m.maxHeld = s.RunMemoryLimit
	}
	if s.RunsMemoryLimit > 0 {
		m.maxHeldTotal = s.RunsMemoryLimit
	}
	m.sweepStart()
	return m
}

// charge counts n more bytes held for a chat run, with r.mu held, unless that passes a limit: then it
// counts nothing and says which.
func (m *runManager) charge(r *run, n int64) string {
	if r.held+n > m.maxHeld {
		return "output_limit"
	}
	if m.held.Add(n) > m.maxHeldTotal {
		m.held.Add(-n)
		return "memory_limit"
	}
	r.held += n
	return ""
}

// CodeRunsMemoryFull is the error code of a run stopped by the total limit. The pages show its message.
const CodeRunsMemoryFull = "runs_memory_full"

// memoryFullError says what holds the memory and what to do, for a run stopped by the total limit.
// Finished runs keep what they hold until they expire, so that is usually what fills it.
func (m *runManager) memoryFullError() map[string]any {
	var finished int64
	var oldest *time.Time
	m.mu.Lock()
	for _, r := range m.runs {
		r.mu.Lock()
		if r.terminal() {
			finished += r.held
			if r.ExpiresAt != nil && (oldest == nil || r.ExpiresAt.Before(*oldest)) {
				oldest = r.ExpiresAt
			}
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	message := fmt.Sprintf("Results are holding %d MiB of memory, and the limit is %d MiB, so this was stopped.", mib(m.held.Load()), mib(m.maxHeldTotal))
	out := map[string]any{"type": "server_error", "code": CodeRunsMemoryFull, "heldBytes": m.held.Load(),
		"limitBytes": m.maxHeldTotal, "finishedBytes": finished}
	if oldest == nil {
		message += " Nothing has finished yet, so wait for the running ones and try again."
	} else {
		at := oldest.UTC().Format("15:04") + " UTC"
		message += fmt.Sprintf(" Finished results hold %d MiB, and the oldest expires at %s. Remove finished results, or wait, and try again. A batch of pictures frees its memory when the last one is removed.", mib(finished), at)
		out["oldestExpiresAt"] = oldest.UTC().Format(time.RFC3339)
	}
	out["message"] = message
	return out
}

// mib rounds up, so that a small amount doesn't read as none.
func mib(b int64) int64 { return (b + 1<<20 - 1) >> 20 }

// release ends what a run holds, when it is dropped.
func (m *runManager) release(r *run) {
	r.mu.Lock()
	m.held.Add(-r.held - r.body)
	r.held, r.body = 0, 0
	r.mu.Unlock()
}

// dismiss drops a run its owner deleted. A run that hadn't ended by then (a stop it took longer than
// stopWait to obey) is given back once more when it ends, by finish.
func (m *runManager) dismiss(r *run) {
	m.mu.Lock()
	delete(m.runs, r.ID)
	m.mu.Unlock()
	r.mu.Lock()
	r.dismissed = true
	r.mu.Unlock()
	m.release(r)
	m.remove(r.ID)
	m.log(r, "run.dismissed")
}

// tooManyRuns is the refusal over runActivePerOwner or runActiveTotal.
func tooManyRuns(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Retry-After", "5")
	httpx.Error(w, req, 429, "rate_limit_error", "too_many_runs",
		fmt.Sprintf("Too many generations are running (at most %d per account). Wait for one to finish or stop one.", runActivePerOwner), nil)
}

// overActive reports whether owner may start no more runs. Called with m.mu held.
func (m *runManager) overActive(owner string) bool {
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
	return mine >= runActivePerOwner || active >= runActiveTotal
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
	obs.WarnRefusedWrite(m.s.Log, obs.WritePrivateAtomic(m.path(r.ID), b))
	r.saved = m.now()
}

func (m *runManager) remove(id string) {
	if m.dir != "" {
		os.Remove(m.path(id))
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
		b, err := obs.ReadOwnedRegular(p)
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
		if r.terminal() && (r.ExpiresAt == nil || !now.Before(*r.ExpiresAt)) {
			os.Remove(p)
			continue
		}
		r.held = int64(len(b)) // Its output is back in memory.
		m.held.Add(r.held)
		if !r.terminal() || (r.Target != chatTarget && r.Status == runCompleted) {
			// Unfinished, or a media result that lived in memory only and is
			// gone: either way the run was interrupted by the restart.
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
		m.release(r)
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
		attrs = append(attrs, "duration_ms", round2(ms(r.EndedAt.Sub(r.CreatedAt))), "output_bytes", r.bytes+len(r.result), "events", len(r.events))
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
	if withOutput && r.Target == chatTarget {
		v["output"] = r.output.snapshot()
	}
	if r.result != nil {
		res := map[string]any{"contentType": r.resultType, "bytes": len(r.result), "url": "/_localbase/runs/" + r.ID + "/result"}
		if len(r.notes) > 0 {
			res["notes"] = r.notes
		}
		v["result"] = res
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
	// Only chat streams become chunks; any other target's body is its result,
	// whatever its type (speech can answer as SSE).
	w.sse = w.r.Target == chatTarget && code >= 200 && code < 300 && strings.EqualFold(ct, "text/event-stream")
}

func (w *runWriter) Flush() {}

func (w *runWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if !w.sse {
		m := w.r.m
		if w.body.Len()+len(b) > m.maxResult {
			m.stop(w.r, runFailed, "output_limit")
			return len(b), nil
		}
		if m.held.Add(int64(len(b))) > m.maxHeldTotal {
			m.held.Add(-int64(len(b)))
			m.stop(w.r, runFailed, "memory_limit")
			return len(b), nil
		}
		w.r.mu.Lock()
		w.r.held += int64(len(b))
		w.r.mu.Unlock()
		w.body.Write(b)
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
		r.held += int64(len(payload))
		m.held.Add(int64(len(payload)))
		r.emit("error", []byte(payload))
		return
	}
	stop := func(reason string) {
		if r.stopStatus == "" {
			r.stopStatus, r.stopReason = runFailed, reason
			r.cancel()
		}
	}
	r.bytes += len(payload)
	if r.bytes > m.maxOutput {
		stop("output_limit")
		return
	}
	// Twice: it is kept as an event and folded into the output, which can't grow by more than it.
	if reason := m.charge(r, 2*int64(len(payload))+runEventHeldFor); reason != "" {
		stop(reason)
		return
	}
	r.output.add(obj)
	r.emit("chunk", []byte(payload))
	if m.now().Sub(r.saved) >= runCheckpointEvery {
		m.checkpoint(r)
	}
}

// snapshot copies what add changes in place, so the copy can be encoded after
// the run's lock is released. Usage is replaced whole, never changed.
func (o *runOutput) snapshot() *runOutput {
	out := *o
	if o.ToolCalls != nil {
		out.ToolCalls = make([]map[string]any, len(o.ToolCalls))
		for i, call := range o.ToolCalls {
			c := maps.Clone(call)
			if fn, ok := call["function"].(map[string]any); ok {
				c["function"] = maps.Clone(fn)
			}
			out.ToolCalls[i] = c
		}
	}
	return &out
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
				// The backend chooses this number: a delta that does not name a
				// real slot is left out of the output (its chunk is still relayed).
				if f < 0 || f >= runMaxToolCalls || f != math.Trunc(f) {
					continue
				}
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
	return httpx.PrincipalFrom(r.Context()).OwnerID()
}

// create starts a run of target with the request's body. A request the /v1
// handler refuses before queueing (validation, scope, unknown model) is
// answered here as /v1 would, and no run exists.
func (m *runManager) create(w http.ResponseWriter, req *http.Request, target string) {
	label, ok := runTargets[target]
	if !ok {
		httpx.RouteDisabled(w, req)
		return
	}
	if req.Method != http.MethodPost {
		// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
		httpx.MethodNotAllowed(w, req, "POST")
		return
	}
	owner := m.s.runOwner(req)
	// Before the body is read (up to runMaxBody), and again below, where it counts.
	m.mu.Lock()
	over := m.overActive(owner)
	m.mu.Unlock()
	if over {
		tooManyRuns(w, req)
		return
	}
	body, ok := readRunBody(w, req, target)
	if !ok {
		return
	}
	now := m.now()
	r := &run{m: m, runRecord: runRecord{ID: newRunID(), Object: "localbase.run", Owner: owner, Target: target, Label: label,
		Status: runQueued, CreatedAt: now}, notify: make(chan struct{}), done: make(chan struct{})}
	r.Model = requestModel(req.Header.Get("Content-Type"), body)
	// The body counts towards what runs hold while the inner request holds it. One that doesn't fit
	// gives a run that has already ended, as a run stopped by that limit ends.
	size := int64(len(body))
	var fullError map[string]any
	if m.held.Add(size) > m.maxHeldTotal {
		m.held.Add(-size)
		size, fullError = 0, m.memoryFullError()
	}

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		m.held.Add(-size)
		httpx.ServiceUnavailable(w, req, "Gateway")
		return
	}
	if m.overActive(owner) {
		m.mu.Unlock()
		m.held.Add(-size)
		tooManyRuns(w, req)
		return
	}
	if fullError != nil {
		r.Error = fullError
		exp := now.Add(m.retention)
		r.EndedAt, r.ExpiresAt = &now, &exp
		r.setStatus(runFailed, "memory_limit")
		end, _ := json.Marshal(r.view(false))
		r.emit("end", end)
		close(r.done)
		m.runs[r.ID] = r
		m.trim(owner)
		m.mu.Unlock()
		r.mu.Lock()
		m.checkpoint(r)
		v := r.view(true)
		r.mu.Unlock()
		m.log(r, "run.failed")
		w.Header().Set("Location", "/_localbase/runs/"+r.ID)
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, 202, v)
		return
	}
	r.body = size
	// Everything Shutdown needs is in place before the run can be seen, and
	// under the lock that checked closing: it either refused this run above or
	// finds it here, stops it and waits for it.
	ctx, cancel := context.WithCancel(context.WithoutCancel(req.Context()))
	r.cancel = cancel
	m.wg.Add(1)
	m.runs[r.ID] = r
	m.trim(owner)
	m.mu.Unlock()
	if m.afterPublish != nil {
		m.afterPublish()
	}

	ctx, r.span = m.s.Otel.Start(ctx, "localbase.run", otlp.KindInternal)
	r.span.SetAttr("localbase.run.id", r.ID)
	r.span.SetAttr("localbase.run.target", target)
	ready := make(chan struct{})
	var readyOnce sync.Once
	signal := func() { readyOnce.Do(func() { close(ready) }) }
	rw := &runWriter{r: r, header: http.Header{}}
	hooks := &httpx.RunHooks{
		Queued: func() {
			r.mu.Lock()
			r.queuedSeen = true
			r.RequestID = rw.header.Get("X-Localbase-Request-Id")
			r.setStatus(runQueued, "")
			m.checkpoint(r)
			r.mu.Unlock()
			m.log(r, "run.started")
			signal()
		},
		Admitted: func() {
			r.mu.Lock()
			t := m.now()
			r.StartedAt = &t
			r.setStatus(runInProgress, "")
			m.checkpoint(r)
			r.mu.Unlock()
		},
		Finished: func(source string) {
			r.mu.Lock()
			r.source = source
			r.mu.Unlock()
		},
	}
	ctx = context.WithValue(ctx, httpx.RunHooksKey{}, hooks)
	// The inner request acts as the caller: the /v1 guard checks its scope.
	if p, ok := req.Context().Value(httpx.PrincipalKey{}).(*auth.Principal); ok {
		ctx = context.WithValue(ctx, browserPrincipalKey{}, p)
	}
	inner, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	inner.Header.Set("Content-Type", req.Header.Get("Content-Type"))
	inner.Host, inner.RemoteAddr = req.Host, req.RemoteAddr
	timer := time.AfterFunc(m.maxDuration, func() { m.stop(r, runInterrupted, "max_duration") })

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
		m.release(r)
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
	httpx.JSON(w, 202, v)
}

// readRunBody reads the /v1 body. Chat runs always stream upstream so that
// partial output exists at every moment (DESIGN §21.1); media bodies (JSON
// or multipart) pass through as they are, for the /v1 route to check.
func readRunBody(w http.ResponseWriter, req *http.Request, target string) ([]byte, bool) {
	if target != "/v1/chat/completions" {
		if req.ContentLength > runMaxBody {
			payloadTooLarge(w, req)
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(req.Body, runMaxBody+1))
		if err != nil {
			httpx.BadRequest(w, req, "The request body could not be read.", nil)
			return nil, false
		}
		if len(b) > runMaxBody {
			payloadTooLarge(w, req)
			return nil, false
		}
		return b, true
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
	full := r.stopReason == "memory_limit"
	m.held.Add(-r.body) // the inner request is done with it
	r.body = 0
	r.mu.Unlock()
	var fullError map[string]any
	if full { // Before taking r.mu for the rest: this reads other runs, with m.mu first.
		fullError = m.memoryFullError()
	}
	r.mu.Lock()
	if !r.queuedSeen {
		close(r.done) // create forgets it; a DELETE may be waiting for it
		r.mu.Unlock()
		return
	}
	status, reason := runCompleted, ""
	switch {
	case r.stopStatus != "":
		status, reason = r.stopStatus, r.stopReason
		if fullError != nil {
			r.Error = fullError
		}
	case r.source == "memory_emergency":
		status, reason = runInterrupted, "memory_pressure"
	case r.Error != nil:
		status = runFailed
	case r.Target != chatTarget && rw.status >= 200 && rw.status < 300:
		r.result, r.resultType = bytes.Clone(rw.body.Bytes()), rw.header.Get("Content-Type")
		if rw.header.Get(audioTruncatedHeader) == "true" {
			r.notes = append(r.notes, noteAudioTruncated)
		}
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
	if rw.body.Len() > 0 && r.result == nil { // The error was taken from it, if anything was.
		r.held -= int64(rw.body.Len())
		m.held.Add(-int64(rw.body.Len()))
	}
	now := m.now()
	exp := now.Add(m.retention)
	r.EndedAt, r.ExpiresAt = &now, &exp
	r.setStatus(status, reason)
	end, _ := json.Marshal(r.view(false))
	r.emit("end", end)
	m.checkpoint(r)
	close(r.done)
	dismissed := r.dismissed
	r.mu.Unlock()
	if dismissed {
		m.release(r)
		m.remove(r.ID)
	}
	r.span.SetAttr("localbase.run.status", status)
	if status == runFailed {
		r.span.SetError("")
	}
	r.span.End()
	m.log(r, map[string]string{runCompleted: "run.completed", runFailed: "run.failed", runCancelled: "run.cancelled",
		runInterrupted: "run.interrupted"}[status])
}

// activeCount is how many runs have not finished, whoever owns them.
func (m *runManager) activeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, o := range m.runs {
		o.mu.Lock()
		if !o.terminal() {
			n++
		}
		o.mu.Unlock()
	}
	return n
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
			m.release(r)
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
	httpx.Error(w, req, 404, "invalid_request_error", "run_not_found", "No such run.", nil)
}

// runsList is GET /_localbase/runs: the caller's runs for the
// "Generating…" indicator, newest first, without output or request text.
func (s *Server) runsList(w http.ResponseWriter, req *http.Request) {
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
	httpx.JSON(w, 200, map[string]any{"object": "list", "data": data})
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
func (m *runManager) events(w http.ResponseWriter, req *http.Request, r *run) {
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
	for range maxFormParts {
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
