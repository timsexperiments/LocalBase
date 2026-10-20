package models

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// FromSpec reads a catalog entry (the TS modelSpecObjectSchema) as a logical
// model with one variant. Facts the entry pins are SourcePinned; the
// architecture of an LLM is left unknown until its GGUF header is inspected.
// Built-in entries record the backends they were qualified on; user entries
// record none.
func FromSpec(s *catalog.Spec) Model {
	m := Model{ID: s.ModelID, Name: s.Name(), Origin: "catalog", SourceURL: s.Source}
	if s.Trust != "qualified" {
		m.Origin = "user-catalog"
	}
	m.Tasks = []string{specTask(s)}
	m.Capabilities = specCapabilities(s)
	v := Variant{ID: "default"}
	if f := specFormat(s); f != "" {
		v.Format = Of(f, SourcePinned)
	}
	if a := specArchitecture(s); a != "" {
		v.Architecture = Of(a, SourcePinned)
	}
	if s.Quant != "" {
		v.Precision = Of(strings.ToUpper(s.Quant), SourcePinned)
	}
	if s.ContextWindowTokens != nil && *s.ContextWindowTokens > 0 {
		v.ContextLength = Of(*s.ContextWindowTokens, SourcePinned)
	}
	for _, a := range s.Artifacts {
		art := Artifact{Filename: a.Filename, Role: specRole(s, a), Required: true, SizeBytes: a.ExpectedSizeBytes, SHA256: a.SHA256,
			Source: Source{Kind: "huggingface", Repo: hfRepo(s.Source), Revision: s.RepositoryRevision, Path: a.SourcePath}}
		if a.SHA256 != "" {
			art.HashTrust = HashAuthoritative
		}
		v.Artifacts = append(v.Artifacts, art)
	}
	if s.Trust == "qualified" {
		v.QualifiedOn = qualifiedOn(s)
	}
	m.Variants = []Variant{v}
	return m
}

func hfRepo(source string) string {
	return strings.Trim(strings.TrimPrefix(source, "https://huggingface.co/"), "/")
}

func specTask(s *catalog.Spec) string {
	switch s.Kind {
	case "llm":
		if s.LLMRuntime != nil {
			switch s.LLMRuntime.Capability {
			case "embedding-only":
				return "embeddings"
			case "decision-only":
				return "decision"
			}
		}
		return "chat"
	case "stt":
		return "transcription"
	case "tts":
		return "speech"
	}
	return s.Kind // image, video
}

// specCapabilities keeps what a backend must preserve: input modalities and
// features that depend on the engine.
func specCapabilities(s *catalog.Spec) []string {
	out := []string{}
	add := func(c string) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if s.Kind == "llm" {
		if slices.Contains(s.InputModalities, "image") && s.Projector() != nil {
			add("vision")
		}
		if slices.Contains(s.InputModalities, "audio") && s.Projector() != nil {
			add("audio-input")
		}
	}
	for _, c := range s.Capabilities {
		switch c.ID {
		case "tool-calling":
			add("tool-calling")
		case "image-to-image":
			add("image-edit")
		case "image-to-video":
			add("image-to-video")
		case "speech-to-video":
			add("speech-to-video")
		}
	}
	return out
}

func specFormat(s *catalog.Spec) string {
	p := s.Primary()
	if p == nil {
		return ""
	}
	switch strings.ToLower(filepath.Ext(p.Filename)) {
	case ".gguf":
		return "gguf"
	case ".safetensors":
		return "safetensors"
	case ".onnx":
		return "onnx"
	case ".bin":
		if s.Kind == "stt" {
			return "ggml"
		}
	}
	return ""
}

func specArchitecture(s *catalog.Spec) string {
	f := strings.ToLower(s.Family)
	switch s.Kind {
	case "stt":
		return "whisper"
	case "tts":
		if s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx" {
			return "kokoro"
		}
		if strings.Contains(f, "qwen3-tts") {
			return "qwen3-tts"
		}
	case "image":
		switch {
		case strings.HasPrefix(f, "stable-diffusion-xl"):
			return "sdxl"
		case strings.HasPrefix(f, "stable-diffusion"):
			return "sd1"
		case strings.HasPrefix(f, "flux.1"):
			return "flux1"
		case strings.HasPrefix(f, "flux.2"):
			return "flux2"
		case strings.HasPrefix(f, "z-image"):
			return "z-image"
		}
	case "video":
		switch {
		case strings.Contains(f, "wan2.1"):
			return "wan2.1"
		case strings.Contains(f, "wan2.2"):
			return "wan2.2"
		case strings.HasPrefix(f, "hunyuanvideo"):
			return "hunyuanvideo"
		case strings.HasPrefix(f, "ltx"):
			return "ltx"
		}
	}
	return ""
}

func specRole(s *catalog.Spec, a catalog.Artifact) string {
	if a.Role == "primary" {
		return RoleWeights
	}
	if p := s.Projector(); p != nil && p.Filename == a.Filename {
		return RoleProjector
	}
	if r := s.ImageRuntime; r != nil {
		switch a.Filename {
		case r.Artifacts.VAE:
			return RoleVAE
		case r.Artifacts.TextEncoder, r.Artifacts.ClipL, r.Artifacts.T5XXL:
			return RoleTextEncoder
		}
	}
	if r := s.VideoRuntime; r != nil {
		switch a.Filename {
		case r.Artifacts.TextEncoder, r.Artifacts.AudioEncoder:
			return RoleTextEncoder
		case r.Artifacts.Decoder.ArtifactFilename:
			return RoleVAE
		}
	}
	if r := s.TTSRuntime; r != nil {
		for _, v := range r.ReferenceVoices {
			if v.ArtifactFilename == a.Filename {
				return RoleVoice
			}
		}
		if r.Artifacts != nil {
			switch a.Filename {
			case r.Artifacts.Tokenizer, r.Artifacts.TokenizerConfig:
				return RoleTokenizer
			case r.Artifacts.Config:
				return RoleConfig
			}
		}
	}
	return RoleCompanion
}

// qualifiedOn names the backend builds the TS project qualified the curated
// catalog on (audit 2026-10-20: Mac Metal, Linux Vulkan).
func qualifiedOn(s *catalog.Spec) []string {
	switch s.Kind {
	case "llm":
		return []string{"llama.cpp-metal", "llama.cpp-vulkan"}
	case "stt":
		return []string{"whisper.cpp-metal", "whisper.cpp-vulkan"}
	case "tts":
		if s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx" {
			return []string{"onnx-cpu"}
		}
		return []string{"tts-server"}
	case "image":
		return []string{"stable-diffusion.cpp-metal", "stable-diffusion.cpp-vulkan"}
	case "video":
		var out []string
		if s.VideoRuntime != nil {
			for _, t := range s.VideoRuntime.SupportedTargets {
				id := "stable-diffusion.cpp-" + t.Accelerator
				if !slices.Contains(out, id) {
					out = append(out, id)
				}
			}
		}
		return out
	}
	return nil
}
