package main

import (
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// The soak is only worth running if it can fail, so these feed it what a broken
// gateway would do and require a violation.

func call(status int, code, retry string) result {
	return result{Call: lab.Call{Status: status, Code: code, RetryAfter: retry, Body: []byte("x")}}
}

func TestAllowList(t *testing.T) {
	steady := &phase{name: "steady"}
	tight := &phase{name: "tight", allow: allow(codeMemory)}
	critical := &phase{name: "critical", allow: allow(codeMemory, codeEmergency), cuts: true}
	drain := &phase{name: "drain", drain: true, cuts: true}
	cut := result{Cut: true, Invalid: "stream ended"}
	cut.Status = 200
	wrong := result{Invalid: "answered by the wrong model"}
	wrong.Status = 200
	transport := result{}
	transport.Err = errString("connection reset")
	cases := []struct {
		name       string
		class      string
		r          result
		at         *phase
		violations int
	}{
		{"a success", "chat", call(200, "", ""), steady, 0},
		{"a refusal with no business in ample memory", "chat", call(503, codeMemory, "5"), steady, 1},
		{"a memory refusal in tight memory", "chat", call(503, codeMemory, "5"), tight, 0},
		{"a 503 without Retry-After", "chat", call(503, codeMemory, ""), tight, 1},
		{"an emergency outside critical memory", "chat", call(503, codeEmergency, "5"), tight, 1},
		{"an emergency in critical memory", "chat", call(503, codeEmergency, "5"), critical, 0},
		{"a 500 even in critical memory", "chat", call(500, "server_error", "5"), critical, 1},
		{"a 502 even in critical memory", "chat", call(502, "upstream_error", "5"), critical, 1},
		{"a queue refusal", "chat", call(429, "inference_queue_full", "1"), steady, 0},
		{"a 429 without Retry-After", "chat", call(429, "inference_queue_full", ""), steady, 1},
		{"a 429 with an unknown code", "chat", call(429, "slow_down", "1"), steady, 1},
		{"a 400", "chat", call(400, "validation_failed", ""), steady, 1},
		{"an unload saying not loaded", "unload", call(404, "model_not_loaded", ""), steady, 0},
		{"another class saying not loaded", "chat", call(404, "model_not_loaded", ""), steady, 1},
		{"a wrong answer", "chat", wrong, steady, 1},
		{"a cut stream in ample memory", "chat-stream", cut, steady, 1},
		{"a cut stream in critical memory", "chat-stream", cut, critical, 0},
		{"a connection error", "chat", transport, steady, 1},
		{"a connection error in the drain", "chat", transport, drain, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := newRecorder()
			rec.add(c.class, 1, c.r, c.at, c.at, nil)
			if got := len(rec.violationList()); got != c.violations {
				t.Errorf("%d violations, expected %d: %+v", got, c.violations, rec.violationList())
			}
		})
	}
	t.Run("a refusal is allowed when the request began in a phase that causes it", func(t *testing.T) {
		rec := newRecorder()
		rec.add("chat", 1, call(503, codeMemory, "5"), tight, steady, nil)
		if len(rec.violationList()) != 0 {
			t.Errorf("%+v", rec.violationList())
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func testSoak(t *testing.T) *soak {
	t.Helper()
	fake := &lab.FakeHost{Path: t.TempDir() + "/host.json", Spec: lab.DefaultHost()}
	return &soak{g: &lab.Gateway{Fake: fake}, rec: newRecorder(), cycles: 1, errs: map[string]int{}, tracked: map[string]int{}, tests: map[string]int{}}
}

func failed(s *soak, name string) string {
	for _, c := range s.checks {
		if c.Name == name && !c.OK {
			return c.Detail
		}
	}
	return ""
}

const giB = float64(1 << 30)

func TestAdmissionCheck(t *testing.T) {
	starting := func(at time.Time, demand float64) lab.Event {
		return lab.Event{Name: "runtime.starting", Time: at, Attributes: map[string]any{"model_id": "m", "demand_bytes": demand * giB}}
	}
	t.Run("a start that fits", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(48, "normal")
		s.checkAdmissions([]lab.Event{starting(time.Now(), 20)})
		if d := failed(s, "memory admission matches the host"); d != "" {
			t.Error(d)
		}
	})
	t.Run("a start above what the host has", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(17, "normal")
		s.checkAdmissions([]lab.Event{starting(time.Now(), 20)})
		if !strings.Contains(failed(s, "memory admission matches the host"), "started") {
			t.Errorf("not caught: %+v", s.checks)
		}
	})
	t.Run("a start under pressure", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(48, "normal")
		now := time.Now()
		s.checkAdmissions([]lab.Event{
			{Name: "runtime.memory-pressure-changed", Time: now.Add(-5 * time.Second), Attributes: map[string]any{"current_state": "critical"}},
			starting(now, 1)})
		if !strings.Contains(failed(s, "memory admission matches the host"), "while memory was critical") {
			t.Errorf("not caught: %+v", s.checks)
		}
	})
	refusal := func(reason string, measured, effective, requested float64) lab.Event {
		return lab.Event{Name: "runtime.memory-admission-rejected", Time: time.Now(), Attributes: map[string]any{
			"reason": reason, "measured_available_bytes": measured * giB, "effective_available_bytes": effective * giB,
			"requested_bytes": requested * giB, "reserve_bytes": 9.6 * giB, "measured_pressure": "normal", "safety_state": "healthy"}}
	}
	t.Run("a refusal that adds up", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(17, "normal")
		s.checkAdmissions([]lab.Event{refusal("system-memory", 17, 17, 21)})
		if d := failed(s, "memory admission matches the host"); d != "" {
			t.Error(d)
		}
	})
	t.Run("a refusal though it would fit", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(48, "normal")
		s.checkAdmissions([]lab.Event{refusal("system-memory", 48, 48, 2)})
		if !strings.Contains(failed(s, "memory admission matches the host"), "refused though") {
			t.Errorf("not caught: %+v", s.checks)
		}
	})
	t.Run("a pressure refusal with nothing wrong", func(t *testing.T) {
		s := testSoak(t)
		s.g.Fake.Set(48, "normal")
		s.checkAdmissions([]lab.Event{refusal("memory-pressure", 48, 48, 2)})
		if !strings.Contains(failed(s, "memory admission matches the host"), "nothing wrong") {
			t.Errorf("not caught: %+v", s.checks)
		}
	})
}

