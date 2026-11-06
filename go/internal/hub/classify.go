// Package hub is the Hugging Face Hub client for `models add hf:`,
// `models search` and the admin UI's model discovery (TS
// models/huggingface.ts and discovery.ts): search, classification, the files
// an entry would pin, and generated user catalog entries. Only these
// commands talk to the Hub; nothing on the request path does.
package hub

import (
	"regexp"
	"slices"
	"strings"
)

// PublicEndpoint is the Hub's public origin, used in generated entries and
// links. Requests go to Client.Endpoint, which tests point elsewhere.
const PublicEndpoint = "https://huggingface.co"

// AddableKinds are the kinds `models add` can write an entry for.
var AddableKinds = []string{"llm", "stt", "image"}

// License is the card license or `license:` tag, lowercased ("" if none).
func License(tags []string, card any) string {
	license := ""
	switch c := card.(type) {
	case string:
		license = c
	case []any:
		if len(c) > 0 {
			license, _ = c[0].(string)
		}
	}
	if license == "" {
		for _, t := range tags {
			if l, ok := strings.CutPrefix(t, "license:"); ok {
				license = l
				break
			}
		}
	}
	return strings.ToLower(license)
}

var openLicenses = map[string]bool{"apache-2.0": true, "mit": true, "bsd": true, "bsd-2-clause": true, "bsd-3-clause": true,
	"cc-by-4.0": true, "cc-by-sa-4.0": true, "cc0-1.0": true, "unlicense": true, "openrail": true, "bigscience-openrail-m": true,
	"creativeml-openrail-m": true, "openrail++": true, "artistic-2.0": true, "mpl-2.0": true, "gpl-3.0": true, "lgpl-3.0": true, "agpl-3.0": true}

var nonCommercialRe = regexp.MustCompile(`(^|-)nc($|-)|non-?commercial|research`)

// CommercialStatus is open, conditional or prohibited for a license.
func CommercialStatus(license string) string {
	switch {
	case license == "":
		return "conditional"
	case nonCommercialRe.MatchString(license):
		return "prohibited"
	case openLicenses[license]:
		return "open"
	}
	return "conditional"
}

var (
	textPipelines   = map[string]bool{"text-generation": true, "text2text-generation": true, "conversational": true}
	visionPipelines = map[string]bool{"image-text-to-text": true, "visual-question-answering": true}
	nonGGMLSpeech   = map[string]bool{"transformers": true, "ctranslate2": true, "whisperkit": true, "nemo": true, "mlx": true,
		"onnx": true, "coreml": true, "pytorch": true, "speechbrain": true, "espnet": true, "transformers.js": true}
)

// Pipeline is a multi-file image pipeline LocalBase can run.
type Pipeline struct{ ID, Label, Template, Family, Size string }

var ImagePipelines = map[string]Pipeline{
	"flux1":          {"flux1", "FLUX.1", "flux1-schnell-q4_0", "FLUX.1", "12B"},
	"z-image-turbo":  {"z-image-turbo", "Z-Image-Turbo", "z-image-turbo-q4_0", "Z-Image", "6B"},
	"flux2-klein-4b": {"flux2-klein-4b", "FLUX.2 klein 4B", "flux2-klein-4b-q4_0", "FLUX.2-klein", "4B"},
	"flux2-klein-9b": {"flux2-klein-9b", "FLUX.2 klein 9B", "flux2-klein-9b-q4_0", "FLUX.2-klein", "9B"},
}

// match is a rule outcome: a supported id, or why it is unsupported.
type match struct{ id, unsupported string }

type rule struct {
	re *regexp.Regexp
	m  match
}

func r(pattern string, id, unsupported string) rule {
	return rule{regexp.MustCompile("(?i)" + pattern), match{id, unsupported}}
}

