package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// A refused placement changes nothing: its --context-size isn't saved either (review 01-M8).
func TestARefusedPlacementSavesNoContextSize(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	cat, _ := catalog.Load("")
	spec := cat.ByID("qwen3.5-4b-q4_k_m")
	os.MkdirAll(filepath.Join(root, "models", "llm"), 0o700)
	placementtest.WriteMoEGGUF(t, filepath.Join(root, "models", "llm", spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	contextSize := func() any {
		t.Helper()
		_, out := runApp(t, "--root", root, "--json", "models", "placement", "qwen3.5-4b-q4_k_m")
		line, _, _ := strings.Cut(out, "\n")
		var env struct {
			Data map[string]any `json:"data"`
		}
		json.Unmarshal([]byte(line), &env)
		req, _ := env.Data["requested"].(map[string]any)
		return req["contextSize"]
	}
	before := contextSize()
	// CPU expert layers don't apply on the test machines (unified memory or no GPU), so this is refused.
	code, out := runApp(t, "--root", root, "--json", "models", "placement", "qwen3.5-4b-q4_k_m", "--mode", "custom", "--cpu-moe-layers", "1", "--context-size", "4096")
	if code == 0 {
		t.Fatalf("the placement was accepted: %s", out)
	}
	if after := contextSize(); after != before || after == 4096.0 {
		t.Errorf("a refused placement saved its context size: %v, was %v", after, before)
	}
}
