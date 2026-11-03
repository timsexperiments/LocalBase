package main

import (
	"strings"
	"testing"
)

func metrics(pairs ...any) result {
	r := result{Platform: "test/os"}
	for i := 0; i+2 < len(pairs)+1 && i+2 < len(pairs); i += 3 {
		r.Metrics = append(r.Metrics, metric{Name: pairs[i].(string), Unit: pairs[i+1].(string), Value: pairs[i+2].(float64)})
	}
	return r
}

func TestCompare(t *testing.T) {
	cases := []struct {
		name      string
		base, now result
		regressed bool
	}{
		{"unchanged", metrics("a", "ms", 10.0), metrics("a", "ms", 10.0), false},
		{"noise on a small figure", metrics("a", "ms", 1.0), metrics("a", "ms", 3.0), false},
		{"past both margins", metrics("a", "ms", 10.0), metrics("a", "ms", 25.0), true},
		{"past the ratio, not the floor", metrics("a", "MiB", 10.0), metrics("a", "MiB", 25.0), false},
		{"better", metrics("a", "ms", 25.0), metrics("a", "ms", 10.0), false},
		{"throughput falling by half", metrics("w", "per s (higher is better)", 100.0), metrics("w", "per s (higher is better)", 40.0), true},
		{"throughput rising", metrics("w", "per s (higher is better)", 100.0), metrics("w", "per s (higher is better)", 400.0), false},
		{"a measure new since the baseline", metrics(), metrics("a", "ms", 1000.0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compareResults(c.base, c.now); (len(got) > 0) != c.regressed {
				t.Errorf("regressions %v, expected any: %v", got, c.regressed)
			}
		})
	}
	other := result{Platform: "other/os"}
	if got := compareResults(other, result{Platform: "test/os"}); len(got) != 1 || !strings.Contains(got[0], "not comparable") {
		t.Errorf("another platform's baseline: %v", got)
	}
}

func TestMarkdownShowsRegressions(t *testing.T) {
	res := metrics("a", "ms", 25.0)
	res.Metrics[0].Budget = 5
	md := markdown(res, "base.json", []string{"a: 25 ms, baseline 10"})
	for _, want := range []string{"REGRESSIONS", "| a | 25.00 ms | 5 |", "- a: 25 ms, baseline 10"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if md := markdown(metrics("a", "ms", 1.0), "base.json", nil); !strings.Contains(md, "no regressions") {
		t.Errorf("a clean comparison:\n%s", md)
	}
}
