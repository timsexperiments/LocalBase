package conf

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// Settings saved by an earlier release that name a former built-in entry,
// installed or not, stay valid after the upgrade (#368).
func TestKeepFormerSelections(t *testing.T) {
	root := t.TempDir()
	cat, err := catalog.LoadRoot(root, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default(root, 64, cat)
	if cfg.Active["llm"] != "qwen3.5-4b-q4_k_m" || cfg.Active["image"] != "dreamshaper-8-lcm" {
		t.Errorf("fresh defaults %s, %s", cfg.Active["llm"], cfg.Active["image"])
	}
	cfg.Selected["llm"] = []string{"qwen3.5-9b-q4_k_m", "gpt-oss-20b-q4_k_m"}
	cfg.Active["llm"] = "qwen3.5-9b-q4_k_m"
	cfg.Selected["stt"], cfg.Active["stt"] = []string{"whisper-tiny-en-q8_0"}, "whisper-tiny-en-q8_0"
	if issues := ValidateModels(cfg, cat, false); len(issues) == 0 {
		t.Fatal("former ids validated before they were kept")
	}
	KeepFormerSelections(root, cfg, cat)
	if issues := ValidateModels(cfg, cat, false); len(issues) > 0 {
		t.Fatalf("issues after keeping: %v", issues)
	}
	for _, id := range []string{"qwen3.5-9b-q4_k_m", "gpt-oss-20b-q4_k_m", "whisper-tiny-en-q8_0"} {
		if s := cat.ByID(id); s == nil || s.Tier != "user" {
			t.Errorf("%s not kept as a user entry", id)
		}
	}
}
