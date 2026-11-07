package hub

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/collate"
)

const minImageCheckpointBytes = 500_000_000

var (
	ggufExtRe   = regexp.MustCompile(`(?i)\.gguf$`)
	sttFileRe   = regexp.MustCompile(`(?i)ggml.*\.bin$`)
	sttSkipRe   = regexp.MustCompile(`(?i)encoder|coreml|silero|vad`)
	imageExtRe  = regexp.MustCompile(`(?i)\.(safetensors|ckpt|gguf)$`)
	imageSkipRe = regexp.MustCompile(`(?i)vae|lora|lycoris|controlnet|refiner|inpaint|text[-_]?encoder|clip|t5|unet|embedding|adapter|mmproj`)
	pipeDirRe   = regexp.MustCompile(`^(split_files/)?diffusion_models/[^/]+$`)
	pipeExtRe   = regexp.MustCompile(`(?i)\.(safetensors|gguf)$`)
	pipeSkipRe  = regexp.MustCompile(`(?i)vae|(^|[-_.])ae[-_.]|lora|text[-_]?encoder|clip|t5|qwen|mmproj|nvfp4|mxfp4|fp4|int8|int4|convrot|scaled|svdq|nunchaku`)
	safetensRe  = regexp.MustCompile(`(?i)\.safetensors$`)
	comfyRe     = regexp.MustCompile(`(?i)comfy`)
)

func filter(files []File, keep func(File) bool) []File {
	var out []File
	for _, f := range files {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

func llmChoices(files []File) []File {
	return filter(files, func(f File) bool {
		if !ggufExtRe.MatchString(f.Path) || auxiliaryGGUF(f.Path) {
			return false
		}
		m := shardRe.FindStringSubmatch(f.Path)
		return m == nil || m[1] == "00001"
	})
}

func sttChoices(files []File) []File {
	return filter(files, func(f File) bool {
		n := path.Base(f.Path)
		return sttFileRe.MatchString(n) && !sttSkipRe.MatchString(n)
	})
}

func imageChoices(files []File) []File {
	return filter(files, func(f File) bool {
		n := path.Base(f.Path)
		return !strings.Contains(f.Path, "/") && imageExtRe.MatchString(n) && f.Size >= minImageCheckpointBytes && !imageSkipRe.MatchString(n)
	})
}

func pipelineChoices(files []File) []File {
	return filter(files, func(f File) bool {
		n := path.Base(f.Path)
		return (!strings.Contains(f.Path, "/") || pipeDirRe.MatchString(f.Path)) && pipeExtRe.MatchString(n) &&
			f.Size >= minImageCheckpointBytes && !pipeSkipRe.MatchString(n)
	})
}

func loraChoices(files []File) []File {
	out := filter(files, func(f File) bool { return !strings.Contains(f.Path, "/") && safetensRe.MatchString(f.Path) })
	sortBySize(out)
	return out
}

// sortBySize orders by size, then path (localeCompare).
func sortBySize(files []File) {
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size < files[j].Size
		}
		return collate.Compare(files[i].Path, files[j].Path) < 0
	})
}

var baseRe = regexp.MustCompile(`(?i)base`)

func defaultChoice(kind string, choices []File) *File {
	if len(choices) == 1 {
		return &choices[0]
	}
	if kind == "llm" {
		for _, q := range preferredQuants {
			var match []File
			for _, f := range choices {
				if QuantOf(f.Path) == q {
					match = append(match, f)
				}
			}
			if len(match) == 1 {
				return &match[0]
			}
		}
		return nil
	}
	sorted := slices.Clone(choices)
	sortBySize(sorted)
	if kind == "stt" {
		for _, f := range sorted {
			if baseRe.MatchString(path.Base(f.Path)) && QuantOf(f.Path) == "Q8_0" {
				return &f
			}
		}
		for _, f := range sorted {
			if QuantOf(f.Path) == "Q8_0" {
				return &f
			}
		}
		if len(sorted) > 0 {
			return &sorted[0]
		}
		return nil
	}
	for _, f := range sorted {
		if safetensRe.MatchString(f.Path) {
			return &f
		}
	}
	for _, q := range imageQuants {
		for _, f := range sorted {
			if QuantOf(f.Path) == q {
				return &f
			}
		}
	}
	if len(sorted) > 0 {
		return &sorted[0]
	}
	return nil
}

