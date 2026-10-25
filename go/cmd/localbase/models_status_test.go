package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

func runApp(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := app()
	a.Stdout, a.Stderr = &out, &errb
	code := a.Run(args)
	return code, out.String() + errb.String()
}

// `models show` reports Enabled and Default from the saved configuration,
// not from fresh defaults. TS 0.1.0 has that bug: its show command runs with
// requiresDatabase: false and no readOnlyConfiguration, so it reads
// defaultConfig and prints "Disabled" for selected models (live, 2026-10-21).
func TestModelsShowStatusFromSavedConfig(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const pref, other, unselected = "qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.8-27b-q4_k_m", "clef-q4_k_m"
	if code, out := runApp(t, "--root", root, "configure", "--llm-models", "qwen3.5-4b-q4_k_m,"+other+","+pref, "--active-llm", pref); code != 0 {
		t.Fatalf("configure models: %s", out)
	}
	for id, want := range map[string]string{
		pref:                "Not installed · Enabled · Default",
		other:               "Not installed · Enabled",
		unselected:          "Not installed · Disabled",
		"qwen3.5-4b-q4_k_m": "Not installed · Enabled",
	} {
		code, out := runApp(t, "--root", root, "models", "show", id)
		if code != 0 {
			t.Fatalf("show %s: %s", id, out)
		}
		var status string
		for _, l := range strings.Split(out, "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(l), "Status:"); ok {
				status = strings.TrimSpace(s)
			}
		}
		if status != want {
			t.Errorf("%s: status %q, want %q", id, status, want)
		}
		_, js := runApp(t, "--root", root, "--json", "models", "show", id)
		js, _, _ = strings.Cut(js, "\n") // the envelope; stderr follows
		var env struct {
			Data struct {
				Status struct{ Enabled, Default bool } `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(js), &env); err != nil {
			t.Fatalf("show --json %s: %v %s", id, err, js)
		}
		if env.Data.Status.Enabled != strings.Contains(want, "Enabled") || env.Data.Status.Default != strings.Contains(want, "Default") {
			t.Errorf("%s: json status %+v, want %q", id, env.Data.Status, want)
		}
	}
}

// configure --default-llm-models with two or more models leaves
// activeLlmModel as it is; one model sets it (operator, after cutover).
func TestConfigureDefaultsLeaveActive(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const big, small = "qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-4b-q4_k_m"
	if code, out := runApp(t, "--root", root, "configure", "--llm-models", small+","+big, "--active-llm", small); code != 0 {
		t.Fatalf("configure models: %s", out)
	}
	active := func() string {
		_, out := runApp(t, "--root", root, "config", "show")
		for _, l := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(l, "activeLlmModel = "); ok {
				return strings.Trim(v, `"`)
			}
		}
		return ""
	}
	if code, out := runApp(t, "--root", root, "configure", "--default-llm-models", big+","+small); code != 0 {
		t.Fatalf("defaults: %s", out)
	}
	if got := active(); got != small {
		t.Errorf("a two-model list moved activeLlmModel to %s", got)
	}
	if code, out := runApp(t, "--root", root, "configure", "--default-llm-models", big); code != 0 {
		t.Fatalf("one default: %s", out)
	}
	if got := active(); got != big {
		t.Errorf("a one-model list left activeLlmModel at %s", got)
	}
}

