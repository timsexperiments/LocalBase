package diagnostics

import (
	"context"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil/busyread"
)

// TestRecoveringHelper is busyread.RecoveringElsewhere's helper process in this test binary.
func TestRecoveringHelper(t *testing.T) { busyread.RecoveringHelper(t) }

// And a peak read that fails partway, which used to be silent.
func TestDiagnosticModelsListsAFailedPeakRead(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, s := range cat.All() {
		if s.Primary().SHA256 != "" {
			spec = s
			break
		}
	}
	st, hold := busyread.Read(t)
	hold()
	problems := &readProblems{dir: t.TempDir()}
	cfg := &store.Config{Active: map[string]string{spec.Kind: spec.ModelID}, Selected: map[string][]string{spec.Kind: {spec.ModelID}}}
	diagnosticModels(context.Background(), problems.dir, st, cat, cfg, 64, problems)
	if len(problems.text) == 0 || !strings.Contains(strings.Join(problems.text, "\n"), "reading the memory peaks seen for "+spec.ModelID+": the database is busy") {
		t.Errorf("%v", problems.text)
	}
}
