package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

const (
	queueCapacity = 12
	queueTimeout  = 6 * time.Second
	requestLimit  = 90 * time.Second
	// fakeDelay is the fake backend's time per completion.
	fakeDelay = 20 * time.Millisecond
)

// The fake machine's memory: 64 GiB, 9.6 GiB reserve, constrained below 12 GiB.
const (
	ampleGiB       = 48.0
	tightGiB       = 17.0 // the small models fit (6.5 GiB above the 9.6 GiB reserve); the large chat and decision models do not
	constrainedGiB = 11.0
	criticalGiB    = 8.0
)

// The documented refusals (DESIGN §13.6, §23): each phase allows only what its
// memory and actions can cause. A 503 outside its phase is a bug.
const (
	codeMemory    = "insufficient_memory"
	codeUnloading = "model_unloading"
	codeEmergency = "service_unavailable"
	codeStopping  = "service_unavailable"
)

// phase is one stretch of a cycle.
type phase struct {
	name     string
	dur      time.Duration
	avail    float64
	pressure string
	allow    map[string]bool
	cuts     bool // streams may end early: a runtime stopped under them
	// drain tolerates connection errors: the gateway is going away.
	drain bool
}

func (p *phase) allows(code string) bool { return p != nil && p.allow[code] }

func allow(codes ...string) map[string]bool {
	m := map[string]bool{}
	for _, c := range codes {
		m[c] = true
	}
	return m
}

func warmPhase() *phase {
	return &phase{name: "warm", dur: 6 * time.Second, avail: ampleGiB, pressure: "normal"}
}

// cyclePhases is one cycle, in order. Odd cycles reach pressure through low
// availability, even ones through the host's own pressure reading.
func cyclePhases(cycle int) []*phase {
	constrained, critical := &phase{avail: constrainedGiB, pressure: "normal"}, &phase{avail: criticalGiB, pressure: "normal"}
	if cycle%2 == 1 {
		constrained.avail, constrained.pressure = ampleGiB, "constrained"
		critical.avail, critical.pressure = ampleGiB, "critical"
	}
	constrained.name, constrained.dur, constrained.allow = "constrained", 10*time.Second, allow(codeMemory)
	critical.name, critical.dur, critical.allow, critical.cuts = "critical", 8*time.Second, allow(codeMemory, codeEmergency), true
	return []*phase{
		{name: "steady", dur: 14 * time.Second, avail: ampleGiB, pressure: "normal"},
		{name: "burst", dur: 14 * time.Second, avail: ampleGiB, pressure: "normal"},
		{name: "unload", dur: 12 * time.Second, avail: ampleGiB, pressure: "normal", allow: allow(codeUnloading)},
		{name: "tight", dur: 14 * time.Second, avail: tightGiB, pressure: "normal", allow: allow(codeMemory)},
		{name: "blind", dur: 6 * time.Second, avail: -1, pressure: "unknown", allow: allow(codeMemory)},
		constrained,
		critical,
		{name: "recovery", dur: 12 * time.Second, avail: ampleGiB, pressure: "normal", allow: allow(codeMemory, codeEmergency), cuts: true},
	}
}

// cycleLength is one cycle with its resource checkpoint.
func cycleLength() time.Duration {
	d := 5 * time.Second
	for _, p := range cyclePhases(0) {
		d += p.dur
	}
	return d
}

