package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// report is one run's outcome, as markdown and JSON.
type report struct {
	Verdict    string        `json:"verdict"`
	Commit     string        `json:"commit"`
	Platform   string        `json:"platform"`
	Race       bool          `json:"race"`
	Cycles     int           `json:"cycles"`
	Seed       uint64        `json:"seed"`
	Duration   time.Duration `json:"duration"`
	Rows       []row         `json:"rows"`
	Points     []checkpoint  `json:"checkpoints"`
	Trend      *trend        `json:"trend,omitempty"`
	Checks     []check       `json:"checks"`
	Violations []*violation  `json:"violations"`
	Drain      drainResult   `json:"drain"`
	CPU        cpuUse        `json:"cpu"`
}

func (s *soak) report() *report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &report{Commit: s.commit, Platform: goruntime.GOOS + "/" + goruntime.GOARCH, Race: s.o.race, Cycles: s.cycles, Seed: s.o.seed,
		Duration: s.ended.Sub(s.began), Rows: s.rec.rows(), Points: s.points, Checks: s.checks, Violations: s.rec.violationList(), Drain: s.drain, CPU: s.cpu, Trend: fitTrend(s.points)}
	r.Verdict = "PASS"
	if r.failed() {
		r.Verdict = "FAIL"
	}
	return r
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (r *report) failed() bool {
	if len(r.Violations) > 0 {
		return true
	}
	for _, c := range r.Checks {
		if !c.OK {
			return true
		}
	}
	return false
}

func (r *report) write(dir string) (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "result.md")
	return path, os.WriteFile(path, []byte(r.markdown()), 0o600)
}

func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond))
}

// markdown is the one-page result.
func (r *report) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Soak: %s\n\n", r.Verdict)
	race := ""
	if r.Race {
		race = ", race detector on"
	}
	fmt.Fprintf(&b, "%s, commit `%s`, %d cycle(s), %v%s, seed %d. Fake backends, fake memory host.\n\n", r.Platform, r.Commit, r.Cycles, r.Duration.Round(time.Second), race, r.Seed)
	fmt.Fprintln(&b, "| client | ops | ok | 429 | 503 | viol. | p50 ms | p95 ms | p99 ms | max ms | first token p50/p99 ms | worker spread | refusals |")
	fmt.Fprintln(&b, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|")
	for _, x := range r.Rows {
		first := "-"
		if x.FirstP50 > 0 {
			first = ms(x.FirstP50) + " / " + ms(x.FirstP99)
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %s | %s | %s | %s | %s | %.2f | %s |\n", x.Class, x.N, x.OK, x.Refused429, x.Refused503, x.Violations,
			ms(x.P50), ms(x.P95), ms(x.P99), ms(x.Max), first, x.WorkerSpread, codeList(x.Codes))
	}
	fmt.Fprintln(&b, "\nLatency covers successes only; a detached run's figure is its whole life, from create to dismissal.")
	if len(r.Points) > 0 {
		fmt.Fprintln(&b, "\n| checkpoint (all models unloaded, no clients) | goroutines | fds | on the database | threads | heap MiB | RSS MiB |")
		fmt.Fprintln(&b, "|---|---:|---:|---:|---:|---:|---:|")
		for _, i := range checkpointRows(len(r.Points)) {
			p := r.Points[i]
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.1f | %.1f |\n", p.Label, p.Stats.Goroutines, p.Stats.FDs, p.Stats.Files.Total(), p.Stats.Threads, mib(p.Stats.HeapInuse), mib(p.Stats.RSS))
		}
	}
	if r.Trend != nil {
		fmt.Fprintf(&b, "\nTrend per cycle, least-squares fit over %d checkpoints after the first cycle: RSS %+.0f KiB, heap %+.0f KiB, threads %+.2f, goroutines %+.2f, fds %+.2f. Over a run this short a slope can be a plateau still settling; only a longer run tells them apart.\n",
			r.Trend.Checkpoints, r.Trend.RSSBytesPerCycle/1024, r.Trend.HeapBytesPerCycle/1024, r.Trend.ThreadsPerCycle, r.Trend.GoroutinesPerCycle, r.Trend.FDsPerCycle)
	}
	fmt.Fprintf(&b, "\nDrain under load: exit ok %v in %v, health while stopping %q, %d/3 short streams finished, long stream cut %v, restart healthy in %v.\n",
		r.Drain.ExitOK, r.Drain.Took.Round(time.Millisecond), r.Drain.HealthAfter, r.Drain.ShortStreams, r.Drain.LongCut, r.Drain.RestartTook.Round(time.Millisecond))
	fmt.Fprintf(&b, "CPU: gateway %.0f s, harness %.0f s, %.2f cores on average.\n\n", r.CPU.GatewaySeconds, r.CPU.HarnessSeconds, r.CPU.AverageCores)
	fmt.Fprintln(&b, "| check | result | detail |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, c := range r.Checks {
		res := "pass"
		if !c.OK {
			res = "**FAIL**"
		}
		detail := strings.ReplaceAll(strings.ReplaceAll(c.Detail, "\n", " "), "|", "\\|")
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, res, detail)
	}
	if len(r.Violations) > 0 {
		fmt.Fprintf(&b, "\n## Violations (%d kinds)\n\n", len(r.Violations))
		for i, v := range r.Violations {
			if i == 25 {
				fmt.Fprintf(&b, "- and %d more kinds; see result.json\n", len(r.Violations)-i)
				break
			}
			fmt.Fprintf(&b, "- %dx in %s, %s: status %d %s: %s\n", v.Count, v.Phase, v.Class, v.Status, v.Code, strings.ReplaceAll(v.Detail, "\n", " "))
		}
	}
	return b.String()
}

