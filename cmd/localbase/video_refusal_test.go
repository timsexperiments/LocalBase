package main

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
)

// The catalog's note and the eligibility check follow each model's targets. A model that can run
// somewhere gets neither.
func TestVideoCatalogLineAndIneligibleFollowTheTargets(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	nowhere := 0
	for _, s := range cat.All() {
		if s.VideoRuntime == nil {
			continue
		}
		if s.VideoRunsAnywhere() {
			if got := videoCatalogLine(s); got != "" {
				t.Errorf("%s can run, and the note says %q", s.ModelID, got)
			}
			continue
		}
		nowhere++
		if got := videoCatalogLine(s); got != catalog.VideoModelRefusal {
			t.Errorf("%s: note %q", s.ModelID, got)
		}
		if got := conf.Ineligible(s, true); got != catalog.VideoModelRefusal {
			t.Errorf("%s: ineligible %q", s.ModelID, got)
		}
	}
	if nowhere == 0 {
		t.Log("every catalog video model can run now, so the second text has nothing to say")
	}
}
