package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// What `models list`, `models recommend`, `models delete` and `models add-definition` answer, recorded
// before their rules moved to library and meta (5-7e2): the listed installs and loose files, the picks
// for each kind at a given amount of VRAM, and each way a delete or an added definition is refused or
// done. The root's path and times, and this device's backend selection, are masked. LOCALBASE_UPDATE_MODELS_GOLDEN=1 rewrites
// testdata/models_library.golden.
func TestModelsLibraryGolden(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	var b strings.Builder
	step := func(args ...string) {
		t.Helper()
		code, out := runApp(t, append([]string{"--root", root}, args...)...)
		b.WriteString("$ " + strings.Join(args, " ") + "\nexit " + strconv.Itoa(code) + "\n" + out + "\n")
	}
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "models", "llm", "zeta-loose.gguf"), []byte("x"))
	write(filepath.Join(root, "models", "llm", "alpha-loose.safetensors"), []byte("x"))
	write(filepath.Join(root, "models", "llm", ".hidden.gguf"), []byte("x"))
	write(filepath.Join(root, "models", "stt", "voice.bin"), []byte("x"))
	step("--json", "models", "list")
	step("models", "list", "--kind", "llm")
	step("models", "list", "--kind", "tts")

	for _, args := range [][]string{{"--vram", "8"}, {"--vram", "64"}, {"--kind", "stt", "--vram", "8"}, {"--kind", "tts", "--vram", "8"},
		{"--kind", "image", "--vram", "24"}, {"--kind", "video", "--vram", "64"}} {
		step(append([]string{"--json", "models", "recommend"}, args...)...)
	}
	step("models", "recommend", "--kind", "stt", "--vram", "4")

	weights := filepath.Join(tmp, "golden-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, weights, 1<<20)
	if code, out := runApp(t, "--root", root, "models", "import", weights, "--task", "chat", "--id", "golden-import", "--name", "Golden"); code != 0 {
		t.Fatalf("import: %s", out)
	}
	_, exported := runApp(t, "--root", root, "models", "export", "golden-import")
	var def map[string]any
	if err := json.NewDecoder(strings.NewReader(exported[strings.Index(exported, "{"):])).Decode(&def); err != nil {
		t.Fatalf("export: %v: %s", err, exported)
	}
	model := def
	if m, ok := def["model"].(map[string]any); ok {
		model = m
	}
	step("models", "delete", "qwen3.5-4b-q4_k_m", "--yes")
	step("models", "delete", "golden-import")
	step("models", "delete", "golden-import", "--yes")
	step("models", "delete", "golden-import", "--yes")

	file := func(name string, v any) string {
		p := filepath.Join(tmp, name)
		bs, _ := json.Marshal(v)
		write(p, bs)
		return p
	}
	step("models", "add-definition", filepath.Join(tmp, "missing.json"))
	notJSON := filepath.Join(tmp, "not.json")
	write(notJSON, []byte("{not json"))
	step("models", "add-definition", notJSON)
	bad := map[string]any{}
	for k, v := range model {
		bad[k] = v
	}
	bad["id"] = "Not An Id"
	step("models", "add-definition", file("bad.json", bad))
	step("--json", "models", "add-definition", file("wrapped-data.json", map[string]any{"data": map[string]any{"model": model}}))
	step("models", "add-definition", file("bare.json", model))
	step("models", "delete", "golden-import", "--yes")
	step("models", "add-definition", file("wrapped.json", map[string]any{"model": model}))
	step("models", "delete", "golden-import", "--yes")
	step("models", "add-definition", file("bare2.json", model))

	// The backend selection is this device's (Metal here, CPU or CUDA elsewhere), and the move leaves
	// it alone: it is masked in JSON, and its printed block after "Added …" is left out.
	var lines []string
	inSelection := false
	for _, line := range strings.Split(b.String(), "\n") {
		switch {
		case strings.HasPrefix(line, `{"data":{"kind":`):
			// A recommendation: the picks, not the whole catalog entry of each.
			var v struct {
				Data struct {
					Kind   string `json:"kind"`
					VRAMGB int    `json:"vramGb"`
					Models []struct {
						ModelID      string `json:"modelId"`
						MinVRAMGB    int    `json:"minVramGb"`
						MemorySource string `json:"memorySource"`
						FitVerdict   string `json:"fitVerdict"`
					} `json:"models"`
				} `json:"data"`
			}
			if json.NewDecoder(strings.NewReader(line)).Decode(&v) == nil {
				bs, _ := json.Marshal(v.Data)
				line = "picks " + string(bs)
			}
		case strings.HasPrefix(line, `{"data":`):
			var v map[string]any
			if json.NewDecoder(strings.NewReader(line)).Decode(&v) == nil {
				if data, ok := v["data"].(map[string]any); ok {
					if _, ok := data["selection"]; ok {
						data["selection"] = "<masked>"
					}
				}
				bs, _ := json.Marshal(v)
				line = string(bs)
			}
		case strings.HasPrefix(line, "Added "):
			lines = append(lines, line, "<selection masked>")
			inSelection = true
			continue
		case inSelection && line == "":
			inSelection = false
		case inSelection:
			continue
		}
		lines = append(lines, line)
	}
	got := strings.Join(lines, "\n")
	if real, err := filepath.EvalSymlinks(tmp); err == nil {
		got = strings.ReplaceAll(got, real, "<tmp>")
	}
	got = strings.ReplaceAll(got, tmp, "<tmp>")
	got = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`).ReplaceAllString(got, "<time>")
	golden := filepath.Join("testdata", "models_library.golden")
	if os.Getenv("LOCALBASE_UPDATE_MODELS_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the answers differ from %s:\n%s", golden, got)
	}
}
