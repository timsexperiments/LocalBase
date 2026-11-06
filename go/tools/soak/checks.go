package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// Budgets for growth between the first and the last checkpoint: the gateway
// with every model unloaded and no client connected. A race-detector gateway's
// memory is several times larger, so its figures get raceFactor.
//
// Descriptors have two parts. budgetFDs is for everything except the database's
// files, and it is no larger than it was. Those files fill as the connection pool
// does, which is not a leak and can add about 17 after warm-up, so they have a
// ceiling of their own, lab.DatabaseCeiling. See lab.DescriptorProblem.
const (
	budgetGoroutines = 12
	budgetFDs        = 8
	budgetHeap       = 32 << 20
	budgetRSS        = 96 << 20
	raceFactor       = 4
)

// logWindow is how far a logged decision can trail the sample it used.
const logWindow = time.Second

func (s *soak) finalChecks() {
	events := s.tail.Events()
	s.checkAdmissions(events)
	s.checkPressure(events)
	s.checkResources()
	s.mu.Lock()
	fatals, errs, leaked := s.fatals, s.errs, s.leaked
	s.mu.Unlock()
	s.addCheck("no panics or fatal errors", len(fatals) == 0, "%s", strings.Join(capStrings(fatals, 5), "; "))
	unexpected := map[string]int{}
	for name, n := range errs {
		unexpected[name] = n
	}
	s.addCheck("no unexpected error events", len(unexpected) == 0, "%s", codeList(unexpected))
	s.addCheck("no key or prompt text in the log", len(leaked) == 0, "%s", strings.Join(capStrings(leaked, 5), "; "))
	var reports []string
	for _, g := range []*lab.Gateway{s.g, s.g2} {
		if g != nil {
			reports = append(reports, g.RaceReports()...)
		}
	}
	if s.o.race {
		detail := "clean"
		if len(reports) > 0 {
			b, _ := os.ReadFile(reports[0])
			detail = fmt.Sprintf("%d report file(s); first:\n%.1500s", len(reports), b)
			copyEvidence(s.o.out, reports)
		}
		s.addCheck("no data races", len(reports) == 0, "%s", detail)
	}
	// A test that stopped early leaves a violation; these say each one really ran.
	for _, test := range []string{"FIFO order", "full queue refuses at once", "queue deadline", "run limit", "image queue"} {
		s.mu.Lock()
		n := s.tests[test]
		s.mu.Unlock()
		s.addCheck(test, n >= s.cycles, "%d of %d bursts", n, s.cycles)
	}
}

func copyEvidence(out string, paths []string) {
	for i, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if err := os.WriteFile(fmt.Sprintf("%s/race-%d.txt", out, i), b, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "soak: could not keep the race evidence:", err)
			}
		}
	}
}

func capStrings(in []string, n int) []string {
	if len(in) > n {
		return append(append([]string(nil), in[:n]...), fmt.Sprintf("and %d more", len(in)-n))
	}
	return in
}

// hostRange is the lowest and highest availability in effect during [from, to],
// in bytes. A changed reading counts from the moment it was written.
func hostRange(history []lab.HostSample, from, to time.Time) (lo, hi int64) {
	lo, hi = int64(1)<<62, -1<<62
	consider := func(g float64) {
		b := int64(g * (1 << 30))
		lo, hi = min(lo, b), max(hi, b)
	}
	in := -1
	for i, h := range history {
		if h.At.After(to) {
			break
		}
		if !h.At.After(from) {
			in = i
			continue
		}
		consider(h.AvailableGiB)
	}
	if in >= 0 {
		consider(history[in].AvailableGiB)
	}
	return lo, hi
}