// soak is one run.
type soak struct {
	g2 *lab.Gateway
	// commit is what this run was built from.
	commit string
	// tests counts how many times each burst test ran to its end.
	tests   map[string]int
	o       options
	w       *lab.Workspace
	g       *lab.Gateway
	c       *lab.Client
	rec     *recorder
	gate    *gate
	tail    *lab.LogTail
	cycles  int
	began   time.Time
	ended   time.Time
	checks  []check
	points  []checkpoint
	phases  []phaseRun
	drain   drainResult
	cpu     cpuUse
	mu      sync.Mutex
	cur     atomic.Pointer[phase]
	prev    atomic.Pointer[phase]
	tracked map[string]int // request id -> sequence, for the FIFO check
	order   []int
	fatals  []string
	errs    map[string]int
	leaked  []string
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type phaseRun struct {
	Cycle int       `json:"cycle"`
	Name  string    `json:"name"`
	Began time.Time `json:"began"`
}

type checkpoint struct {
	Label string    `json:"label"`
	Stats lab.Stats `json:"stats"`
}

func newSoak(ctx context.Context, w *lab.Workspace, o options, cycles int) (*soak, error) {
	g, err := w.Start(ctx, lab.Options{QueueCapacity: queueCapacity, QueueTimeoutMS: int(queueTimeout / time.Millisecond), Parallel: "1",
		FakeDelayMS: int(fakeDelay / time.Millisecond), CtxSize: 8192})
	if err != nil {
		return nil, err
	}
	s := &soak{o: o, w: w, g: g, c: lab.NewClient(g.Base), rec: newRecorder(), gate: newGate(), cycles: cycles,
		tracked: map[string]int{}, errs: map[string]int{}, tests: map[string]int{}}
	s.commit = gitCommit()
	s.tail = s.newTail(g)
	return s, nil
}

func (s *soak) newTail(g *lab.Gateway) *lab.LogTail {
	secrets := append([]string{canary, g.Keys.User, g.Keys.Admin}, g.Keys.Runs...)
	return &lab.LogTail{Path: g.LogPath,
		Keep: func(e lab.Event) bool {
			switch e.Name {
			case "runtime.starting", "runtime.memory-admission-rejected", "runtime.memory-pressure-changed", "gateway.stopped", "gateway.stopping":
				return true
			}
			return false
		},
		Line: func(line string) {
			for i, secret := range secrets {
				if strings.Contains(line, secret) {
					s.mu.Lock()
					what := "an API key"
					if i == 0 {
						what = "prompt text"
					}
					s.leaked = append(s.leaked, what)
					s.mu.Unlock()
				}
			}
		},
		Seen: func(e lab.Event) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if (e.Severity == "error" || e.Severity == "fatal") && !expectedError(e) {
				s.errs[e.Name]++
			}
			if e.Name == "http.panic" {
				s.fatals = append(s.fatals, e.Name+": "+e.Message)
			}
			if e.Name == "inference.completed" {
				if seq, ok := s.tracked[e.RequestID]; ok {
					s.order = append(s.order, seq)
				}
			}
		},
		Raw: func(line string) {
			if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") || strings.HasPrefix(line, "WARNING: DATA RACE") {
				s.mu.Lock()
				s.fatals = append(s.fatals, line)
				s.mu.Unlock()
			}
		}}
}

// expectedError is an error-level event the schedule causes: a refusal the
// clients already judged, a stream the drain or an emergency cut, and critical
// memory itself.
func expectedError(e lab.Event) bool {
	switch e.Name {
	case "http.request":
		return e.HTTP.Status == 503
	case "runtime.memory-pressure-changed":
		return e.Attr("current_state") == "critical"
	case "inference.completed":
		switch e.Attr("terminal_source") {
		case "memory_emergency", "response_stream_error":
			return true
		}
		status, _ := e.Num("http_status")
		return status == 503 || status == 429
	}
	return false
}

func (s *soak) current() *phase  { return s.cur.Load() }
func (s *soak) previous() *phase { return s.prev.Load() }

