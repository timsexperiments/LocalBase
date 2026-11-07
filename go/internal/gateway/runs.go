package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
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

// runOptions is what the run manager needs from the gateway around it.
type runOptions struct {
	handler http.Handler               // the gateway, which serves a run's inner request
	owner   func(*http.Request) string // whose runs a request may see (runOwner)
	// log and otel are read when they are used, as the server's fields were: a test may set them
	// after New.
	log                   func() *slog.Logger
	otel                  func() *otlp.Runtime
	dir                   string        // "" keeps runs in memory only
	maxHeld, maxHeldTotal int64         // 0: runMaxHeld and runsMaxHeld
	maxDuration           time.Duration // 0: runMaxDuration
}

type runManager struct {
	opts runOptions
	dir  string // "" keeps runs in memory only
	now  func() time.Time

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

func newRunManager(o runOptions) *runManager {
	m := &runManager{opts: o, dir: o.dir, now: time.Now, retention: runRetention, maxDuration: runMaxDuration,
		ping: runPing, stopWait: runStopWait, maxOutput: runMaxOutput, maxResult: runMaxResult, maxHeld: runMaxHeld, maxHeldTotal: runsMaxHeld, runs: map[string]*run{}, quit: make(chan struct{})}
	if o.maxHeld > 0 {
		m.maxHeld = o.maxHeld
	}
	if o.maxHeldTotal > 0 {
		m.maxHeldTotal = o.maxHeldTotal
	}
	if o.maxDuration > 0 {
		m.maxDuration = o.maxDuration
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