// checkAdmissions holds the gateway's logged memory decisions against what the
// fake host said at the time: nothing started that could not fit above the
// reserve or while memory was not healthy, and every refusal's figures add up.
func (s *soak) checkAdmissions(events []lab.Event) {
	history := s.g.Fake.History()
	reserve := s.g.Fake.ReserveBytes()
	state := "healthy"
	var stateAt time.Time
	started, refused, bad := 0, 0, []string{}
	reasons := map[string]int{}
	for _, e := range events {
		switch e.Name {
		case "runtime.memory-pressure-changed":
			state, stateAt = e.Attr("current_state"), e.Time
		case "runtime.starting":
			started++
			demand, ok := e.Num("demand_bytes")
			if !ok {
				bad = append(bad, "runtime.starting without demand_bytes")
				continue
			}
			_, hi := hostRange(history, e.Time.Add(-logWindow), e.Time)
			if int64(demand)+reserve > hi {
				bad = append(bad, fmt.Sprintf("%s started %s needing %.1f GiB with at most %.1f GiB free (reserve %.1f)",
					e.Attr("model_id"), e.Time.Format("15:04:05.000"), demand/(1<<30), float64(hi)/(1<<30), float64(reserve)/(1<<30)))
			}
			if state != "healthy" && e.Time.Sub(stateAt) > logWindow {
				bad = append(bad, fmt.Sprintf("%s started %s while memory was %s", e.Attr("model_id"), e.Time.Format("15:04:05.000"), state))
			}
		case "runtime.memory-admission-rejected":
			refused++
			reasons[e.Attr("reason")]++
			if why := refusalProblem(e, history, reserve); why != "" {
				bad = append(bad, why)
			}
		}
	}
	s.addCheck("memory admission matches the host", len(bad) == 0, "%d starts, %d refusals checked. %s", started, refused, strings.Join(capStrings(bad, 5), "; "))
	s.addCheck("admission was exercised", started > 0 && refused > 0, "%d starts, %d refusals", started, refused)
	// Each cycle must reach these two. The log says so the moment a decision is
	// made; a client's answer can come up to a settle timeout later, in another
	// phase. An unmeasurable host gets too short a phase to require it.
	s.addCheck("memory refusals by reason", reasons["system-memory"] >= s.cycles && reasons["memory-pressure"] >= s.cycles, "%s", codeList(reasons))
}

func refusalProblem(e lab.Event, history []lab.HostSample, reserve int64) string {
	at := e.Time.Format("15:04:05.000")
	measured, measuredOK := e.Num("measured_available_bytes")
	effective, effectiveOK := e.Num("effective_available_bytes")
	requested, requestedOK := e.Num("requested_bytes")
	reserveAttr, _ := e.Num("reserve_bytes")
	lo, _ := hostRange(history, e.Time.Add(-logWindow), e.Time)
	switch e.Attr("reason") {
	case "system-memory":
		if !measuredOK || !effectiveOK || !requestedOK {
			return fmt.Sprintf("%s: system-memory refusal without figures: %v", at, e.Attributes)
		}
		if int64(reserveAttr) != reserve {
			return fmt.Sprintf("%s: refused with reserve %.0f, expected %d", at, reserveAttr, reserve)
		}
		if effective-requested >= reserveAttr {
			return fmt.Sprintf("%s: refused though %.1f GiB free covers %.1f GiB above the reserve", at, effective/(1<<30), requested/(1<<30))
		}
		// A releasing runtime's credit can only raise the sample.
		if int64(measured) < lo {
			return fmt.Sprintf("%s: measured %.1f GiB, below anything the host said (%.1f GiB)", at, measured/(1<<30), float64(lo)/(1<<30))
		}
	case "memory-pressure":
		if e.Attr("safety_state") == "healthy" && e.Attr("measured_pressure") == "normal" && measuredOK && measured >= 1.25*reserveAttr {
			return fmt.Sprintf("%s: a memory-pressure refusal with nothing wrong: %v", at, e.Attributes)
		}
	case "measurement-unavailable":
		if measuredOK && measured >= 0 && e.Attr("measured_pressure") != "unknown" {
			return fmt.Sprintf("%s: measurement-unavailable with a measurement: %v", at, e.Attributes)
		}
	case "accelerator-memory":
		// The working-set ledger; its figures are checked by unit tests.
	default:
		return fmt.Sprintf("%s: unknown refusal reason %q", at, e.Attr("reason"))
	}
	return ""
}

