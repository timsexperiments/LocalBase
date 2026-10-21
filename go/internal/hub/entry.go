package hub

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/collate"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Reference is `hf:<org>/<repo>[:<file-or-quant>]`.
type Reference struct {
	Repo     string `json:"repo"`
	Selector string `json:"selector,omitempty"`
}

var referenceRe = regexp.MustCompile(`^hf:([^:]+)(?::(.+))?$`)

// ParseReference parses an hf: reference.
func ParseReference(value string) (Reference, error) {
	m := referenceRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return Reference{}, &Error{Message: fmt.Sprintf("Expected hf:<org>/<repo>[:<file-or-quant>], got %q.", value)}
	}
	if !ValidRepoID(m[1]) {
		return Reference{}, &Error{Message: fmt.Sprintf("%q is not a Hugging Face repository name (expected <org>/<repo>).", m[1])}
	}
	return Reference{Repo: m[1], Selector: m[2]}, nil
}

// File is a repository file stored in LFS (size and sha256 known).
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var (
	shardRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	// quantRe is TS QUANT with its trailing lookahead (?=[-_.]|$) consumed.
	quantRe         = regexp.MustCompile(`(?i)(?:^|[-_.])((?:IQ|Q)\d(?:_[A-Z0-9]+)*|BF16|F16|F32|MXFP4(?:_MOE)?)(?:[-_.]|$)`)
	weightExtRe     = regexp.MustCompile(`(?i)\.(gguf|safetensors|ckpt|bin)$`)
	auxiliaryRe     = regexp.MustCompile(`(?i)(?:^|[/_.-])(?:mmproj|projectors?|mtp|draft|imatrix|importance[-_]matrix|adapters?|lora|tokenizers?|vocab)(?:[/_.-]|$)`)
	preferredQuants = []string{"Q4_K_M", "Q4_K_S", "Q5_K_M", "Q8_0"}
	imageQuants     = []string{"Q8_0", "Q5_0", "Q5_1", "Q4_K", "Q4_0"}
	pipelineQuants  = []string{"Q4_0", "Q4_K", "Q4_K_S", "Q5_0", "Q8_0"}
)

const minImageCheckpointBytes = 500_000_000

// lfsFiles lists the repository's LFS files.
func lfsFiles(info *ModelInfo) []File {
	var out []File
	for _, s := range info.Siblings {
		if s.LFS != nil {
			out = append(out, File{s.RFilename, s.LFS.Size, strings.ToLower(s.LFS.SHA256)})
		}
	}
	return out
}

