package runtime

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestFFmpegPinsMatchTheManifest: the Go manifest pins the video converter
// releases in testdata/ffmpeg-pins.json for every target. The pins were
// recorded from the TS converter (tools/golden/ffmpeg.ts, removed), so a 0.1.0
// rollback installs and verifies the same ffmpeg.
func TestFFmpegPinsMatchTheManifest(t *testing.T) {
	raw, err := os.ReadFile("testdata/ffmpeg-pins.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Platform     string `json:"platform"`
		Architecture string `json:"architecture"`
		FFmpeg       any    `json:"ffmpeg"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Targets []struct {
			Platform     string         `json:"platform"`
			Architecture string         `json:"architecture"`
			Runtimes     map[string]any `json:"runtimes"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(goManifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	if len(golden) != 4 {
		t.Fatalf("%d golden targets", len(golden))
	}
	for _, g := range golden {
		found := false
		for _, tg := range m.Targets {
			if tg.Platform == g.Platform && tg.Architecture == g.Architecture {
				found = true
				if !reflect.DeepEqual(tg.Runtimes["ffmpeg"], g.FFmpeg) {
					t.Errorf("%s/%s: %v\nwant %v", g.Platform, g.Architecture, tg.Runtimes["ffmpeg"], g.FFmpeg)
				}
			}
		}
		if !found {
			t.Errorf("%s/%s: no target", g.Platform, g.Architecture)
		}
	}
}