func (s *soak) addCheck(name string, ok bool, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

func (s *soak) enter(cycle int, p *phase) {
	if err := s.g.Fake.Set(p.avail, p.pressure); err != nil {
		s.addCheck("fake host", false, "%v", err)
	}
	if prev := s.cur.Load(); prev != nil {
		s.prev.Store(prev)
	}
	s.rec.clearPhase(p.name)
	s.gate.phaseName.Store(&p.name)
	s.cur.Store(p)
	s.mu.Lock()
	s.phases = append(s.phases, phaseRun{Cycle: cycle, Name: p.name, Began: time.Now()})
	s.mu.Unlock()
	fmt.Fprintf(os.Stderr, "soak: %s cycle %d: %s (%.0f GiB, %s)\n", time.Now().Format("15:04:05"), cycle, p.name, p.avail, p.pressure)
}

// execute is the whole run.
func (s *soak) execute(ctx context.Context) {
	s.began = time.Now()
	trafficCtx, stopTraffic := context.WithCancel(ctx)
	defer stopTraffic()
	warm := warmPhase()
	s.enter(0, warm)
	traffic := s.startTraffic(trafficCtx)
	sleepCtx(ctx, warm.dur)
	s.resourceCheckpoint(ctx, "after warm-up")
	for cycle := 0; cycle < s.cycles && ctx.Err() == nil; cycle++ {
		for _, p := range cyclePhases(cycle) {
			phaseEnd := time.Now().Add(p.dur)
			s.enter(cycle, p)
			s.phaseAction(ctx, p)
			sleepCtx(ctx, time.Until(phaseEnd))
			s.verifyPhase(cycle, p)
			s.readLog()
		}
		s.resourceCheckpoint(ctx, fmt.Sprintf("after cycle %d", cycle+1))
	}
	s.cpu = s.measureCPU()
	s.drainAndRestart(ctx, stopTraffic, traffic)
	s.ended = time.Now()
	s.readLog()
	s.finalChecks()
}

// phaseAction runs what a phase tests beyond its traffic. It must fit the
// phase's time or the phase simply runs longer.
func (s *soak) phaseAction(ctx context.Context, p *phase) {
	switch p.name {
	case "burst":
		s.burst(ctx)
	case "critical":
		// Every runtime stops within two pressure samples and a stop.
		deadline := time.Now().Add(p.dur - time.Second)
		var left []lab.Proc
		for time.Now().Before(deadline) && ctx.Err() == nil {
			left, _ = s.w.Backends()
			if len(left) == 0 {
				return
			}
			sleepCtx(ctx, 100*time.Millisecond)
		}
		s.addCheck("critical memory stops every runtime", false, "still running after %v: %v", p.dur-time.Second, procNames(left))
	}
}

func procNames(procs []lab.Proc) string {
	var names []string
	for _, p := range procs {
		names = append(names, fmt.Sprintf("%s(%d)", p.Command, p.PID))
	}
	return strings.Join(names, ", ")
}

// verifyPhase checks the phase did what it is for, beyond the allow-list every
// result already went through.
// readLog brings the log's events up to date. The checks judge by them, so a log that
// cannot be read fails the run rather than passing it on less evidence.
func (s *soak) readLog() {
	if err := s.tail.Read(); err != nil {
		s.rec.violate("log", "", 0, "", "could not read the gateway's log: "+err.Error())
	}
}

func (s *soak) verifyPhase(cycle int, p *phase) {
	want := func(class, why string) {
		if s.rec.phaseOK(class, p.name) == 0 {
			s.rec.violate(p.name, class, 0, "", fmt.Sprintf("cycle %d: no successes: %s", cycle, why))
		}
	}
	switch p.name {
	case "steady":
		for _, c := range []string{"chat", "chat-stream", "chat-large", "embeddings", "decisions", "images", "speech", "transcription", "runs", "runs-image", "control"} {
			want(c, "ample memory serves every class")
		}
	case "unload":
		want("unload", "unloads ran")
		want("chat", "requests go on loading what was unloaded")
	case "tight":
		// Models that fit keep working. A refused large model may hold the queues
		// while it waits for memory, and this host never frees any, so a class may
		// see only queue timeouts; but one refused for memory and never served has
		// a real problem. Embeddings run too rarely to require a result at all.
		for _, c := range []string{"chat", "embeddings", "images", "speech", "transcription"} {
			ok, refused, stalled := s.rec.phaseOK(c, p.name), s.rec.phaseCode(c, p.name, codeMemory), s.rec.phaseCode(c, p.name, "inference_queue_timeout")
			switch {
			case ok == 0 && refused > 0:
				s.rec.violate(p.name, c, 0, codeMemory, fmt.Sprintf("cycle %d: a model that fits was refused %d times and never served", cycle, refused))
			case ok == 0 && stalled == 0 && c != "embeddings":
				s.rec.violate(p.name, c, 0, "", fmt.Sprintf("cycle %d: no successes and no queue timeouts in tight memory", cycle))
			}
		}
		want("control", "health and lists answer in tight memory")
	case "blind":
		want("control", "health and lists answer without a memory reading")
	case "constrained":
		want("control", "health and lists answer under pressure")
	case "critical":
		want("control", "health and lists answer in an emergency")
	case "recovery":
		for _, c := range []string{"chat", "chat-stream", "embeddings", "images", "speech", "transcription", "control"} {
			want(c, "every class comes back after memory recovers")
		}
	}
}

// burst owns the LLM queue for a moment: FIFO order, exact queue-full refusals,
// the queue deadline and the per-owner run limit.
func (s *soak) burst(ctx context.Context) {
	s.gate.llm.Store(true)
	defer s.gate.llm.Store(false)
	if !settle(ctx, &s.gate.llmInflight, requestLimit) {
		s.addCheck("burst", false, "LLM clients did not settle")
		return
	}
	s.fifoAndFull(ctx)
	s.queueDeadline(ctx)
	s.runQuota(ctx)
	s.gate.image.Store(true)
	defer s.gate.image.Store(false)
	if settle(ctx, &s.gate.imageInflight, requestLimit) {
		s.imageQueueFull(ctx)
	}
}

func (s *soak) did(test string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tests[test]++
}

// loadSmall loads the small chat model and returns once it answers.
func (s *soak) loadSmall(ctx context.Context) bool {
	c := s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, "fake:reply loaded"))
	if c.Err != nil || c.Status != 200 {
		s.rec.violate("burst", "burst", c.Status, c.Code, fmt.Sprintf("loading the small model: %v %.200s", c.Err, c.Body))
		return false
	}
	return true
}

