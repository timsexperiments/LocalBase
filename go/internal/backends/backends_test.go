package backends

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

func host(os_, arch string, accs []string, reg *Registry, install map[string]string) Host {
	h := Host{OS: os_, Arch: arch, Accelerators: accs, Install: map[string]string{}}
	for _, d := range reg.All() {
		h.Install[d.ID] = Installable
		if d.Runtime != nil && !d.Runtime.Managed {
			h.Install[d.ID] = Missing
		}
	}
	for k, v := range install {
		h.Install[k] = v
	}
	return h
}

func chatModel(variants ...models.Variant) *models.Model {
	return &models.Model{ID: "m", Name: "M", Tasks: []string{"chat"}, Capabilities: []string{}, Variants: variants}
}

func gguf(id, arch string) models.Variant {
	v := models.Variant{ID: id, Format: models.Of("gguf", models.SourceInspect),
		Artifacts: []models.Artifact{{Filename: id + ".gguf", Role: models.RoleWeights, Required: true}}}
	if arch != "" {
		v.Architecture = models.Of(arch, models.SourceInspect)
	}
	return v
}

func writeMLX(t *testing.T, root string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "mlx-server")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	d := &Definition{Schema: Schema, ID: "mlx-lm", Name: "MLX LM", Engine: "mlx", Adapter: "process-openai/v1",
		Tasks: []string{"chat"}, Formats: []string{"mlx"}, Architectures: Architectures{Supported: []string{"qwen3"}},
		Platforms: []Platform{{OS: "darwin", Arch: "arm64", Accelerator: "metal"}},
		Launch: &Launch{Command: exe, Args: []string{"--model", "{model_dir}", "--port", "{port}"},
			Memory: &Memory{WeightsFactor: 1.2, OverheadGB: 1, Assumption: "weights plus 20% and 1 GB runtime overhead, declared by the person who registered it"}}}
	if err := Add(root, d); err != nil {
		t.Fatal(err)
	}
}

// Acceptance: the same logical model chooses a Metal/MLX variant on Apple
// Silicon and a CUDA/Vulkan variant on Linux, by explicit rules.
func TestPlatformVariants(t *testing.T) {
	root := t.TempDir()
	writeMLX(t, root)
	reg, err := Load(root)
	if err != nil || len(reg.Skipped()) > 0 {
		t.Fatal(err, reg.Skipped())
	}
	mlx := models.Variant{ID: "mlx-4bit", Format: models.Of("mlx", models.SourceDeclared), Architecture: models.Of("qwen3", models.SourceDeclared),
		Artifacts: []models.Artifact{{Filename: "model.safetensors", Role: models.RoleWeights, Required: true}}}
	m := chatModel(mlx, gguf("gguf-q4", "qwen3"))

	cases := []struct {
		name, os, arch string
		accs           []string
		install        map[string]string
		backend, varnt string
	}{
		{"apple silicon with mlx", "darwin", "arm64", []string{"metal", "cpu"}, map[string]string{"mlx-lm": Installed}, "mlx-lm", "mlx-4bit"},
		{"linux cuda", "linux", "x64", []string{"cuda", "vulkan", "cpu"}, map[string]string{"llama.cpp-cuda": Installed}, "llama.cpp-cuda", "gguf-q4"},
		{"linux vulkan", "linux", "x64", []string{"vulkan", "cpu"}, nil, "llama.cpp-vulkan", "gguf-q4"},
		{"linux cuda without a cuda build", "linux", "x64", []string{"cuda", "vulkan", "cpu"}, nil, "llama.cpp-vulkan", "gguf-q4"},
	}
	for _, c := range cases {
		h := host(c.os, c.arch, c.accs, reg, c.install)
		rec := reg.Recommend(m, h, nil)
		if rec.BackendID != c.backend || rec.VariantID != c.varnt {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, rec.BackendID, rec.VariantID, c.backend, c.varnt)
		}
		if len(rec.Options) != len(reg.All()) || rec.Options[0].BackendID != c.backend {
			t.Errorf("%s: options must list every backend, recommended first", c.name)
		}
	}
	// Without an MLX backend, Apple Silicon uses llama.cpp on Metal.
	reg2, _ := Load(t.TempDir())
	rec := reg2.Recommend(m, host("darwin", "arm64", []string{"metal", "cpu"}, reg2, nil), nil)
	if rec.BackendID != "llama.cpp-metal" || rec.VariantID != "gguf-q4" {
		t.Errorf("no mlx: got %s/%s", rec.BackendID, rec.VariantID)
	}
}

