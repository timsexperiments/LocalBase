package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// violation is one thing the gateway must not do.
type violation struct {
	Phase  string `json:"phase"`
	Class  string `json:"class"`
	Status int    `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail"`
	Count  int    `json:"count"`
}

// classStats accumulates one client class.
type classStats struct {
	N, OK, Refused429, Refused503, Other int
	Violations                           int
	Dur                                  []time.Duration // successful operations
	First                                []time.Duration // streams: first content
	PerWorker                            map[int]int     // successes
	Codes                                map[string]int
	PhaseOK                              map[string]int
	PhaseCodes                           map[string]map[string]int
}

// recorder classifies every result against what its phase allows.
type recorder struct {
	mu         sync.Mutex
	classes    map[string]*classStats
	violations map[string]*violation
	order      []string
}

func newRecorder() *recorder {
	return &recorder{classes: map[string]*classStats{}, violations: map[string]*violation{}}
}

func (r *recorder) stats(class string) *classStats {
	c := r.classes[class]
	if c == nil {
		c = &classStats{PerWorker: map[int]int{}, Codes: map[string]int{}, PhaseOK: map[string]int{}, PhaseCodes: map[string]map[string]int{}}
		r.classes[class] = c
	}
	return c
}

// violate records one violation, folding repeats of the same kind.
func (r *recorder) violate(phase, class string, status int, code, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violateLocked(phase, class, status, code, detail)
}

func (r *recorder) violateLocked(phase, class string, status int, code, detail string) {
	key := fmt.Sprintf("%s|%s|%d|%s|%s", phase, class, status, code, firstWords(detail))
	if v := r.violations[key]; v != nil {
		v.Count++
		return
	}
	r.violations[key] = &violation{Phase: phase, Class: class, Status: status, Code: code, Detail: detail, Count: 1}
	r.order = append(r.order, key)
}

func firstWords(s string) string {
	if len(s) > 48 {
		return s[:48]
	}
	return s
}

// add classifies a result. A code is allowed if the phase the request began in,
// the phase it ended in or the one before that allows it: a refusal can belong
// to the memory the request saw on arrival, and recovery takes a few samples.
func (r *recorder) add(class string, w int, res result, start, end, prev *phase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.stats(class)
	c.N++
	name := end.name
	allowed := func(code string) bool {
		return start.allows(code) || end.allows(code) || (prev != nil && prev.allows(code))
	}
	cutAllowed := start.cuts || end.cuts || (prev != nil && prev.cuts)
	switch {
	case res.Err != nil && (start.drain || end.drain):
		// The gateway is going away: a closed connection is its refusal.
		c.Refused503++
		c.Codes["connection_closed"]++
	case res.Err != nil:
		c.Other++
		c.Violations++
		r.violateLocked(name, class, 0, "transport", res.Err.Error())
	case res.Cut && cutAllowed:
		// A runtime stopped for critical memory may end a stream; that is the
		// documented refusal, not a success.
		c.Refused503++
		c.Codes["stream_cut"]++
	case res.Invalid != "":
		c.Other++
		c.Violations++
		r.violateLocked(name, class, res.Status, res.Code, res.Invalid)
	case res.Status >= 200 && res.Status < 300:
		c.OK++
		c.Dur = append(c.Dur, res.Dur)
		if res.FirstToken > 0 {
			c.First = append(c.First, res.FirstToken)
		}
		c.PerWorker[w]++
		c.PhaseOK[name]++
	case class == "unload" && (res.Code == "model_not_loaded" || res.Code == "model_busy"):
		c.OK++ // what an unload is allowed to say; the models come and go
		c.Dur = append(c.Dur, res.Dur)
		c.PerWorker[w]++
		c.PhaseOK[name]++
	case res.Status == 429:
		c.Refused429++
		c.Codes[res.Code]++
		r.notePhaseCode(c, name, res.Code)
		switch {
		case res.Code != "inference_queue_full" && res.Code != "inference_queue_timeout" && res.Code != "too_many_runs":
			c.Violations++
			r.violateLocked(name, class, 429, res.Code, "a 429 with an undocumented code")
		case res.RetryAfter == "":
			c.Violations++
			r.violateLocked(name, class, 429, res.Code, "a 429 without Retry-After")
		}
	case res.Status >= 500:
		c.Refused503++
		c.Codes[res.Code]++
		r.notePhaseCode(c, name, res.Code)
		switch {
		case res.Status != 503 || !allowed(res.Code):
			c.Violations++
			r.violateLocked(name, class, res.Status, res.Code, fmt.Sprintf("not a documented refusal in this phase (began in %s): %.300s", start.name, res.Body))
		case res.RetryAfter == "":
			c.Violations++
			r.violateLocked(name, class, res.Status, res.Code, "a 503 without Retry-After")
		}
	default:
		c.Other++
		c.Violations++
		r.violateLocked(name, class, res.Status, res.Code, fmt.Sprintf("unexpected status: %.300s", res.Body))
	}
}

func (r *recorder) notePhaseCode(c *classStats, phase, code string) {
	if c.PhaseCodes[phase] == nil {
		c.PhaseCodes[phase] = map[string]int{}
	}
	c.PhaseCodes[phase][code]++
}

// phaseOK is how many operations of class succeeded in phase.
func (r *recorder) phaseOK(class, phase string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats(class).PhaseOK[phase]
}

// phaseCode is how many operations of class ended with code in phase.
func (r *recorder) phaseCode(class, phase, code string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats(class).PhaseCodes[phase][code]
}

func (r *recorder) clearPhase(phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.classes {
		delete(c.PhaseOK, phase)
		delete(c.PhaseCodes, phase)
	}
}

func (r *recorder) violationList() []*violation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*violation, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.violations[k])
	}
	return out
}

// row is one line of the result table.
type row struct {
	Class      string         `json:"class"`
	N          int            `json:"n"`
	OK         int            `json:"ok"`
	Refused429 int            `json:"refused429"`
	Refused503 int            `json:"refused503"`
	Violations int            `json:"violations"`
	P50        time.Duration  `json:"p50"`
	P95        time.Duration  `json:"p95"`
	P99        time.Duration  `json:"p99"`
	Max        time.Duration  `json:"max"`
	FirstP50   time.Duration  `json:"firstP50,omitempty"`
	FirstP99   time.Duration  `json:"firstP99,omitempty"`
	Codes      map[string]int `json:"codes,omitempty"`
	// WorkerSpread is the least over the most successes among a class's workers.
	WorkerSpread float64 `json:"workerSpread"`
}

func (r *recorder) rows() []row {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.classes))
	for n := range r.classes {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]row, 0, len(names))
	for _, n := range names {
		c := r.classes[n]
		dur, first := lab.Sorted(c.Dur), lab.Sorted(c.First)
		row := row{Class: n, N: c.N, OK: c.OK, Refused429: c.Refused429, Refused503: c.Refused503, Violations: c.Violations,
			P50: lab.Percentile(dur, 50), P95: lab.Percentile(dur, 95), P99: lab.Percentile(dur, 99), Max: lab.Percentile(dur, 100),
			FirstP50: lab.Percentile(first, 50), FirstP99: lab.Percentile(first, 99), Codes: c.Codes}
		lo, hi := -1, 0
		for _, k := range c.PerWorker {
			if lo < 0 || k < lo {
				lo = k
			}
			hi = max(hi, k)
		}
		if hi > 0 {
			row.WorkerSpread = float64(lo) / float64(hi)
		}
		out = append(out, row)
	}
	return out
}

func codeList(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