func TestHostRange(t *testing.T) {
	now := time.Now()
	h := []lab.HostSample{{At: now.Add(-10 * time.Second), AvailableGiB: 48}, {At: now.Add(-time.Second), AvailableGiB: 17}, {At: now.Add(time.Second), AvailableGiB: 8}}
	lo, hi := hostRange(h, now.Add(-2*time.Second), now)
	if lo != int64(17*giB) || hi != int64(48*giB) {
		t.Errorf("range %d..%d", lo, hi)
	}
}

func TestPressureCheck(t *testing.T) {
	change := func(from, to string) lab.Event {
		return lab.Event{Name: "runtime.memory-pressure-changed", Time: time.Now(), Attributes: map[string]any{"previous_state": from, "current_state": to}}
	}
	s := testSoak(t)
	s.checkPressure([]lab.Event{change("healthy", "critical"), change("critical", "healthy")})
	if failed(s, "pressure transitions follow the host") == "" {
		t.Errorf("a skipped state was not caught: %+v", s.checks)
	}
}

func TestResourceCheck(t *testing.T) {
	point := func(g, fds int, heap, rss int64) checkpoint {
		return checkpoint{Stats: lab.Stats{Goroutines: g, FDs: fds, HeapInuse: heap, RSS: rss}}
	}
	for _, c := range []struct {
		name  string
		last  checkpoint
		check string
	}{
		{"goroutines", point(100, 14, 10<<20, 50<<20), "goroutines bounded"},
		{"descriptors", point(10, 40, 10<<20, 50<<20), "file descriptors bounded"},
		{"heap", point(10, 14, 100<<20, 50<<20), "heap bounded"},
		{"rss", point(10, 14, 10<<20, 500<<20), "RSS bounded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testSoak(t)
			s.points = []checkpoint{point(10, 14, 10<<20, 50<<20), c.last}
			s.checkResources()
			if failed(s, c.check) == "" {
				t.Errorf("growth not caught: %+v", s.checks)
			}
		})
	}
	t.Run("a steady climb across a long run", func(t *testing.T) {
		s := testSoak(t)
		for i := range 12 {
			s.points = append(s.points, point(10+2*i, 14, 10<<20, 50<<20))
		}
		s.checkResources()
		if failed(s, "goroutines bounded") == "" {
			t.Errorf("growth not caught: %+v", s.checks)
		}
		if failed(s, "no climb in the second half: goroutines") == "" {
			t.Errorf("the climb not caught: %+v", s.checks)
		}
	})
}