var pipelineRules = []rule{
	r(`flux[-_.]?1[^ ]*?(kontext|fill|redux|canny|depth)`, "", "FLUX.1 editing pipeline (Kontext, Fill, Redux, Canny and Depth need image conditioning LocalBase has no profile for)"),
	r(`flux[-_.]?1`, "flux1", ""),
	r(`flux[-_.]?2[-_.]?klein[^ ]*?base`, "", "FLUX.2 klein base (undistilled; LocalBase profiles the distilled four-step klein models)"),
	r(`flux[-_.]?2[-_.]?klein[-_.]?4b`, "flux2-klein-4b", ""),
	r(`flux[-_.]?2[-_.]?klein[-_.]?9b`, "flux2-klein-9b", ""),
	r(`flux[-_.]?2`, "", "FLUX.2 dev (needs a Mistral text encoder LocalBase has no runtime profile for)"),
	r(`z[-_]?image[-_]?turbo`, "z-image-turbo", ""),
	r(`z[-_]?image`, "", "Z-Image base or edit model (LocalBase profiles Z-Image-Turbo only)"),
	r(`(^|[^a-z])wan[-_]?\d|video|ltx|mochi|cogvideo|hunyuan[-_]?video`, "", "video model (LocalBase adds video models from its measured catalog only)"),
	r(`sd-?3|stable-diffusion-3`, "", "Stable Diffusion 3 (needs CLIP-G, CLIP-L and T5 encoders; LocalBase has no SD3 runtime profile yet)"),
	r(`qwen[-_]?image`, "", "Qwen-Image (needs a Qwen2.5-VL encoder; LocalBase has no Qwen-Image runtime profile yet)"),
	r(`hunyuan|lumina|kolors|pixart|sana|hidream|cogview|auraflow|chroma|omnigen|minimax|ovis`, "", "multi-file image pipeline LocalBase has no runtime profile for"),
}

var baseModelRe = regexp.MustCompile(`(?i)^base_model:(?:[a-z]+:)?(.+)$`)

// lineage is the repository name and the base models its tags name.
func lineage(repo string, tags []string) string {
	parts := []string{repo}
	for _, t := range tags {
		if m := baseModelRe.FindStringSubmatch(t); m != nil {
			parts = append(parts, m[1])
		}
	}
	return strings.Join(parts, " ")
}

// imagePipelineFor is the pipeline a repository's name or base model
// belongs to; the name wins over its base model.
func imagePipelineFor(repo string, tags []string) *match {
	for _, text := range []string{repo, lineage(repo, tags)} {
		for _, rl := range pipelineRules {
			if rl.re.MatchString(text) {
				m := rl.m
				return &m
			}
		}
	}
	return nil
}

// LoraBase is a built-in image model a LoRA can be pinned onto.
type LoraBase struct{ ID, Label, Template string }

var ImageLoraBases = map[string]LoraBase{
	"flux1-schnell":  {"flux1-schnell", "FLUX.1 schnell", "flux1-schnell-q4_0"},
	"z-image-turbo":  {"z-image-turbo", "Z-Image-Turbo", "z-image-turbo-q4_0"},
	"flux2-klein-4b": {"flux2-klein-4b", "FLUX.2 klein 4B", "flux2-klein-4b-q4_0"},
	"flux2-klein-9b": {"flux2-klein-9b", "FLUX.2 klein 9B", "flux2-klein-9b-q4_0"},
	"sdxl":           {"sdxl", "SDXL 1.0", "sdxl-base-1.0"},
	"sd1.5":          {"sd1.5", "Stable Diffusion 1.5", "stable-diffusion-v1-5"},
}

var loraBaseRules = []rule{
	r(`flux[-_.]?1[^ ]*?(kontext|fill|redux|canny|depth)`, "", "a FLUX.1 editing pipeline LocalBase has no profile for"),
	r(`flux[-_.]?1[-_.]?schnell`, "flux1-schnell", ""),
	r(`flux[-_.]?1`, "", "FLUX.1 dev (LocalBase's built-in FLUX.1 is schnell; dev LoRAs need the dev weights)"),
	r(`flux[-_.]?2[-_.]?klein[^ ]*?base`, "", "FLUX.2 klein base (LocalBase's built-in klein models are the distilled ones)"),
	r(`flux[-_.]?2[-_.]?klein[-_.]?4b`, "flux2-klein-4b", ""),
	r(`flux[-_.]?2[-_.]?klein[-_.]?9b`, "flux2-klein-9b", ""),
	r(`flux[-_.]?2`, "", "FLUX.2 dev, which LocalBase has no runtime profile for"),
	r(`z[-_]?image[-_]?turbo`, "z-image-turbo", ""),
	r(`z[-_]?image`, "", "a Z-Image model other than Z-Image-Turbo"),
	r(`stable-diffusion-xl-refiner|sdxl[-_]?(turbo|lightning|refiner)`, "", "an SDXL variant other than SDXL Base 1.0"),
	r(`stable-diffusion-xl-base-1\.0|sdxl`, "sdxl", ""),
	r(`stable-diffusion-v1-5|(^|[^a-z0-9])sd[-_]?v?1[-_.]?5`, "sd1.5", ""),
}

// imageLoraBaseFor is the built-in model a LoRA belongs on: declared
// base_model tags decide alone; the name is a fallback without them.
func imageLoraBaseFor(repo string, tags []string) *match {
	text := strings.TrimSpace(lineage("", tags))
	if text == "" {
		text = repo
	}
	for _, rl := range loraBaseRules {
		if rl.re.MatchString(text) {
			m := rl.m
			return &m
		}
	}
	return nil
}