// checkpointRows keeps the table to a page: the first, the last and a few between.
func checkpointRows(n int) []int {
	const most = 6
	if n <= most {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, most)
	for i := range most {
		out = append(out, i*(n-1)/(most-1))
	}
	return out
}

// trend is each resource's change per cycle, a least-squares fit, so a slow leak
// shows as a number even while it stays inside the budgets. It leaves out the
// warm-up checkpoint: the first cycle is when caches and pools fill.
type trend struct {
	Checkpoints        int     `json:"checkpoints"`
	RSSBytesPerCycle   float64 `json:"rssBytesPerCycle"`
	HeapBytesPerCycle  float64 `json:"heapBytesPerCycle"`
	ThreadsPerCycle    float64 `json:"threadsPerCycle"`
	GoroutinesPerCycle float64 `json:"goroutinesPerCycle"`
	FDsPerCycle        float64 `json:"fdsPerCycle"`
}

// minTrendPoints is the fewest checkpoints a slope means anything over.
const minTrendPoints = 3

func fitTrend(points []checkpoint) *trend {
	if len(points) < minTrendPoints+1 {
		return nil
	}
	cycles := points[1:]
	over := func(get func(lab.Stats) float64) float64 {
		ys := make([]float64, len(cycles))
		for i, p := range cycles {
			ys[i] = get(p.Stats)
		}
		return slope(ys)
	}
	return &trend{Checkpoints: len(cycles),
		RSSBytesPerCycle:   over(func(s lab.Stats) float64 { return float64(s.RSS) }),
		HeapBytesPerCycle:  over(func(s lab.Stats) float64 { return float64(s.HeapInuse) }),
		ThreadsPerCycle:    over(func(s lab.Stats) float64 { return float64(s.Threads) }),
		GoroutinesPerCycle: over(func(s lab.Stats) float64 { return float64(s.Goroutines) }),
		FDsPerCycle:        over(func(s lab.Stats) float64 { return float64(s.FDs) })}
}

// slope is the least-squares slope of ys over 0, 1, 2, ...
func slope(ys []float64) float64 {
	n := float64(len(ys))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, y := range ys {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	return (n*sumXY - sumX*sumY) / (n*sumXX - sumX*sumX)
}