func TestExpectedErrors(t *testing.T) {
	bad := lab.Event{Name: "http.request"}
	bad.HTTP.Status = 500
	if expectedError(bad) {
		t.Error("a 500 is not expected")
	}
	ok := lab.Event{Name: "http.request"}
	ok.HTTP.Status = 503
	if !expectedError(ok) {
		t.Error("a 503 is judged by the clients")
	}
	if expectedError(lab.Event{Name: "backend.start-failed"}) {
		t.Error("a failed backend start is not expected")
	}
	if !expectedError(lab.Event{Name: "runtime.memory-pressure-changed", Attributes: map[string]any{"current_state": "critical"}}) {
		t.Error("critical memory is the schedule's")
	}
}

func TestCycleFitsTheSmokeBudget(t *testing.T) {
	if d := cycleLength(); d > 2*time.Minute {
		t.Errorf("a cycle takes %v; the smoke promises about 2 minutes", d)
	}
}

func TestTrend(t *testing.T) {
	if got := slope([]float64{10, 12, 14, 16}); got != 2 {
		t.Errorf("slope of a line: %v", got)
	}
	if got := slope([]float64{5, 5, 5}); got != 0 {
		t.Errorf("slope of a flat line: %v", got)
	}
	point := func(rss int64, threads int) checkpoint {
		return checkpoint{Stats: lab.Stats{RSS: rss, Threads: threads, HeapInuse: 10 << 20, Goroutines: 10, FDs: 14}}
	}
	if fitTrend([]checkpoint{point(1, 1), point(2, 1), point(3, 1)}) != nil {
		t.Error("a trend over too few checkpoints")
	}
	// The warm-up checkpoint is left out: its jump is not a slope.
	got := fitTrend([]checkpoint{point(1<<30, 1), point(60<<20, 14), point(61<<20, 14), point(62<<20, 15), point(63<<20, 16)})
	if got == nil || got.Checkpoints != 4 || got.RSSBytesPerCycle != 1<<20 || got.HeapBytesPerCycle != 0 || got.GoroutinesPerCycle != 0 {
		t.Fatalf("%+v", got)
	}
	if got.ThreadsPerCycle < 0.6 || got.ThreadsPerCycle > 0.8 {
		t.Errorf("threads per cycle %v", got.ThreadsPerCycle)
	}
	var rep report
	rep.Trend = got
	if md := rep.markdown(); !strings.Contains(md, "RSS +1024 KiB") {
		t.Errorf("the report does not show the slope:\n%s", md)
	}
}

func TestDurationRoundsUpToWholeCycles(t *testing.T) {
	c := cycleLength()
	for _, tc := range []struct {
		d    time.Duration
		want int
	}{{0, 1}, {c, 1}, {c + time.Second, 2}, {30 * time.Minute, int((30*time.Minute + c - 1) / c)}} {
		if got := wholeCycles(tc.d); got != tc.want {
			t.Errorf("%v: %d cycles, expected %d", tc.d, got, tc.want)
		}
	}
	if wholeCycles(30*time.Minute)*int(c/time.Second) < 30*60 {
		t.Error("a 30-minute soak runs shorter than 30 minutes")
	}
}
