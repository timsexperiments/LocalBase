package runtime

import (
	"errors"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The refusal comes from the model's targets, with the model first. It says nothing of a platform
// LocalBase can't run video on.
func TestVideoEligibleRefusesWithWhatTheTargetsSay(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.NewController(unsampled{newStepHost(50 * memory.GiB)}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	s := &Service{d: ServiceDeps{Memory: mem, Config: func() *store.Config { return &store.Config{AllowExperimental: true} }}}
	onAMac := goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64"
	seen := 0
	for _, spec := range cat.All() {
		if spec.VideoRuntime == nil {
			continue
		}
		seen++
		err := s.videoEligible(spec)
		if err == nil {
			if !onAMac || !spec.VideoRunsAnywhere() {
				t.Errorf("%s was allowed", spec.ModelID)
			}
			continue
		}
		if onAMac && spec.VideoRunsAnywhere() {
			t.Errorf("%s has a target here: %v", spec.ModelID, err)
		}
		var launch *LaunchError
		if !errors.As(err, &launch) || launch.Code != "model_hardware_ineligible" {
			t.Fatalf("%s: %v", spec.ModelID, err)
		}
		if want := spec.ModelID + ": " + spec.VideoRefusal(); launch.Message != want {
			t.Errorf("got %q, want %q", launch.Message, want)
		}
		if strings.Contains(launch.Message, "NVIDIA") || strings.Contains(launch.Message, "Linux") {
			t.Errorf("implies support that doesn't exist: %q", launch.Message)
		}
	}
	if seen == 0 {
		t.Error("no video models")
	}
}