// Acceptance: a GGUF extension does not establish support; an incomplete
// multi-file model names its missing files.
func TestExtensionIsNotEvidence(t *testing.T) {
	reg, _ := Load("")
	h := host("darwin", "arm64", []string{"metal", "cpu"}, reg, nil)
	dir := t.TempDir()

	// Not a GGUF at all, despite the name.
	os.WriteFile(filepath.Join(dir, "fake.gguf"), []byte("this is not a model, just text bytes"), 0o644)
	// A real GGUF header for an architecture no backend declares.
	testutil.GGUF(filepath.Join(dir, "odd.gguf"), map[string]any{"general.architecture": "made-up-arch", "general.file_type": 15}, 4096)
	for _, name := range []string{"fake", "odd"} {
		v := models.Variant{ID: name, Artifacts: []models.Artifact{{Filename: name + ".gguf", Role: models.RoleWeights, Required: true}}}
		in := models.InspectVariant(v, dir)
		m := chatModel(v)
		rec := reg.Recommend(m, h, map[string]*models.Inspection{name: &in})
		if rec.BackendID != "" {
			t.Errorf("%s: recommended %s from a file name", name, rec.BackendID)
		}
		for _, o := range rec.Options {
			if o.Rating == Supported || o.Rating == Likely {
				t.Errorf("%s: %s rated %s", name, o.BackendID, o.Rating)
			}
		}
		if len(rec.Options) != len(reg.All()) {
			t.Errorf("%s: every registered backend must stay selectable", name)
		}
	}

	// A split GGUF with only its first shard.
	testutil.GGUF(filepath.Join(dir, "big-00001-of-00003.gguf"), map[string]any{
		"general.architecture": "qwen3", "split.count": 3, "split.no": 0, "general.file_type": 15}, 4096)
	v := models.Variant{ID: "split", Artifacts: []models.Artifact{{Filename: "big-00001-of-00003.gguf", Role: models.RoleWeights, Required: true}}}
	in := models.InspectVariant(v, dir)
	want := []string{"big-00002-of-00003.gguf", "big-00003-of-00003.gguf"}
	if !slices.Equal(in.Missing, want) {
		t.Errorf("missing %v, want %v", in.Missing, want)
	}
	f := Assess(chatModel(v), &in.Variant, reg.Get("llama.cpp-metal"), h, in.Missing)
	if f.Rating != Supported || len(f.Missing) != 2 {
		t.Errorf("split finding %+v", f)
	}

	// A vision model without its projector cannot be served by llama.cpp.
	vis := chatModel(gguf("v", "qwen3"))
	vis.Capabilities = []string{"vision"}
	if f := Assess(vis, &vis.Variants[0], reg.Get("llama.cpp-metal"), h, nil); f.Rating != Incompatible {
		t.Errorf("vision without projector rated %s", f.Rating)
	}
}