// Doctor on a Linux host with an NVIDIA GPU reports its free memory and
// that placement is planned against it; on unified memory it says
// placement does not apply (DESIGN §20.2).
func TestDoctorNVIDIALines(t *testing.T) {
	s := memory.Specs{OSName: "Ubuntu", GPUName: "NVIDIA GeForce RTX 3060",
		NVIDIA: []memory.NvidiaGPU{{Name: "NVIDIA GeForce RTX 3060", MemoryMiB: 12288}}}
	got := strings.Join(gpuLines(s, []memory.GPUMemory{{Index: 0, Name: "NVIDIA GeForce RTX 3060", FreeBytes: 11899 << 20, TotalBytes: 12288 << 20}}), "\n")
	for _, want := range []string{"NVIDIA GPU 0: NVIDIA GeForce RTX 3060, 11.6 GB free of 12.0 GB",
		"planned against its free memory (0.5 GB kept free)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Discrete Memory") || strings.Contains(got, "not qualified") {
		t.Errorf("stale VRAM wording:\n%s", got)
	}
	s.NVIDIA = append(s.NVIDIA, memory.NvidiaGPU{Name: "NVIDIA A10", MemoryMiB: 23028})
	if two := strings.Join(gpuLines(s, nil), "\n"); !strings.Contains(two, "CUDA_VISIBLE_DEVICES") {
		t.Errorf("multi-GPU wording:\n%s", two)
	}
	mac := strings.Join(gpuLines(memory.Specs{GPUName: "Apple M4 Max GPU", IsAppleSilicon: true, GPUVramGB: 64}, nil), "\n")
	if !strings.Contains(mac, "Unified Memory") || !strings.Contains(mac, "does not apply") || strings.Contains(mac, "NVIDIA") {
		t.Errorf("unified:\n%s", mac)
	}
}

// `models placement` shows and changes the stored choice; the context size
// is the shared setting (DESIGN §20.2, §20.8).
func TestModelsPlacementCLI(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	cat, _ := catalog.Load("")
	spec := cat.ByID("qwen3.5-4b-q4_k_m")
	os.MkdirAll(filepath.Join(root, "models", "llm"), 0o700)
	placementtest.WriteMoEGGUF(t, filepath.Join(root, "models", "llm", spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	run := func(args ...string) (int, map[string]any, string) {
		code, out := runApp(t, append([]string{"--root", root, "--json", "models", "placement", "qwen3.5-4b-q4_k_m"}, args...)...)
		line, _, _ := strings.Cut(out, "\n")
		var env struct {
			Data  map[string]any `json:"data"`
			Error map[string]any `json:"error"`
		}
		json.Unmarshal([]byte(line), &env)
		if env.Error != nil {
			msg, _ := env.Error["message"].(string)
			return code, nil, msg
		}
		return code, env.Data, ""
	}
	code, d, _ := run()
	if code != 0 || d["requested"].(map[string]any)["mode"] != "auto" {
		t.Fatalf("show: %d %v", code, d)
	}
	if code, d, _ = run("--mode", "cpu"); code != 0 || d["requested"].(map[string]any)["mode"] != "cpu" || d["next"].(map[string]any)["gpuLayers"] != 0.0 {
		t.Errorf("cpu: %d %v", code, d)
	}
	if code, d, _ = run("--context-size", "16384"); code != 0 || d["requested"].(map[string]any)["contextSize"] != 16384.0 || d["requested"].(map[string]any)["contextMode"] != "explicit" {
		t.Errorf("context: %d %v", code, d)
	}
	// Not applicable on this test machine (unified memory or no GPU).
	if code, _, msg := run("--mode", "custom", "--cpu-moe-layers", "1"); code == 0 || msg == "" {
		t.Errorf("cpu expert layers here: %d %q", code, msg)
	}
	if code, d, _ = run("--reset"); code != 0 || d["requested"].(map[string]any)["mode"] != "auto" {
		t.Errorf("reset: %d %v", code, d)
	}
	if code, _, msg := run("--mode", "fastest"); code == 0 || !strings.Contains(msg, "auto, gpu, cpu or custom") {
		t.Errorf("bad mode: %d %q", code, msg)
	}
	// models show prints the Placement block in text, not in its JSON.
	_, text := runApp(t, "--root", root, "models", "show", "qwen3.5-4b-q4_k_m")
	if !strings.Contains(text, "Placement") || !strings.Contains(text, "shared by all models") {
		t.Errorf("show text:\n%s", text)
	}
	_, js := runApp(t, "--root", root, "--json", "models", "show", "qwen3.5-4b-q4_k_m")
	if strings.Contains(js, "placement") {
		t.Errorf("show JSON gained placement (TS shape changed)")
	}
}