// checkPressure compares the log's transitions with the schedule: each cycle
// goes healthy, constrained, critical, healthy, at most a few samples late.
func (s *soak) checkPressure(events []lab.Event) {
	var got []string
	var times []time.Time
	for _, e := range events {
		if e.Name == "runtime.memory-pressure-changed" {
			got = append(got, e.Attr("previous_state")+">"+e.Attr("current_state"))
			times = append(times, e.Time)
		}
	}
	var want []string
	for range s.cycles {
		want = append(want, "healthy>constrained", "constrained>critical", "critical>healthy")
	}
	ok := strings.Join(got, " ") == strings.Join(want, " ")
	detail := fmt.Sprintf("%d transitions", len(got))
	if !ok {
		detail = "got " + strings.Join(capStrings(got, 9), " ")
	}
	s.addCheck("pressure transitions follow the host", ok, "%s", detail)
	if !ok {
		return
	}
	// Timing: within two samples of the change; recovery after three normal ones.
	var phases []phaseRun
	s.mu.Lock()
	phases = append(phases, s.phases...)
	s.mu.Unlock()
	lates := []string{}
	i := 0
	for _, p := range phases {
		if i >= len(times) {
			break
		}
		var lo, hi time.Duration
		switch p.Name {
		case "constrained", "critical":
			lo, hi = 0, 5*time.Second
		case "recovery":
			lo, hi = 3*time.Second, 10*time.Second
		default:
			continue
		}
		d := times[i].Sub(p.Began)
		if d < lo-logWindow || d > hi {
			lates = append(lates, fmt.Sprintf("cycle %d %s after %v (expected %v to %v)", p.Cycle, p.Name, d.Round(10*time.Millisecond), lo, hi))
		}
		i++
	}
	s.addCheck("pressure transitions on time", len(lates) == 0, "%s", strings.Join(capStrings(lates, 5), "; "))
}

// checkResources compares the first and last checkpoints against the budgets
// and, on longer runs, looks for a steady climb across all of them.
func (s *soak) checkResources() {
	s.mu.Lock()
	points := append([]checkpoint(nil), s.points...)
	s.mu.Unlock()
	if len(points) < 2 {
		s.addCheck("resources stay bounded", false, "only %d checkpoints", len(points))
		return
	}
	first, last := points[0].Stats, points[len(points)-1].Stats
	factor := int64(1)
	if s.o.race {
		factor = raceFactor
	}
	s.addCheck("goroutines bounded", last.Goroutines-first.Goroutines <= budgetGoroutines, "%d > %d (budget +%d)", first.Goroutines, last.Goroutines, budgetGoroutines)
	descriptors := lab.DescriptorProblem(first, last, budgetFDs, lab.DatabaseCeiling)
	s.addCheck("file descriptors bounded", descriptors == "" && first.FDs > 0, "%d to %d, with %d on the database's files: %s", first.FDs, last.FDs, last.Files.Total(), descriptors)
	s.addCheck("heap bounded", last.HeapInuse-first.HeapInuse <= budgetHeap*factor, "%.1f > %.1f MiB (budget +%d)", mib(first.HeapInuse), mib(last.HeapInuse), budgetHeap*factor>>20)
	s.addCheck("RSS bounded", last.RSS-first.RSS <= budgetRSS*factor, "%.1f > %.1f MiB (budget +%d)", mib(first.RSS), mib(last.RSS), budgetRSS*factor>>20)
	// Over many cycles a leak climbs at every checkpoint. Skip the first half:
	// caches and retained runs fill early, and retention then holds them level.
	if len(points) >= 8 {
		half := points[len(points)/2:]
		grow := func(get func(lab.Stats) int64, limit int64, name string) {
			if d := get(half[len(half)-1].Stats) - get(half[0].Stats); d > limit {
				s.addCheck("no climb in the second half: "+name, false, "+%d, limit %d", d, limit)
			} else {
				s.addCheck("no climb in the second half: "+name, true, "+%d", d)
			}
		}
		grow(func(st lab.Stats) int64 { return int64(st.Goroutines) }, budgetGoroutines/2, "goroutines")
		grow(func(st lab.Stats) int64 { return int64(st.FDs - st.Files.Total()) }, budgetFDs/2, "descriptors")
		grow(func(st lab.Stats) int64 { return st.HeapInuse }, budgetHeap*factor/2, "heap bytes")
	}
}

func mib(b int64) float64 { return float64(b) / (1 << 20) }
