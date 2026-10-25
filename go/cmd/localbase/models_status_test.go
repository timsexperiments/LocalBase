package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"strings"
	"testing"
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

// Doctor on a Linux host with an NVIDIA GPU says the GPU is present and
// that its VRAM is not used for admission (operator observability audit).
func TestDoctorNVIDIALines(t *testing.T) {
	s := memory.Specs{OSName: "Ubuntu", GPUName: "NVIDIA GeForce RTX 3060",
		NVIDIA: []memory.NvidiaGPU{{Name: "NVIDIA GeForce RTX 3060", MemoryMiB: 12288}}}
	got := strings.Join(gpuLines(s), "\n")
	for _, want := range []string{"NVIDIA GPU: NVIDIA GeForce RTX 3060, 12288 MiB",
		"NVIDIA GPU present, VRAM admission not qualified on this build"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Discrete Memory") {
		t.Errorf("reports 0 GB of discrete VRAM:\n%s", got)
	}
	plain := strings.Join(gpuLines(memory.Specs{GPUName: "CPU / Integrated Graphics"}), "\n")
	if strings.Contains(plain, "NVIDIA") {
		t.Errorf("no GPU mentions NVIDIA:\n%s", plain)
	}
}