// holdSlot starts a slow stream and returns once it is generating; the channel
// yields its result.
func (s *soak) holdSlot(ctx context.Context, words int) (<-chan lab.Stream, bool) {
	body := chatBody(lab.ChatSmall, fmt.Sprintf("fake:slow %d", words))
	body["stream"] = true
	started := make(chan struct{})
	done := make(chan lab.Stream, 1)
	var once sync.Once
	go func() {
		st := s.c.ChatStream(ctx, s.g.Keys.User, body, func(*lab.Stream) bool {
			once.Do(func() { close(started) })
			return true
		})
		once.Do(func() { close(started) })
		done <- st
	}()
	select {
	case <-started:
		return done, true
	case <-ctx.Done():
		return done, false
	}
}

// queued is the LLM queue's depth as the gateway reports it for a loaded model.
func (s *soak) queued(ctx context.Context) int {
	c := s.c.Do(ctx, "GET", "/_localbase/models/"+lab.ChatSmall, s.g.Keys.User, "", nil)
	var m struct {
		Device struct {
			Runtime *struct {
				Queued *int `json:"queuedRequests"`
			} `json:"runtime"`
		} `json:"device"`
	}
	if c.Err != nil || c.Status != 200 || json.Unmarshal(c.Body, &m) != nil || m.Device.Runtime == nil || m.Device.Runtime.Queued == nil {
		return -1
	}
	return *m.Device.Runtime.Queued
}