func isLoraRepository(library string, tags []string) bool {
	if library == "peft" || slices.Contains(tags, "lora") {
		return true
	}
	return slices.ContainsFunc(tags, func(t string) bool { return strings.HasPrefix(t, "base_model:adapter:") })
}

// IsDecisionRepository: Hub tags or GGUF metadata mark a decision model.
func IsDecisionRepository(tags []string, ggufArchitecture string) bool {
	for _, t := range tags {
		if l := strings.ToLower(t); l == "decision-model" || l == "systemone" {
			return true
		}
	}
	return ggufArchitecture == "clef"
}

// Likely is what LocalBase would run a repository as.
type Likely struct {
	Kind     *string `json:"kind"` // nil when it cannot
	Label    string  `json:"label"`
	Pipeline string  `json:"pipeline,omitempty"`
	Lora     string  `json:"lora,omitempty"`
}

func kindOf(k string) *string { return &k }

var whisperRe = regexp.MustCompile(`(?i)whisper`)

// LikelyKind classifies a repository from its pipeline tag, tags and library.
func LikelyKind(pipelineTag string, tags []string, repo, library string) Likely {
	library = strings.ToLower(library)
	if IsDecisionRepository(tags, "") && (pipelineTag == "" || visionPipelines[pipelineTag] || textPipelines[pipelineTag]) {
		return Likely{Kind: kindOf("llm"), Label: "decision"}
	}
	if visionPipelines[pipelineTag] {
		return Likely{Kind: kindOf("llm"), Label: "llm (vision)"}
	}
	if textPipelines[pipelineTag] {
		return Likely{Kind: kindOf("llm"), Label: "llm"}
	}
	if pipelineTag == "feature-extraction" || pipelineTag == "sentence-similarity" {
		return Likely{Label: "embeddings (not supported by models add)"}
	}
	if pipelineTag == "automatic-speech-recognition" {
		if library != "" && nonGGMLSpeech[library] {
			return Likely{Label: "speech to text (" + library + " weights; whisper.cpp needs ggml files)"}
		}
		if !whisperRe.MatchString(repo) && !slices.ContainsFunc(tags, whisperRe.MatchString) {
			return Likely{Label: "speech to text (only Whisper models run on whisper.cpp)"}
		}
		return Likely{Kind: kindOf("stt"), Label: "speech to text"}
	}
	if pipelineTag == "text-to-speech" {
		return Likely{Label: "text to speech (not supported by models add)"}
	}
	imageTag := pipelineTag == "text-to-image" || pipelineTag == "image-to-image"
	var pipeline *match
	if imageTag || pipelineTag == "" {
		pipeline = imagePipelineFor(repo, tags)
	}
	lora := isLoraRepository(library, tags)
	var loraBase *match
	if lora {
		loraBase = imageLoraBaseFor(repo, tags)
	}
	imageLike := pipelineTag == "text-to-image" ||
		(lora && pipelineTag == "" && loraBase != nil) ||
		(pipelineTag == "image-to-image" && pipeline != nil) ||
		(pipelineTag == "" && (library == "diffusion-single-file" ||
			(pipeline != nil && (library == "" || library == "gguf" || library == "diffusers" || library == "ggml"))))
	if imageLike {
		if lora {
			switch {
			case loraBase == nil:
				return Likely{Label: "image LoRA for a base model LocalBase has no profile for"}
			case loraBase.unsupported != "":
				return Likely{Label: "image LoRA for " + loraBase.unsupported}
			}
			return Likely{Kind: kindOf("image"), Label: "image LoRA (" + ImageLoraBases[loraBase.id].Label + ")", Lora: loraBase.id}
		}
		if library == "mlx" || library == "onnx" || slices.Contains(tags, "coreml") {
			lib := library
			if lib == "" {
				lib = "coreml"
			}
			return Likely{Label: "image (" + lib + " weights; sd.cpp needs a checkpoint file)"}
		}
		if pipeline != nil && pipeline.unsupported != "" {
			return Likely{Label: "image: " + pipeline.unsupported}
		}
		if pipeline != nil {
			return Likely{Kind: kindOf("image"), Label: "image (" + ImagePipelines[pipeline.id].Label + " pipeline)", Pipeline: pipeline.id}
		}
		return Likely{Kind: kindOf("image"), Label: "image"}
	}
	if pipelineTag == "" && slices.Contains(tags, "gguf") {
		return Likely{Kind: kindOf("llm"), Label: "llm (no pipeline tag)"}
	}
	if pipelineTag != "" {
		return Likely{Label: pipelineTag + " (not supported)"}
	}
	return Likely{Label: "unknown"}
}
