package httpx

import "github.com/timsexperiments/LocalBase/go/internal/store"

// ReadProblemsJSON is what our own routes say about them.
func ReadProblemsJSON(problems []*store.ReadError) []map[string]string {
	out := make([]map[string]string, 0, len(problems))
	for _, p := range problems {
		out = append(out, map[string]string{"step": p.Step, "reason": p.Reason()})
	}
	return out
}