// fifoAndFull fills the queue one request at a time, waiting on the gateway's
// own count, then checks refusals beyond capacity and the order of completion.
func (s *soak) fifoAndFull(ctx context.Context) {
	if !s.loadSmall(ctx) {
		return
	}
	done, ok := s.holdSlot(ctx, 60)
	if !ok {
		return
	}
	type answer struct {
		seq int
		c   lab.Call
	}
	answers := make(chan answer, queueCapacity)
	s.mu.Lock()
	s.tracked = map[string]int{}
	s.order = nil
	s.mu.Unlock()
	track := make(chan struct{}, queueCapacity)
	for seq := range queueCapacity {
		go func() {
			c := s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, fmt.Sprintf("fake:reply seq-%d", seq)))
			answers <- answer{seq, c}
			track <- struct{}{}
		}()
		deadline := time.Now().Add(2 * time.Second)
		for s.queued(ctx) != seq+1 {
			if time.Now().After(deadline) {
				s.rec.violate("burst", "fifo", 0, "", fmt.Sprintf("the queue never reached %d (it reports %d)", seq+1, s.queued(ctx)))
				break
			}
			sleepCtx(ctx, 2*time.Millisecond)
		}
	}
	// A full queue refuses at once, with its code and Retry-After.
	for range 3 {
		began := time.Now()
		c := s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, "over capacity"))
		if c.Status != 429 || c.Code != "inference_queue_full" || c.RetryAfter == "" || time.Since(began) > time.Second {
			s.rec.violate("burst", "queue-full", c.Status, c.Code, fmt.Sprintf("over capacity: Retry-After %q after %v: %.200s", c.RetryAfter, time.Since(began), c.Body))
		}
	}
	s.did("full queue refuses at once")
	// Readiness says the LLM cannot take more while it is full.
	if c := s.c.Do(ctx, "GET", "/health/ready", "", "", nil); c.Err == nil && strings.Contains(string(c.Body), `"llm"`) {
		s.rec.violate("burst", "queue-full", c.Status, "", "readiness lists the LLM while its queue is full: "+string(c.Body))
	}
	st := <-done
	if st.Err != nil || st.Status != 200 || !st.Done {
		s.rec.violate("burst", "fifo", st.Status, st.Code, fmt.Sprintf("the slot holder: %v", st.Err))
	}
	for range queueCapacity {
		a := <-answers
		want := fmt.Sprintf("seq-%d", a.seq)
		switch {
		case a.c.Err != nil || a.c.Status != 200:
			s.rec.violate("burst", "fifo", a.c.Status, a.c.Code, fmt.Sprintf("queued request %d: %v %.200s", a.seq, a.c.Err, a.c.Body))
		case !strings.Contains(string(a.c.Body), want):
			s.rec.violate("burst", "fifo", a.c.Status, "", fmt.Sprintf("queued request %d got another's answer: %.200s", a.seq, a.c.Body))
		}
		s.mu.Lock()
		s.tracked[a.c.RequestID] = a.seq
		s.mu.Unlock()
	}
	// The log has the true order: requests complete one at a time on one slot.
	s.fifoLogCheck()
}

// fifoLogCheck reads completion order from the log; the events were written
// before the responses that named their request ids arrived here, so read the
// whole file once with the ids known.
func (s *soak) fifoLogCheck() {
	s.mu.Lock()
	tracked := s.tracked
	s.mu.Unlock()
	var order []int
	tail := &lab.LogTail{Path: s.g.LogPath, Keep: func(lab.Event) bool { return false }, Seen: func(e lab.Event) {
		if e.Name == "inference.completed" {
			if seq, ok := tracked[e.RequestID]; ok {
				order = append(order, seq)
			}
		}
	}}
	if err := tail.Read(); err != nil {
		s.addCheck("FIFO order", false, "%v", err)
		return
	}
	bad := len(order) != queueCapacity
	for i := 1; i < len(order); i++ {
		bad = bad || order[i] != order[i-1]+1
	}
	if bad {
		s.rec.violate("burst", "fifo", 0, "", fmt.Sprintf("queued requests completed out of order: %v", order))
	}
	s.did("FIFO order")
}

