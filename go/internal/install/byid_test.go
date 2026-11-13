package install

import (
	"context"
	"errors"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// ByID refuses an unknown id and an ineligible model in their own words, before anything is
// downloaded, and never as an *InstallFailed (which is for what Model says). The eligibility
// judgement is the caller's.
func TestByIDRefusesWhatIsNotThere(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var judged *catalog.Spec
	d := ByIDDeps{Catalog: cat, Ineligible: func(s *catalog.Spec) string { judged = s; return "it isn't allowed here" }}
	var failed *InstallFailed
	if _, err := ByID(context.Background(), d, "no-such-model", nil); err == nil || err.Error() != "Unknown model id: no-such-model" || errors.As(err, &failed) {
		t.Errorf("an unknown id: %v", err)
	}
	if judged != nil {
		t.Error("an unknown id was judged")
	}
	some := cat.All()[0]
	if _, err := ByID(context.Background(), d, some.ModelID, nil); err == nil || err.Error() != some.ModelID+": it isn't allowed here" || errors.As(err, &failed) {
		t.Errorf("an ineligible model: %v", err)
	}
	if judged != some {
		t.Errorf("judged %v, want %s", judged, some.ModelID)
	}
}

func TestInstallFailedUnwraps(t *testing.T) {
	inner := errors.New("HTTP 403")
	err := error(&InstallFailed{inner})
	if !errors.Is(err, inner) || err.Error() != "HTTP 403" {
		t.Errorf("%v", err)
	}
}
