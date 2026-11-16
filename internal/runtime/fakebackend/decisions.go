package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// fakeDecision answers a question in its type's shape. The state chooses how
// sure it is, so a test can reach every word: "fake:close", "fake:unsure",
// "fake:leaning", and "fake:slow" takes a while. A yes-or-no question stays at
// even odds unless told otherwise.
func fakeDecision(q map[string]any, state string) map[string]any {
	if strings.Contains(state, "fake:slow") {
		time.Sleep(3 * time.Second)
	}
	odds := []float64{0.91, 0.06, 0.03}
	noul := 0.5
	switch {
	case strings.Contains(state, "fake:close"):
		odds, noul = []float64{0.52, 0.44, 0.04}, 0.55
	case strings.Contains(state, "fake:unsure"):
		odds = []float64{0.41, 0.33, 0.26}
	case strings.Contains(state, "fake:leaning"):
		odds, noul = []float64{0.68, 0.22, 0.10}, 0.7
	case strings.Contains(state, "fake:strong"):
		noul = 0.95
	}
	share := func(i, n int) float64 {
		if n <= len(odds) {
			if i < n-1 {
				return odds[i]
			}
			rest := 1.0
			for _, p := range odds[:n-1] {
				rest -= p
			}
			return rest
		}
		if i == 0 {
			return odds[0]
		}
		return (1 - odds[0]) / float64(n-1)
	}
	switch q["type"] {
	case "choice":
		crit, _ := q["criteria"].(map[string]any)
		names := make([]string, 0, len(crit))
		for name := range crit {
			names = append(names, name)
		}
		sort.Strings(names)
		probs := map[string]any{}
		for i, name := range names {
			probs[name] = share(i, len(names))
		}
		choice := ""
		if len(names) > 0 {
			choice = names[0]
		}
		return map[string]any{"type": "choice", "choice": choice, "probabilities": probs, "confidence": share(0, len(names))}
	case "score":
		crit, _ := q["criteria"].([]any)
		probs, legend := map[string]any{}, map[string]any{}
		for i, level := range crit {
			probs[strconv.Itoa(i)] = share(i, len(crit))
			legend[strconv.Itoa(i)] = level
		}
		return map[string]any{"type": "score", "score": 0, "probabilities": probs, "confidence": share(0, len(crit)), "legend": legend}
	}
	return map[string]any{"type": "noul", "noul": noul}
}