// queueDeadline leaves requests queued past the deadline: 429 with its code.
func (s *soak) queueDeadline(ctx context.Context) {
	if !s.loadSmall(ctx) {
		return
	}
	holdCtx, release := context.WithCancel(ctx)
	defer release()
	words := int((queueTimeout+2*time.Second)/(50*time.Millisecond)) + 1
	if _, ok := s.holdSlot(holdCtx, words); !ok {
		return
	}
	began := time.Now()
	c := s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, "past the deadline"))
	took := time.Since(began)
	if c.Status != 429 || c.Code != "inference_queue_timeout" || c.RetryAfter == "" || took < queueTimeout-200*time.Millisecond || took > queueTimeout+1500*time.Millisecond {
		s.rec.violate("burst", "queue-deadline", c.Status, c.Code, fmt.Sprintf("after %v (deadline %v), Retry-After %q: %.200s", took, queueTimeout, c.RetryAfter, c.Body))
	}
	release()
	// Cancelling the holder frees the slot for what follows.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probe := s.c.PostJSON(ctx, "/v1/chat/completions", s.g.Keys.User, chatBody(lab.ChatSmall, "fake:reply free"))
		if probe.Status == 200 {
			s.did("queue deadline")
			return
		}
		sleepCtx(ctx, 50*time.Millisecond)
	}
	s.rec.violate("burst", "queue-deadline", 0, "", "a cancelled stream never gave its slot back")
}

// runQuota fills one owner's run limit: the next run is refused, and stopping
// them frees it.
func (s *soak) runQuota(ctx context.Context) {
	key := s.g.Keys.Runs[len(s.g.Keys.Runs)-1]
	var ids []string
	for i := range 5 {
		c := s.c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:slow 200"))
		switch {
		case i < 4 && c.Status != 202:
			s.rec.violate("burst", "run-quota", c.Status, c.Code, fmt.Sprintf("run %d of the limit refused: %.200s", i+1, c.Body))
		case i == 4 && (c.Status != 429 || c.Code != "too_many_runs" || c.RetryAfter == ""):
			s.rec.violate("burst", "run-quota", c.Status, c.Code, fmt.Sprintf("a run past the limit, Retry-After %q: %.200s", c.RetryAfter, c.Body))
		}
		var v runView
		if json.Unmarshal(c.Body, &v) == nil && v.ID != "" {
			ids = append(ids, v.ID)
		}
	}
	for _, id := range ids {
		c := s.c.PostJSON(ctx, "/_localbase/runs/"+id+"/cancel", key, "")
		var v runView
		if c.Status != 200 || json.Unmarshal(c.Body, &v) != nil || (v.Status != "cancelled" && v.Status != "completed") {
			s.rec.violate("burst", "run-quota", c.Status, c.Code, fmt.Sprintf("stopping a run: %.200s", c.Body))
		}
		s.c.Do(ctx, "DELETE", "/_localbase/runs/"+id, key, "", nil)
	}
	c := s.c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:reply freed"))
	if c.Status != 202 {
		s.rec.violate("burst", "run-quota", c.Status, c.Code, fmt.Sprintf("stopped runs did not free the limit: %.200s", c.Body))
		return
	}
	var v runView
	if json.Unmarshal(c.Body, &v) == nil && v.ID != "" {
		s.c.Do(ctx, "DELETE", "/_localbase/runs/"+v.ID, key, "", nil)
	}
	s.did("run limit")
}

