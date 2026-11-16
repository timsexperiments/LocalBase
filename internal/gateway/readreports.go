package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// reportReads says a listing is missing something without changing what a client parses: a header,
// and an error event naming each step.
func (s *Server) reportReads(w http.ResponseWriter, problems []*store.ReadError) {
	var kinds []string
	for _, p := range problems {
		if !slices.Contains(kinds, p.Kind) {
			kinds = append(kinds, p.Kind)
		}
		s.cfg.Log.Error("A listing is missing something it couldn't read.", "event", "models.read-failed", "step", p.Step, "error", p.Err.Error())
	}
	if len(kinds) > 0 {
		w.Header().Set("X-Localbase-Warning", strings.Join(kinds, ", "))
	}
}
