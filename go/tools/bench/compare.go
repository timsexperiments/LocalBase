package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// budgets are limits on a developer laptop-class machine, set well above what was
// measured so that they mean something has really changed. docs/performance.md has
// the measurements behind them. A budget is only reported; what fails a nightly run
// is a regression against a baseline from the same kind of machine.
var budgets = map[string]float64{
	"startup, existing root":          500,
	"startup, new root":               1000,
	"idle RSS":                        128,
	"idle goroutines":                 40,
	"idle file descriptors":           40,
	"added per request, p50":          5,
	"added per request, p99":          25,
	"added per streamed token, p50":   100,
	"gateway CPU per request":         1000,
	"gateway CPU per streamed token":  100,
	"added to first token, p50":       5,
	"added to first token, p99":       25,
	"memory per idle connection, RSS": 64,
	"goroutines per idle connection":  2,
	"auth with 1000 keys, p50":        15,
	"auth with 1000 keys, p99":        50,
	"read with 4 writers, p99":        250,
	"failed writes with 4 writers":    0.5,
}

// A figure regresses only past both margins: twice the baseline, and more than an
// absolute floor for its unit, since a small figure on a shared machine can double
// on noise alone.
const regressionRatio = 2.0

var regressionFloor = map[string]float64{"ms": 5, "µs": 50, "MiB": 32, "KiB": 64, "count": 8}

func compareResults(base, now result) []string {
	if base.Platform != now.Platform {
		return []string{fmt.Sprintf("the baseline is from %s and this run is on %s: not comparable", base.Platform, now.Platform)}
	}
	old := map[string]metric{}
	for _, m := range base.Metrics {
		old[m.Name] = m
	}
	var out []string
	for _, m := range now.Metrics {
		was, ok := old[m.Name]
		if !ok {
			continue
		}
		if strings.Contains(m.Unit, "higher is better") {
			// Throughput regresses by falling to under half.
			if was.Value > 0 && m.Value < was.Value/regressionRatio {
				out = append(out, fmt.Sprintf("%s: %.2f, baseline %.2f %s", m.Name, m.Value, was.Value, m.Unit))
			}
			continue
		}
		if m.Value > was.Value*regressionRatio && m.Value-was.Value > regressionFloor[m.Unit] {
			out = append(out, fmt.Sprintf("%s: %.2f %s, baseline %.2f", m.Name, m.Value, m.Unit, was.Value))
		}
	}
	return out
}

func commitOf(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// getLoad reads the one-minute load average; it reports how busy the machine was,
// and nothing depends on it.
func getLoad(la *[3]float64) (int, error) {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return 0, err
	}
	s := string(out)
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return 0, fmt.Errorf("no load in %q", s)
	}
	fields := strings.Fields(strings.ReplaceAll(s[i+1:], ",", " "))
	if len(fields) == 0 {
		return 0, fmt.Errorf("no load in %q", s)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	la[0] = v
	return 1, nil
}