func loraDefault(choices []File) *File {
	if len(choices) == 1 {
		return &choices[0]
	}
	native := filter(choices, func(f File) bool { return !comfyRe.MatchString(path.Base(f.Path)) })
	if len(native) == 1 {
		return &native[0]
	}
	return nil
}

func pipelineDefault(choices []File) *File {
	sorted := slices.Clone(choices)
	sortBySize(sorted)
	for _, q := range pipelineQuants {
		for _, f := range sorted {
			if QuantOf(f.Path) == q {
				return &f
			}
		}
	}
	if len(sorted) > 0 {
		return &sorted[0]
	}
	return nil
}

var noFiles = map[string]string{
	"llm":   `has no GGUF weights LocalBase can download. LocalBase runs language models as GGUF files; look for a repository with "GGUF" in its name (try ` + "`local-base models search`" + `).`,
	"stt":   "has no whisper.cpp ggml-*.bin files. LocalBase runs speech recognition with whisper.cpp; try ggerganov/whisper.cpp or a repository with ggml files.",
	"image": "has no single-file Stable Diffusion checkpoint (.safetensors, .ckpt or .gguf at the repository root). Multi-file pipelines outside FLUX.1, FLUX.2 klein and Z-Image-Turbo are not supported by models add.",
}

const noDiffusersFiles = "is a diffusers folder layout (model_index.json with separate unet, text encoder and VAE folders), which sd.cpp cannot load. Look for a single-file .safetensors or GGUF release of the same model."

var noun = map[string]string{"llm": "GGUF", "stt": "ggml", "image": "checkpoint"}

// Choices are the files `models add` could pin for a repository.
type Choices struct {
	Kind     string
	Files    []File
	Default  *File
	Pipeline string
	Lora     string
}

// EntryChoices is the files an entry could pin, after the kind check.
func EntryChoices(info *ModelInfo) (*Choices, error) {
	likely := LikelyKind(info.PipelineTag, info.Tags, info.ID, info.Library)
	if likely.Kind == nil {
		return nil, &Error{Message: info.ID + " looks like " + likely.Label + ". models add supports GGUF language models, whisper.cpp speech models, single-file Stable Diffusion checkpoints, FLUX.1, FLUX.2 klein and Z-Image-Turbo diffusion models, and image LoRAs for FLUX.1 schnell, FLUX.2 klein, Z-Image-Turbo, SDXL 1.0 and Stable Diffusion 1.5."}
	}
	kind := *likely.Kind
	files := lfsFiles(info)
	c := &Choices{Kind: kind, Lora: likely.Lora}
	if c.Lora == "" {
		c.Pipeline = likely.Pipeline
	}
	switch {
	case c.Lora != "":
		c.Files = loraChoices(files)
	case kind == "llm":
		c.Files = llmChoices(files)
	case kind == "stt":
		c.Files = sttChoices(files)
	case c.Pipeline != "":
		c.Files = pipelineChoices(files)
	default:
		c.Files = imageChoices(files)
	}
	if len(c.Files) == 0 {
		var why string
		switch {
		case c.Lora != "":
			why = "has no LoRA .safetensors file at the repository root."
		case c.Pipeline != "":
			why = "has no " + ImagePipelines[c.Pipeline].Label + " diffusion model file sd.cpp can load (GGUF or BF16/FP16/FP8 safetensors at the root or in diffusion_models/)."
		case kind == "image" && slices.ContainsFunc(info.Siblings, func(s Sibling) bool { return s.RFilename == "model_index.json" }):
			why = noDiffusersFiles
		default:
			why = noFiles[kind]
		}
		return nil, &Error{Message: info.ID + " " + why}
	}
	switch {
	case c.Lora != "":
		c.Default = loraDefault(c.Files)
	case c.Pipeline != "":
		c.Default = pipelineDefault(c.Files)
	default:
		c.Default = defaultChoice(kind, c.Files)
	}
	return c, nil
}

