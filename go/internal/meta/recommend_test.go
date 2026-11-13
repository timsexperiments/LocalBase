package meta

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// The rule's parts, on the built-in catalog: only the kind asked for and only what fits; LLMs largest
// minimum first, without their own runtime; the rest smallest on disk first; never more than six; no
// video.
func TestRecommend(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	minGB := func(s *catalog.Spec) int { return runtime.FitFor(s, 0, nil, nil, cat.PlannableFeedPeaks(s)).MinVramGB }
	for _, kind := range []string{"llm", "stt", "tts", "image"} {
		for _, vram := range []int{4, 8, 24, 1000} {
			picks := Recommend(cat, kind, vram)
			if len(picks) > 6 {
				t.Errorf("%s at %d GB: %d picks", kind, vram, len(picks))
			}
			for i, s := range picks {
				if s.Kind != kind || minGB(s) > vram || (kind == "llm" && s.LLMRuntime != nil) {
					t.Errorf("%s at %d GB: %s (%s, %d GB)", kind, vram, s.ModelID, s.Kind, minGB(s))
				}
				if i > 0 && kind == "llm" && minGB(picks[i-1]) < minGB(s) {
					t.Errorf("%s at %d GB: %s before %s", kind, vram, picks[i-1].ModelID, s.ModelID)
				}
				if i > 0 && kind != "llm" && picks[i-1].StorageGB > s.StorageGB {
					t.Errorf("%s at %d GB: %s before %s", kind, vram, picks[i-1].ModelID, s.ModelID)
				}
			}
		}
	}
	if picks := Recommend(cat, "video", 1000); len(picks) != 0 {
		t.Errorf("video: %d picks", len(picks))
	}
	n := 0
	for _, s := range cat.All() {
		if s.Kind == "image" {
			n++
		}
	}
	if want := min(n, 6); len(Recommend(cat, "image", 1000)) != want {
		t.Errorf("image with room for everything: %d, want %d", len(Recommend(cat, "image", 1000)), want)
	}
}