// Acceptance: unknown metadata leaves the selection blank while every
// backend stays manually selectable; manual choices survive reinspection;
// a removed backend is not replaced.
func TestSelectionModes(t *testing.T) {
	root := t.TempDir()
	writeMLX(t, root)
	reg, _ := Load(root)
	h := host("darwin", "arm64", []string{"metal", "cpu"}, reg, map[string]string{"mlx-lm": Installed})
	unknownVariant := models.Variant{ID: "x", Artifacts: []models.Artifact{{Filename: "x.bin", Role: models.RoleWeights, Required: true}}}
	m := chatModel(unknownVariant)

	s := reg.Resolve(m, h, nil, nil)
	if s.Mode != ModeAuto || s.BackendID != "" || s.Finding != nil {
		t.Fatalf("unknown model should have a blank automatic selection: %+v", s)
	}
	// Any backend can be chosen, including one detection flags.
	s = reg.Resolve(m, h, nil, &Choice{Mode: ModeManual, BackendID: "whisper.cpp-metal"})
	if s.BackendID != "whisper.cpp-metal" || s.Finding == nil || s.Finding.Rating != Incompatible || len(s.Finding.Reasons) == 0 {
		t.Fatalf("manual incompatible choice: %+v", s)
	}
	// Reinspection with new facts does not change a manual choice.
	m.Variants[0].Format = models.Of("gguf", models.SourceInspect)
	m.Variants[0].Architecture = models.Of("qwen3", models.SourceInspect)
	s = reg.Resolve(m, h, nil, &Choice{Mode: ModeManual, BackendID: "whisper.cpp-metal"})
	if s.BackendID != "whisper.cpp-metal" || s.Recommendation.BackendID != "llama.cpp-metal" {
		t.Fatalf("manual choice replaced by detection: %+v", s)
	}
	// Removing the chosen backend leaves it selected and unavailable.
	s = reg.Resolve(m, h, nil, &Choice{Mode: ModeManual, BackendID: "mlx-lm"})
	if s.BackendID != "mlx-lm" || s.Unavailable {
		t.Fatalf("mlx choice: %+v", s)
	}
	if ok, err := Remove(root, "mlx-lm"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	reg, _ = Load(root)
	s = reg.Resolve(m, h, nil, &Choice{Mode: ModeManual, BackendID: "mlx-lm"})
	if s.BackendID != "mlx-lm" || !s.Unavailable || s.Finding != nil {
		t.Fatalf("removed backend must stay selected and unavailable: %+v", s)
	}
}

func TestCustomDefinitionRules(t *testing.T) {
	root := t.TempDir()
	base := func() *Definition {
		return &Definition{Schema: Schema, ID: "my-engine", Name: "Mine", Engine: "mine", Adapter: "process-openai/v1",
			Tasks: []string{"chat"}, Formats: []string{"gguf"}, Platforms: []Platform{{OS: "linux", Arch: "x64", Accelerator: "cpu"}},
			Launch: &Launch{Command: "/bin/sh", Args: []string{"--port", "{port}"}, Memory: &Memory{WeightsFactor: 1, Assumption: "weights only"}}}
	}
	bad := map[string]func(d *Definition){
		"built-in id":      func(d *Definition) { d.ID = "llama.cpp-metal" },
		"unknown adapter":  func(d *Definition) { d.Adapter = "python-plugin/v1" },
		"relative command": func(d *Definition) { d.Launch.Command = "bin/server" },
		"no assumption":    func(d *Definition) { d.Launch.Memory.Assumption = "" },
		"no port":          func(d *Definition) { d.Launch.Args = []string{"--model", "{model}"} },
		"wrong schema":     func(d *Definition) { d.Schema = "localbase.backend/v0" },
	}
	for name, mutate := range bad {
		d := base()
		mutate(d)
		if err := Add(root, d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Add(root, base()); err != nil {
		t.Fatal(err)
	}
	// A file that does not validate is reported, not loaded.
	os.WriteFile(filepath.Join(Dir(root), "broken.json"), []byte(`{"schema":"localbase.backend/v1","id":"broken"}`), 0o600)
	reg, _ := Load(root)
	if reg.Get("my-engine") == nil || reg.Get("broken") != nil || len(reg.Skipped()) != 1 {
		t.Errorf("custom load: %v", reg.Skipped())
	}
	// Custom backends are never chosen automatically unless a rule names them.
	h := host("linux", "x64", []string{"cpu"}, reg, map[string]string{"my-engine": Installed})
	rec := reg.Recommend(chatModel(gguf("g", "qwen3")), h, nil)
	if rec.BackendID == "my-engine" {
		t.Error("an unrouted custom backend was picked automatically")
	}
}

func TestBuiltinsLoad(t *testing.T) {
	reg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range reg.All() {
		if !d.BuiltIn {
			t.Errorf("%s not built-in", d.ID)
		}
	}
	if compareVersions("b11671", "b10419") <= 0 || compareVersions("v1.2.10", "v1.2.9") <= 0 {
		t.Error("version compare")
	}
}

// "Likely" or better never comes from a name: only from facts with an
// evidence source.
func TestNoRatingFromNames(t *testing.T) {
	reg, _ := Load("")
	h := host("darwin", "arm64", []string{"metal", "cpu"}, reg, nil)
	v := models.Variant{ID: "v", Format: models.Fact[string]{Value: "gguf", Source: "filename"},
		Architecture: models.Fact[string]{Value: "llama", Source: "filename"},
		Artifacts:    []models.Artifact{{Filename: "llama-3-8b.gguf", Role: models.RoleWeights, Required: true}}}
	m := chatModel(v)
	for _, d := range reg.All() {
		f := Assess(m, &m.Variants[0], d, h, nil)
		if Plausible(f.Rating) {
			t.Errorf("%s rated %s from file-name facts", d.ID, f.Rating)
		}
		for _, r := range f.Reasons {
			if r.Code == "" || r.Message == "" || !strings.HasSuffix(r.Message, ".") {
				t.Errorf("%s: reason %+v needs a code and a sentence", d.ID, r)
			}
		}
	}
	f := Assess(m, &m.Variants[0], reg.Get("llama.cpp-metal"), h, nil)
	codes := []string{}
	for _, r := range f.Reasons {
		codes = append(codes, r.Code)
	}
	if !slices.Contains(codes, CodeFormatUnknown) || !slices.Contains(codes, CodeArchWeakEvidence) {
		t.Errorf("codes %v", codes)
	}
}

// CrispASR without a managed package (Linux today) is listed and labelled,
// never chosen automatically; with the pinned macOS package it is chosen for
// the speech models it is qualified on. The Vulkan build stays manual.
func TestCrispASRWithoutPin(t *testing.T) {
	reg, _ := Load("")
	tts := &models.Model{ID: "k", Tasks: []string{"speech"}, Capabilities: []string{}, Variants: []models.Variant{{ID: "v",
		Format: models.Of("gguf", models.SourceInspect), Architecture: models.Of("kokoro", models.SourceInspect),
		Artifacts: []models.Artifact{{Filename: "k.gguf", Role: models.RoleWeights, Required: true}, {Filename: "af.bin", Role: models.RoleVoice, Required: true}}}}}
	// No package for this device.
	h := host("darwin", "arm64", []string{"metal", "cpu"}, reg, map[string]string{"crispasr-metal": Missing, "crispasr-cpu": Missing})
	h.Unpackaged = map[string]bool{"crispasr-metal": true, "crispasr-cpu": true}
	rec := reg.Recommend(tts, h, nil)
	if strings.HasPrefix(rec.BackendID, "crispasr") {
		t.Errorf("an unpackaged CrispASR was chosen automatically")
	}
	for _, o := range rec.Options {
		if o.BackendID == "crispasr-metal" {
			codes := []string{}
			for _, w := range o.Warnings {
				codes = append(codes, w.Code)
			}
			if !slices.Contains(codes, CodeNoManagedPackage) {
				t.Errorf("warnings %v", codes)
			}
		}
	}
	// The pinned package: installable, chosen.
	h = host("darwin", "arm64", []string{"metal", "cpu"}, reg, map[string]string{"crispasr-metal": Installable, "crispasr-cpu": Installable})
	if rec := reg.Recommend(tts, h, nil); rec.BackendID != "crispasr-metal" {
		t.Errorf("pinned CrispASR not chosen: %q", rec.BackendID)
	}
	// Recognition is not qualified: a GGUF whisper model goes to whisper.cpp
	// even with CrispASR installed and first in the routing order, and the
	// CrispASR option says why it was passed over.
	stt := &models.Model{ID: "w", Tasks: []string{"transcription"}, Capabilities: []string{}, Variants: []models.Variant{{ID: "v",
		Format: models.Of("gguf", models.SourceInspect), Architecture: models.Of("whisper", models.SourceInspect),
		Artifacts: []models.Artifact{{Filename: "w.gguf", Role: models.RoleWeights, Required: true}}}}}
	h = host("darwin", "arm64", []string{"metal", "cpu"}, reg, map[string]string{"crispasr-metal": Installed, "crispasr-cpu": Installed, "whisper.cpp-metal": Installed})
	rec = reg.Recommend(stt, h, nil)
	if strings.HasPrefix(rec.BackendID, "crispasr") {
		t.Errorf("CrispASR chosen automatically for transcription: %q", rec.BackendID)
	}
	for _, o := range rec.Options {
		if o.BackendID == "crispasr-metal" && !slices.ContainsFunc(o.Warnings, func(w Reason) bool { return w.Code == CodeManualOnly }) {
			t.Errorf("CrispASR transcription option lacks %s: %v", CodeManualOnly, o.Warnings)
		}
	}
	if rec := reg.Recommend(tts, h, nil); rec.BackendID != "crispasr-metal" {
		t.Errorf("installed CrispASR not chosen for speech: %q", rec.BackendID)
	}
	bad := *reg.Get("crispasr-metal")
	bad.AutomaticTasks = []string{"image"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "automaticTasks") {
		t.Errorf("automaticTasks outside tasks: %v", err)
	}
	// The Vulkan build is manual-only, even when installed.
	h = host("linux", "x64", []string{"vulkan", "cpu"}, reg, map[string]string{"crispasr-vulkan": Installed, "crispasr-cpu": Missing})
	h.Unpackaged = map[string]bool{"crispasr-cpu": true}
	if rec := reg.Recommend(tts, h, nil); strings.HasPrefix(rec.BackendID, "crispasr") {
		t.Errorf("Linux CrispASR chosen automatically: %q", rec.BackendID)
	}
}