// imageQueueFull overfills the image queue while one slow image holds it: the
// surplus is refused at once and the rest succeed.
func (s *soak) imageQueueFull(ctx context.Context) {
	if c := s.c.PostJSON(ctx, "/v1/images/generations", s.g.Keys.User, imageBody()); c.Status != 200 {
		s.rec.violate("burst", "image-queue", c.Status, c.Code, fmt.Sprintf("loading the image model: %.200s", c.Body))
		return
	}
	slow := imageBody()
	slow["prompt"] = "fake:slow-image"
	holder := make(chan lab.Call, 1)
	go func() { holder <- s.c.PostJSON(ctx, "/v1/images/generations", s.g.Keys.User, slow) }()
	sleepCtx(ctx, 150*time.Millisecond)
	const surplus = 3
	results := make(chan lab.Call, queueCapacity+surplus)
	for range queueCapacity + surplus {
		go func() { results <- s.c.PostJSON(ctx, "/v1/images/generations", s.g.Keys.User, imageBody()) }()
	}
	refused, served := 0, 0
	for range queueCapacity + surplus {
		c := <-results
		switch {
		case c.Status == 429 && c.Code == "inference_queue_full" && c.RetryAfter != "":
			refused++
		case c.Status == 200:
			served++
		default:
			s.rec.violate("burst", "image-queue", c.Status, c.Code, fmt.Sprintf("%v %.200s", c.Err, c.Body))
		}
	}
	<-holder
	if refused != surplus || served != queueCapacity {
		s.rec.violate("burst", "image-queue", 0, "", fmt.Sprintf("%d refused and %d served, expected %d and %d", refused, served, surplus, queueCapacity))
	}
	s.did("image queue")
}

// resourceCheckpoint pauses all traffic, unloads every model and samples the
// gateway alone, so two checkpoints compare like with like.
func (s *soak) resourceCheckpoint(ctx context.Context, label string) {
	s.gate.all.Store(true)
	defer s.gate.all.Store(false)
	if !settle(ctx, &s.gate.inflight, requestLimit) {
		s.addCheck("clients settle", false, "%s: requests still running after %v", label, requestLimit)
		return
	}
	for _, id := range unloadTargets {
		c := s.c.Do(ctx, "POST", "/_localbase/models/"+id+"/unload", s.g.Keys.Admin, "", nil)
		if c.Err != nil || (c.Status != 200 && c.Code != "model_not_loaded") {
			s.rec.violate(s.current().name, "checkpoint", c.Status, c.Code, fmt.Sprintf("unloading %s: %v %.200s", id, c.Err, c.Body))
		}
	}
	var left []lab.Proc
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		left, _ = s.w.Backends()
		if len(left) == 0 {
			break
		}
		sleepCtx(ctx, 100*time.Millisecond)
	}
	if len(left) > 0 {
		s.rec.violate(s.current().name, "checkpoint", 0, "", fmt.Sprintf("%s: backends still running with every model unloaded: %s", label, procNames(left)))
	}
	s.c.CloseIdle()
	sleepCtx(ctx, 500*time.Millisecond)
	st, err := s.g.Settled(ctx)
	if err != nil {
		s.addCheck("resource sample", false, "%s: %v", label, err)
		return
	}
	s.mu.Lock()
	s.points = append(s.points, checkpoint{Label: label, Stats: st})
	n := len(s.points)
	s.mu.Unlock()
	_ = s.g.GoroutineDump(ctx, filepath.Join(s.o.out, fmt.Sprintf("goroutines-%02d.txt", n)))
	if zombies := lab.Zombies(s.g.Cmd.Process.Pid); zombies > 0 {
		s.rec.violate(s.current().name, "checkpoint", 0, "", fmt.Sprintf("%s: %d unreaped children", label, zombies))
	}
}

type cpuUse struct {
	GatewaySeconds float64 `json:"gatewaySeconds"`
	HarnessSeconds float64 `json:"harnessSeconds"`
	AverageCores   float64 `json:"averageCores"`
}

func (s *soak) measureCPU() cpuUse {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru) // a figure for the report; nothing is judged on it
	harness := float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	gw := lab.CPUSeconds(s.g.Cmd.Process.Pid)
	u := cpuUse{GatewaySeconds: gw, HarnessSeconds: harness}
	if d := time.Since(s.began).Seconds(); d > 0 {
		u.AverageCores = (gw + harness) / d
	}
	return u
}