// QuantOf is the quantization a weights file name declares ("" if none).
func QuantOf(p string) string {
	m := quantRe.FindStringSubmatch(weightExtRe.ReplaceAllString(path.Base(p), ""))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

func auxiliaryGGUF(p string) bool {
	return strings.Contains(strings.ToLower(path.Base(p)), "mmproj") || auxiliaryRe.MatchString(p)
}

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

var (
	slugInvalidRe = regexp.MustCompile(`[^a-z0-9._-]+`)
	slugTrimRe    = regexp.MustCompile(`^[-._]+|[-._]+$`)
	ggmlPrefixRe  = regexp.MustCompile(`(?i)^ggml[-_]?`)
	// sizeRe is TS sizeOf with its trailing lookahead consumed.
	sizeRe      = regexp.MustCompile(`(?i)(?:^|[-_.])(\d+(?:\.\d+)?)([BM])(?:[-_.]|$)`)
	whisperSize = regexp.MustCompile(`(?i)(tiny|base|small|medium|large(?:-v\d)?(?:-turbo)?)`)
	modelIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	ggufSuffix  = regexp.MustCompile(`(?i)[-_]?GGUF$`)
	coderRe     = regexp.MustCompile(`(?i)coder|code`)
	englishRe   = regexp.MustCompile(`(?i)(^|[-_.])en($|[-_.])`)
)

func slug(v string) string {
	return slugTrimRe.ReplaceAllString(slugInvalidRe.ReplaceAllString(strings.ToLower(v), "-"), "")
}

// ModelIDFor is the catalog id derived from a weights file name.
func ModelIDFor(p, kind string) string {
	stem := weightExtRe.ReplaceAllString(shardRe.ReplaceAllString(path.Base(p), ""), "")
	if kind == "stt" {
		variant := slug(ggmlPrefixRe.ReplaceAllString(stem, ""))
		if strings.Contains(variant, "whisper") {
			return variant
		}
		return "whisper-" + variant
	}
	return slug(stem)
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

// Entry is a generated user catalog entry.
type Entry struct {
	Spec          *jsjson.Object `json:"spec"`
	ModelID       string         `json:"modelId"`
	Kind          string         `json:"kind"`
	Repo          string         `json:"repo"`
	Revision      string         `json:"revision"`
	License       *string        `json:"license"`
	PipelineTag   *string        `json:"pipelineTag"`
	Weights       []File         `json:"weights"`
	Projector     *File          `json:"projector"`
	Detected      []string       `json:"detected"`
	ContextLength *int64         `json:"contextLength"`
}

func storageGB(bytes int64) float64 {
	return math.Max(0.01, jsRound(float64(bytes)/1e9*100)/100)
}

// jsRound is JavaScript's Math.round.
func jsRound(x float64) float64 {
	f := math.Floor(x)
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}

func obj(kv ...any) *jsjson.Object {
	o := jsjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func strs(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// copyObject copies o (shallow) so a spread can override keys in place.
func copyObject(o *jsjson.Object) *jsjson.Object {
	out := jsjson.NewObject()
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		out.Set(k, v)
	}
	return out
}

// GenerateEntry builds a user catalog entry for one weights file (TS
// generateEntry).
func GenerateEntry(cat *catalog.Catalog, info *ModelInfo, ref Reference, modelID string) (*Entry, error) {
	repo := info.ID
	available, err := EntryChoices(info)
	if err != nil {
		return nil, err
	}
	kind := available.Kind
	files := lfsFiles(info)
	first, err := selectFile(repo, available, ref.Selector)
	if err != nil {
		return nil, err
	}
	weights := []File{first}
	if kind == "llm" {
		if weights, err = shardsOf(files, first); err != nil {
			return nil, err
		}
	}
	repoName := strings.SplitN(repo, "/", 2)[1]
	if modelID == "" {
		if available.Lora != "" {
			modelID = slug(repoName)
		} else {
			modelID = ModelIDFor(first.Path, kind)
		}
	}
	if !modelIDRe.MatchString(modelID) {
		return nil, &Error{Message: fmt.Sprintf("%q is not a usable model id; choose one with --id (lowercase letters, digits, \".\", \"_\" and \"-\").", modelID)}
	}
	taken := takenFilenames(cat, modelID)
	filenameFor := func(f File, sharded bool) string {
		name := path.Base(f.Path)
		if owner, ok := taken[name]; ok && owner != f.SHA256 && !sharded {
			return modelID + "-" + name
		}
		return name
	}
	for _, f := range weights {
		if owner, ok := taken[path.Base(f.Path)]; ok && shardRe.MatchString(f.Path) && owner != f.SHA256 {
			return nil, &Error{Message: path.Base(f.Path) + " is already installed for another catalog model."}
		}
	}
	license := License(info.Tags, info.CardLicense)
	stem := weightExtRe.ReplaceAllString(path.Base(first.Path), "")
	var totalWeight int64
	for _, f := range weights {
		totalWeight += f.Size
	}
	licenseText := license
	if licenseText == "" {
		licenseText = "not stated"
	}
	notes := fmt.Sprintf("Added with local-base models add hf:%s:%s at revision %s", repo, path.Base(first.Path), info.SHA)
	if info.PipelineTag != "" {
		notes += " (pipeline " + info.PipelineTag + ")"
	}
	notes += "."
	common := [][2]any{{"modelId", modelID}, {"provider", strings.SplitN(repo, "/", 2)[0]}, {"version", info.SHA[:7]},
		{"source", PublicEndpoint + "/" + repo}, {"repositoryRevision", info.SHA}, {"commercialStatus", CommercialStatus(license)},
		{"catch", "License: " + licenseText + ". Added from Hugging Face; not qualified by LocalBase."}, {"notes", notes}}
	spec := jsjson.NewObject()
	for _, kv := range common {
		spec.Set(kv[0].(string), kv[1])
	}
	primary := obj("sourcePath", first.Path, "filename", filenameFor(first, false), "expectedSizeBytes", float64(first.Size),
		"sha256", first.SHA256, "role", "primary")
	e := &Entry{ModelID: modelID, Kind: kind, Repo: repo, Revision: info.SHA, License: strPtr(license),
		PipelineTag: strPtr(info.PipelineTag), Weights: weights, Detected: []string{}, Spec: spec}

	switch {
	case kind == "stt":
		variant := ggmlPrefixRe.ReplaceAllString(stem, "")
		size := "unknown"
		if m := whisperSize.FindStringSubmatch(variant); m != nil {
			size = strings.ToLower(m[1])
		}
		quant := QuantOf(first.Path)
		if quant == "" {
			quant = "F16"
		}
		caps := strs("speech-to-text")
		if !englishRe.MatchString(variant) {
			caps = append(caps, "multilingual")
		}
		for _, kv := range [][2]any{{"displayName", "Whisper " + variant}, {"kind", "stt"}, {"family", "Whisper"}, {"size", size},
			{"quant", quant}, {"storageGb", storageGB(first.Size)}, {"artifacts", []any{primary}},
			{"inputModalities", strs("audio")}, {"outputModalities", strs("text")}, {"capabilities", caps}} {
			spec.Set(kv[0].(string), kv[1])
		}
		return e, nil
	case kind == "image" && available.Lora != "":
		return e, loraEntry(cat, e, available.Lora, info, first, modelID, repoName, taken)
	case kind == "image" && available.Pipeline != "":
		return e, pipelineEntry(cat, e, available.Pipeline, primary, first, repoName, stem)
	case kind == "image":
		prediction := VPrediction(info, stem)
		display := repoName
		if len(available.Files) > 1 {
			display = repoName + " (" + stem + ")"
		}
		launch := obj("diffusionFlashAttention", false)
		if prediction != "" {
			launch.Set("prediction", "v")
		}
		for _, kv := range [][2]any{{"displayName", display}, {"kind", "image"}, {"family", repoName},
			{"size", jsjson.ToFixed1(float64(first.Size)/1e9) + "GB"},
			{"quant", imageQuant(first.Path, stem)}, {"storageGb", storageGB(first.Size)}, {"artifacts", []any{primary}},
			{"inputModalities", strs("text")}, {"outputModalities", strs("image")}, {"imageLaunchOptions", launch},
			{"capabilities", strs("text-to-image", "image-to-image")}} {
			spec.Set(kv[0].(string), kv[1])
		}
		if prediction != "" {
			spec.Set("notes", notes+" Launches with --prediction v because "+prediction+".")
		}
		return e, nil
	}

	// A language model.
	likely := LikelyKind(info.PipelineTag, info.Tags, repo, info.Library)
	projector := selectProjector(files)
	projectorName := ""
	if projector != nil {
		projectorName = path.Base(projector.Path)
		// Projectors share generic names across repositories.
		if !strings.Contains(strings.ToLower(projectorName), modelID) {
			projectorName = modelID + "-" + projectorName
		}
	}
	family := ggufSuffix.ReplaceAllString(repoName, "")
	if family == "" {
		family = repoName
	}
	quant := QuantOf(first.Path)
	if quant == "" {
		quant = "unknown"
	}
	decision := IsDecisionRepository(info.Tags, info.GGUF.Architecture)
	vision := projector != nil && (likely.Label == "llm (vision)" || (decision && info.PipelineTag == "image-text-to-text") ||
		slices.ContainsFunc(info.Tags, func(t string) bool { return t == "vision" || t == "multimodal" || t == "image-text-to-text" }))
	coder := coderRe.MatchString(repo) || slices.Contains(info.Tags, "code")
	if decision {
		e.Detected = []string{"decision"}
	} else if info.GGUF.ChatTemplate != "" {
		op := runtime.AnalyzeChatTemplate(info.GGUF.ChatTemplate)
		for _, id := range []string{"tool-calling", "reasoning"} {
			if op[id] {
				e.Detected = append(e.Detected, id)
			}
		}
	}
	var ctxLen any
	if info.GGUF.ContextLength > 0 {
		n := info.GGUF.ContextLength
		e.ContextLength = &n
		ctxLen = float64(n)
	}
	total := totalWeight
	if vision {
		total += projector.Size
	}
	size := sizeOf(repoName)
	if size == "unknown" {
		size = sizeOf(first.Path)
	}
	sharded := len(weights) > 1
	var artifacts []any
	for i, f := range weights {
		role := "supplementary"
		if i == 0 {
			role = "primary"
		}
		artifacts = append(artifacts, obj("sourcePath", f.Path, "filename", filenameFor(f, sharded), "expectedSizeBytes", float64(f.Size),
			"sha256", f.SHA256, "role", role))
	}
	inputs := strs("text")
	if vision {
		artifacts = append(artifacts, obj("sourcePath", projector.Path, "filename", projectorName, "expectedSizeBytes", float64(projector.Size),
			"sha256", projector.SHA256, "role", "supplementary"))
		inputs = strs("text", "image")
		e.Projector = projector
	}
	for _, kv := range [][2]any{{"displayName", family + " " + quant}, {"kind", "llm"}, {"family", family}, {"size", size},
		{"quant", quant}, {"storageGb", storageGB(total)}, {"artifacts", artifacts}, {"inputModalities", inputs},
		{"outputModalities", strs("text")}, {"contextWindowTokens", ctxLen}} {
		spec.Set(kv[0].(string), kv[1])
	}
	if decision {
		spec.Set("llmRuntime", obj("capability", "decision-only"))
		spec.Set("capabilities", strs("decision"))
	} else {
		caps := strs("text-generation")
		if coder {
			caps = append(caps, "code-generation")
		}
		spec.Set("capabilities", caps)
	}
	return e, nil
}

var (
	bf16Re = regexp.MustCompile(`(?i)(^|[-_.])bf16($|[-_.])`)
	fp16Re = regexp.MustCompile(`(?i)(^|[-_.])fp16($|[-_.])`)
	fp8Re  = regexp.MustCompile(`(?i)(^|[-_.])fp8`)
	fp32Re = regexp.MustCompile(`(?i)(^|[-_.])fp32($|[-_.])`)
	// vPredRe is TS V_PRED_NAME with its negative lookahead (?![a-z0-9])
	// written as a consumed non-alphanumeric or the end.
	vPredRe   = regexp.MustCompile(`(?i)v[-_]?pred|768[-_]v([^a-z0-9]|$)|v2[-_.]?1[-_]768`)
	vPredTag  = regexp.MustCompile(`(?i)^v[-_]?pred(iction)?$`)
	schnellRe = regexp.MustCompile(`(?i)schnell`)
)

func imageQuant(p, stem string) string {
	if q := QuantOf(p); q != "" {
		return q
	}
	switch {
	case bf16Re.MatchString(stem):
		return "BF16"
	case fp16Re.MatchString(stem):
		return "F16"
	case fp8Re.MatchString(stem):
		return "FP8"
	case fp32Re.MatchString(stem):
		return "F32"
	}
	return "unknown"
}

// VPrediction is why a single-file checkpoint needs --prediction v ("" for
// an ordinary epsilon model). Names and tags win over the scheduler config.
func VPrediction(info *ModelInfo, stem string) string {
	if vPredRe.MatchString(info.ID) || vPredRe.MatchString(stem) {
		return "the name marks a v-prediction checkpoint"
	}
	if slices.ContainsFunc(info.Tags, vPredTag.MatchString) {
		return "the repository is tagged v-prediction"
	}
	if info.SchedulerPrediction == "v" {
		return "scheduler_config.json sets prediction_type v_prediction"
	}
	return ""
}

var commercialOrder = []string{"open", "conditional", "prohibited"}

func stricterStatus(a, b string) string {
	if slices.Index(commercialOrder, a) >= slices.Index(commercialOrder, b) {
		return a
	}
	return b
}

// templateArtifacts are a built-in entry's artifacts with their source made
// explicit, as jsjson objects.
func templateArtifacts(t *catalog.Spec, keep func(catalog.Artifact) bool) ([]any, int64) {
	var out []any
	var total int64
	for _, a := range t.Artifacts {
		if keep != nil && !keep(a) {
			continue
		}
		o := jsjson.FromGo(a).(*jsjson.Object)
		if _, ok := o.Get("source"); !ok {
			o.Set("source", obj("repositoryUrl", t.Source, "revision", t.RepositoryRevision))
		}
		out = append(out, o)
		total += a.ExpectedSizeBytes
	}
	return out, total
}

func capabilityIDs(t *catalog.Spec) []any {
	var out []any
	for _, c := range t.Capabilities {
		out = append(out, c.ID)
	}
	return out
}

func loraEntry(cat *catalog.Catalog, e *Entry, base string, info *ModelInfo, first File, modelID, repoName string, taken map[string]string) error {
	profile := ImageLoraBases[base]
	t := pipelineTemplate(cat, profile.Template)
	if t == nil {
		return &Error{Message: fmt.Sprintf("The %s base model (%s) is missing from this LocalBase build.", profile.Label, profile.Template)}
	}
	name := path.Base(first.Path)
	filename := modelID + "-" + name
	if owner, ok := taken[name]; strings.Contains(name, modelID) && (!ok || owner == first.SHA256) {
		filename = name
	}
	baseArtifacts, baseBytes := templateArtifacts(t, nil)
	lora := obj("sourcePath", first.Path, "filename", filename, "expectedSizeBytes", float64(first.Size), "sha256", first.SHA256, "role", "supplementary")
	trigger := ""
	if v, ok := info.CardData["instance_prompt"].(string); ok {
		if tr := strings.TrimSpace(v); tr != "" && len([]rune(tr)) <= 200 {
			trigger = tr
		}
	}
	spec := e.Spec
	commonCatch, _ := spec.Get("catch")
	commonNotes, _ := spec.Get("notes")
	commonStatus, _ := spec.Get("commercialStatus")
	spec.Set("displayName", repoName+" ("+profile.Label+" LoRA)")
	spec.Set("kind", "image")
	spec.Set("family", t.Family)
	spec.Set("size", t.Size)
	spec.Set("quant", t.Quant)
	spec.Set("storageGb", storageGB(first.Size+baseBytes))
	spec.Set("artifacts", append(baseArtifacts, lora))
	if t.ImageRuntime != nil {
		spec.Set("imageRuntime", jsjson.FromGo(t.ImageRuntime))
	}
	spec.Set("inputModalities", strs(t.InputModalities...))
	spec.Set("outputModalities", strs(t.OutputModalities...))
	launch := jsjson.NewObject()
	if t.ImageLaunchOptions != nil {
		launch = copyObject(jsjson.FromGo(t.ImageLaunchOptions).(*jsjson.Object))
	}
	loraOpt := obj("artifactFilename", filename, "defaultStrength", float64(1))
	if trigger != "" {
		loraOpt.Set("trigger", trigger)
	}
	launch.Set("lora", loraOpt)
	spec.Set("imageLaunchOptions", launch)
	spec.Set("capabilities", capabilityIDs(t))
	spec.Set("commercialStatus", stricterStatus(commonStatus.(string), t.CommercialStatus))
	spec.Set("catch", fmt.Sprintf("%s Base model %s: %s", commonCatch, t.ModelID, t.Catch))
	notes := fmt.Sprintf("%s LoRA for %s: sd-server applies %s to the weights, VAE and encoders pinned by the %s entry.", commonNotes, profile.Label, filename, t.ModelID)
	if trigger != "" {
		notes += fmt.Sprintf(" The model card's trigger phrase is %q.", trigger)
	}
	spec.Set("notes", notes)
	return nil
}

func pipelineEntry(cat *catalog.Catalog, e *Entry, pipeline string, primary *jsjson.Object, first File, repoName, stem string) error {
	profile := ImagePipelines[pipeline]
	t := pipelineTemplate(cat, profile.Template)
	if t == nil || t.ImageRuntime == nil || t.ImageRuntime.Kind == "checkpoint" {
		return &Error{Message: fmt.Sprintf("The %s pipeline profile (%s) is missing from this LocalBase build.", profile.Label, profile.Template)}
	}
	supplementary, suppBytes := templateArtifacts(t, func(a catalog.Artifact) bool { return a.Role == "supplementary" })
	steps := t.ImageRuntime.Generation.Steps
	if pipeline == "flux1" && !schnellRe.MatchString(repoName+" "+stem) {
		// Guidance-distilled FLUX.1 checkpoints need more than four steps.
		steps = 20
	}
	rt := jsjson.FromGo(t.ImageRuntime).(*jsjson.Object)
	arts, _ := rt.Get("artifacts")
	arts2 := copyObject(arts.(*jsjson.Object))
	fn, _ := primary.Get("filename")
	arts2.Set("diffusionModel", fn)
	rt.Set("artifacts", arts2)
	gen, _ := rt.Get("generation")
	gen2 := copyObject(gen.(*jsjson.Object))
	gen2.Set("steps", float64(steps))
	rt.Set("generation", gen2)
	notes, _ := e.Spec.Get("notes")
	spec := e.Spec
	spec.Set("displayName", repoName+" ("+stem+")")
	spec.Set("kind", "image")
	spec.Set("family", profile.Family)
	spec.Set("size", profile.Size)
	spec.Set("quant", imageQuant(first.Path, stem))
	spec.Set("storageGb", storageGB(first.Size+suppBytes))
	spec.Set("artifacts", append([]any{primary}, supplementary...))
	spec.Set("imageRuntime", rt)
	spec.Set("inputModalities", strs(t.InputModalities...))
	spec.Set("outputModalities", strs(t.OutputModalities...))
	if t.ImageLaunchOptions != nil {
		spec.Set("imageLaunchOptions", jsjson.FromGo(t.ImageLaunchOptions))
	}
	spec.Set("capabilities", capabilityIDs(t))
	spec.Set("notes", fmt.Sprintf("%s %s pipeline: the VAE and text encoders are the ones pinned by the %s profile.", notes, profile.Label, t.ModelID))
	return nil
}

// pipelineTemplate is the pinned entry a pipeline or LoRA profile borrows
// its encoders, VAE and runtime profile from. A template that left the
// built-in catalog is the pinned former entry, so discovering those
// repositories keeps working (TS pipelineTemplate, #368).
func pipelineTemplate(cat *catalog.Catalog, modelID string) *catalog.Spec {
	if s := cat.ByID(modelID); s != nil {
		return s
	}
	return catalog.FormerEntry(modelID)
}
