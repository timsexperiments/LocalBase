package hub

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

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