func selectFile(repo string, c *Choices, selector string) (File, error) {
	list := func() string {
		var lines []string
		for _, f := range c.Files {
			l := "  " + f.Path
			if q := QuantOf(f.Path); q != "" {
				l += " (" + q + ")"
			}
			lines = append(lines, l)
		}
		return strings.Join(lines, "\n")
	}
	if selector != "" {
		wanted := strings.ToLower(selector)
		for _, f := range c.Files {
			base := path.Base(f.Path)
			if strings.ToLower(f.Path) == wanted || strings.ToLower(base) == wanted ||
				strings.ToLower(weightExtRe.ReplaceAllString(base, "")) == wanted || ModelIDFor(f.Path, c.Kind) == wanted {
				return f, nil
			}
		}
		var byQuant []File
		for _, f := range c.Files {
			if strings.ToLower(QuantOf(f.Path)) == wanted && QuantOf(f.Path) != "" {
				byQuant = append(byQuant, f)
			}
		}
		if len(byQuant) == 1 {
			return byQuant[0], nil
		}
		if len(byQuant) > 1 {
			return File{}, &Error{Message: fmt.Sprintf("More than one file in %s is %s; name the file instead:\n%s", repo, strings.ToUpper(selector), list())}
		}
		return File{}, &Error{Message: fmt.Sprintf("%s has no %s file or quant named %q. Choose one of:\n%s", repo, noun[c.Kind], selector, list())}
	}
	if c.Default != nil {
		return *c.Default, nil
	}
	return File{}, &Error{Message: fmt.Sprintf("%s has %d %s files; pick one with hf:%s:<file-or-quant>:\n%s", repo, len(c.Files), noun[c.Kind], repo, list())}
}

func shardsOf(files []File, first File) ([]File, error) {
	m := shardRe.FindStringSubmatchIndex(first.Path)
	if m == nil {
		return []File{first}, nil
	}
	prefix := first.Path[:m[0]]
	total := first.Path[m[4]:m[5]]
	var shards []File
	for _, f := range files {
		sm := shardRe.FindStringSubmatchIndex(f.Path)
		if sm != nil && f.Path[sm[4]:sm[5]] == total && f.Path[:sm[0]] == prefix {
			shards = append(shards, f)
		}
	}
	sort.SliceStable(shards, func(i, j int) bool { return collate.Compare(shards[i].Path, shards[j].Path) < 0 })
	n, _ := strconv.Atoi(total)
	if len(shards) != n {
		return nil, &Error{Message: fmt.Sprintf("%s is split into %d parts but %d are in the repository.", first.Path, n, len(shards))}
	}
	return shards, nil
}

var (
	mmprojRe = regexp.MustCompile(`(?i)mmproj`)
	f16Re    = regexp.MustCompile(`(?i)(^|[^b])f16`)
)

func selectProjector(files []File) *File {
	var projectors []File
	for _, f := range files {
		if ggufExtRe.MatchString(f.Path) && mmprojRe.MatchString(path.Base(f.Path)) {
			projectors = append(projectors, f)
		}
	}
	for _, f := range projectors {
		if f16Re.MatchString(path.Base(f.Path)) {
			return &f
		}
	}
	if len(projectors) > 0 {
		return &projectors[0]
	}
	return nil
}

func sizeOf(name string) string {
	m := sizeRe.FindStringSubmatch(name)
	if m == nil {
		return "unknown"
	}
	return m[1] + strings.ToUpper(m[2])
}

// takenFilenames are the files other catalog models install, with their
// checksums; a file with the same name and checksum is shared.
func takenFilenames(cat *catalog.Catalog, modelID string) map[string]string {
	out := map[string]string{}
	for _, s := range cat.All() {
		if s.ModelID == modelID {
			continue
		}
		for _, a := range s.Artifacts {
			out[a.Filename] = a.SHA256
		}
	}
	return out
}
