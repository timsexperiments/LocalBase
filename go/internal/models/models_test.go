package models

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

func TestFromSpecWholeCatalog(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cat.Models {
		m := FromSpec(s)
		if err := m.Validate(); err != nil {
			t.Errorf("%s: %v", s.ModelID, err)
			continue
		}
		v := m.Variants[0]
		if len(v.QualifiedOn) == 0 && s.Kind != "video" {
			t.Errorf("%s: built-in entry without qualification record", s.ModelID)
		}
		if s.Kind == "llm" && v.Architecture.Known() {
			t.Errorf("%s: LLM architecture must come from the GGUF header, not the record", s.ModelID)
		}
		for _, a := range v.Artifacts {
			if a.SHA256 != "" && a.HashTrust != HashAuthoritative {
				t.Errorf("%s: pinned hash not authoritative", s.ModelID)
			}
		}
	}
}

func TestMerge(t *testing.T) {
	pinned, inspected := Of("Q4_K_M", SourcePinned), Of("Q8_0", SourceInspect)
	if got := Merge(pinned, inspected); got.Value != "Q8_0" || got.Source != SourceInspect {
		t.Errorf("the file must win over the record: %+v", got)
	}
	if got := Merge(Of("a", SourcePinned), Of("b", SourceDeclared)); got.Known() || got.Conflict == "" {
		t.Errorf("equal-strength disagreement must be unknown: %+v", got)
	}
	if got := Merge(Fact[string]{}, Fact[string]{}); got.Known() {
		t.Error("nothing is not a fact")
	}
}

func TestInspectFormats(t *testing.T) {
	dir := t.TempDir()
	// safetensors with a config.json sidecar.
	header, _ := json.Marshal(map[string]any{
		"a": map[string]any{"dtype": "BF16", "shape": []int{1000, 10}, "data_offsets": []int{0, 20000}},
		"b": map[string]any{"dtype": "F32", "shape": []int{10}, "data_offsets": []int{20000, 20040}},
	})
	st := make([]byte, 8)
	binary.LittleEndian.PutUint64(st, uint64(len(header)))
	st = append(append(st, header...), make([]byte, 20040)...)
	os.WriteFile(filepath.Join(dir, "model.safetensors"), st, 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644)
	ff := InspectFile(filepath.Join(dir, "model.safetensors"))
	if ff.Format.Value != "safetensors" || ff.Precision.Value != "BF16" || ff.Architecture.Value != "qwen3" {
		t.Errorf("safetensors: %+v", ff)
	}
	// whisper.cpp ggml.
	g := make([]byte, 64)
	binary.LittleEndian.PutUint32(g, 0x67676d6c)
	os.WriteFile(filepath.Join(dir, "ggml-base.bin"), g, 0o644)
	if ff := InspectFile(filepath.Join(dir, "ggml-base.bin")); ff.Format.Value != "ggml" || ff.Architecture.Value != "whisper" {
		t.Errorf("ggml: %+v", ff)
	}
	// GGUF projector.
	testutil.GGUF(filepath.Join(dir, "mmproj.gguf"), map[string]any{"general.architecture": "clip", "clip.has_vision_encoder": true}, 1024)
	if ff := InspectFile(filepath.Join(dir, "mmproj.gguf")); ff.Role != RoleProjector {
		t.Errorf("projector: %+v", ff)
	}
	// The record says Q4_K_M; the file says Q8_0 and qwen3 with 32k context.
	testutil.GGUF(filepath.Join(dir, "w.gguf"), map[string]any{"general.architecture": "qwen3", "general.file_type": 7,
		"qwen3.context_length": 32768, "qwen3.embedding_length": 1024}, 4096)
	v := Variant{ID: "v", Precision: Of("Q4_K_M", SourcePinned),
		Artifacts: []Artifact{{Filename: "w.gguf", Role: RoleWeights, Required: true}, {Filename: "absent.gguf", Role: RoleProjector, Required: true}}}
	in := InspectVariant(v, dir)
	if in.Variant.Precision.Value != "Q8_0" || in.Variant.Architecture.Value != "qwen3" || in.Variant.ContextLength.Value != 32768 ||
		in.Variant.Dimensions.Value != 1024 || len(in.Missing) != 1 || in.Missing[0] != "absent.gguf" {
		t.Errorf("inspection: %+v", in)
	}
}
