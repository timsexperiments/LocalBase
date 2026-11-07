package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// --- execution ---------------------------------------------------------------------

// newRunID is 128 random bits.
func newRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "run_" + hex.EncodeToString(b)
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
	owner := m.opts.owner(req)
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

	ctx, r.span = m.opts.otel().Start(ctx, "localbase.run", otlp.KindInternal)
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
		m.opts.handler.ServeHTTP(rw, inner)
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
