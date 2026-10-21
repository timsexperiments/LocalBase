package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Capability detection and resolution, ported from capability-detection.ts.
// Precedence per capability: verified > runtime > detected > declared, and a
// catalog "unsupported" mark yields only to a verified round trip.

// Opinions maps a capability to true (supported) or false (ruled out); a
// missing key means the source cannot tell.
type Opinions map[string]bool

// Detected is what the installed files and engine facts say.
type Detected struct {
	Capabilities    Opinions
	Order           []string // keys of Capabilities in insertion order, as TS objects keep them
	InputModalities []string // nil when the source cannot tell
	Evidence        []string
}

func (d *Detected) set(k string, v bool) {
	if d.Capabilities == nil {
		d.Capabilities = Opinions{}
	}
	if _, ok := d.Capabilities[k]; !ok {
		d.Order = append(d.Order, k)
	}
	d.Capabilities[k] = v
}

// opinions builds a Detected from (id, bool) pairs, keeping their order.
func opinions(evidence []string, pairs ...any) Detected {
	d := Detected{Capabilities: Opinions{}, Evidence: evidence}
	for i := 0; i < len(pairs); i += 2 {
		d.set(pairs[i].(string), pairs[i+1].(bool))
	}
	return d
}

// Mismatch records sources that disagree, for diagnostics.
type Mismatch struct {
	ID                string `json:"id"`
	Declared          bool   `json:"declared"`
	Detected          *bool  `json:"detected,omitempty"`
	Runtime           *bool  `json:"runtime,omitempty"`
	Verified          *bool  `json:"verified,omitempty"`
	Effective         bool   `json:"effective"`
	UnsupportedReason string `json:"unsupportedReason,omitempty"`
}

// Resolved is a model's effective capability set.
type Resolved struct {
	Capabilities    []catalog.Capability
	Mismatches      []Mismatch
	InputModalities []string
	InputSource     string
	Evidence        []string
	RuntimeBuild    string
}

func (r Resolved) Has(id string) bool {
	for _, c := range r.Capabilities {
		if c.ID == id {
			return true
		}
	}
	return false
}

var (
	toolMarkers      = regexp.MustCompile(`\btools\b|\btool_calls\b`)
	reasoningMarkers = regexp.MustCompile(`<think>|\benable_thinking\b|\breasoning_content\b|\breasoning_effort\b|\bthinking\b`)
)

// AnalyzeChatTemplate: a template that never renders tools cannot pass them,
// so a missing marker rules tool calling out. Reasoning is only asserted.
func AnalyzeChatTemplate(t string) Opinions {
	o := Opinions{"tool-calling": toolMarkers.MatchString(t)}
	if reasoningMarkers.MatchString(t) {
		o["reasoning"] = true
	}
	return o
}

func engineCapabilities(s *catalog.Spec) Detected {
	switch {
	case s.Kind == "llm":
		if s.Projector() != nil {
			return Detected{Capabilities: Opinions{}}
		}
		return Detected{Capabilities: Opinions{}, InputModalities: []string{"text"}}
	case s.Kind == "image" && s.ImageRuntime != nil:
		rt := s.ImageRuntime
		return opinions([]string{"image profile " + rt.Kind + ": text-to-image only"}, "text-to-image", true, "image-to-image", false)
	case s.Kind == "video" && s.VideoRuntime != nil:
		rt := s.VideoRuntime
		return opinions([]string{"video profile " + rt.Mode}, "text-to-video", rt.Mode == "t2v" || rt.Mode == "ti2v",
			"image-to-video", rt.Mode == "ti2v", "speech-to-video", rt.Mode == "s2v")
	}
	return Detected{Capabilities: Opinions{}}
}

const longContextTokens = 128 * 1024

func ggufCapabilities(s *catalog.Spec, h *memory.CapabilityHeader) Detected {
	d := Detected{Capabilities: Opinions{}}
	if h.Architecture != "" {
		d.Evidence = append(d.Evidence, "gguf: "+h.Architecture)
	}
	if h.ContextLength > 0 {
		d.set("long-context", h.ContextLength >= longContextTokens)
		d.Evidence = append(d.Evidence, "gguf: context "+itoa(h.ContextLength))
	}
	d.set("mixture-of-experts", h.ExpertCount > 0)
	if h.ExpertCount > 0 {
		d.Evidence = append(d.Evidence, "gguf: "+itoa(h.ExpertCount)+" experts")
	}
	switch {
	case h.DecisionType != "":
		d.set("decision", true)
		d.Evidence = append(d.Evidence, "gguf: decision type "+h.DecisionType)
	case s.IsDecision():
		d.set("decision", false)
	case s.IsEmbedding():
		if h.PoolingType != nil {
			d.set("embeddings", true)
			d.Evidence = append(d.Evidence, "gguf: pooling type "+itoa(*h.PoolingType))
		}
	case h.ChatTemplate != "":
		o := AnalyzeChatTemplate(h.ChatTemplate)
		d.set("tool-calling", o["tool-calling"])
		if o["reasoning"] {
			d.set("reasoning", true)
		}
		d.Evidence = append(d.Evidence, "gguf: chat template")
	}
	return d
}

// --- file detection -----------------------------------------------------------

const (
	whisperMagic        = 0x67676d6c
	whisperEnglishVocab = 51864
	maxSafetensorsHdr   = 32 << 20
)

func whisperDetect(path string) *Detected {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var b [48]byte
	if _, err := io.ReadFull(f, b[:]); err != nil || binary.LittleEndian.Uint32(b[:]) != whisperMagic {
		return nil
	}
	field := func(i int) int { return int(int32(binary.LittleEndian.Uint32(b[4+i*4:]))) }
	vocab, audio, text := field(0), field(4), field(8)
	if vocab <= 0 || audio <= 0 || text <= 0 {
		return nil
	}
	multi := vocab > whisperEnglishVocab
	turbo := audio == 32 && text == 4
	d := opinions([]string{"whisper: vocab " + itoa(vocab) + ", " + itoa(audio) + "/" + itoa(text) + " layers"}, "speech-to-text", true, "multilingual", multi, "translation", multi && !turbo)
	return &d
}

func safetensorsNames(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var p [8]byte
	if _, err := io.ReadFull(f, p[:]); err != nil {
		return nil
	}
	n := binary.LittleEndian.Uint64(p[:])
	if n == 0 || n > maxSafetensorsHdr {
		return nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil
	}
	var hdr map[string]json.RawMessage
	if json.Unmarshal(b, &hdr) != nil {
		return nil
	}
	var names []string
	for k := range hdr {
		if k != "__metadata__" {
			names = append(names, k)
		}
	}
	return names
}

func checkpointDetect(names []string) *Detected {
	has := func(prefix string) bool {
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				return true
			}
		}
		return false
	}
	if !has("model.diffusion_model.input_blocks.") || !has("first_stage_model.") {
		return nil
	}
	kind := "SD"
	if has("conditioner.embedders.1.") {
		kind = "SDXL"
	}
	d := opinions([]string{"safetensors: " + kind + " UNet checkpoint"}, "text-to-image", true, "image-to-image", true)
	return &d
}

var img2imgPipeline = regexp.MustCompile(`^StableDiffusion(XL)?(Img2Img|Inpaint)?Pipeline$|Img2Img|Inpaint`)

func diffusersDetect(path string) *Detected {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var idx struct {
		ClassName string `json:"_class_name"`
	}
	if json.Unmarshal(b, &idx) != nil || idx.ClassName == "" {
		return nil
	}
	d := opinions([]string{"model_index.json: " + idx.ClassName}, "text-to-image", true, "image-to-image", img2imgPipeline.MatchString(idx.ClassName))
	return &d
}

// fileCache memoizes detection per path while size and mtime are unchanged;
// GGUF headers are re-read (they are small and the OS caches them).
var fileCache sync.Map // path -> cachedDetect

type cachedDetect struct {
	sig string
	d   *Detected
}

func cachedFile(path string, detect func() *Detected) *Detected {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	sig := itoa(int(info.Size())) + ":" + info.ModTime().String()
	if c, ok := fileCache.Load(path); ok && c.(cachedDetect).sig == sig {
		return c.(cachedDetect).d
	}
	d := detect()
	if d != nil {
		fileCache.Store(path, cachedDetect{sig, d})
	}
	return d
}

func detectFromFiles(s *catalog.Spec, dir string) *Detected {
	primary := filepath.Join(dir, s.Primary().Filename)
	switch s.Kind {
	case "llm":
		h := memory.ReadCapabilityHeader(primary)
		if h == nil {
			return nil
		}
		d := ggufCapabilities(s, h)
		d.InputModalities = []string{"text"}
		if a := s.Projector(); a != nil {
			if p := memory.ReadCapabilityHeader(filepath.Join(dir, a.Filename)); p != nil {
				if p.HasVisionEncoder {
					d.InputModalities = append(d.InputModalities, "image")
					d.Evidence = append(d.Evidence, "projector: vision encoder")
				}
				if p.HasAudioEncoder {
					d.InputModalities = append(d.InputModalities, "audio")
					d.Evidence = append(d.Evidence, "projector: audio encoder")
				}
			}
		}
		return &d
	case "stt":
		return cachedFile(primary, func() *Detected { return whisperDetect(primary) })
	case "image":
		for _, a := range s.Artifacts {
			if strings.HasSuffix(a.Filename, "model_index.json") {
				p := filepath.Join(dir, a.Filename)
				return cachedFile(p, func() *Detected { return diffusersDetect(p) })
			}
		}
		if s.ImageRuntime != nil || !strings.HasSuffix(primary, ".safetensors") {
			return nil
		}
		return cachedFile(primary, func() *Detected {
			if names := safetensorsNames(primary); names != nil {
				return checkpointDetect(names)
			}
			return nil
		})
	}
	return nil
}

// DetectCapabilities intersects engine facts with installed-file evidence.
// Incomplete installs get engine facts only. Engine "true" fills gaps, file
// evidence overrides it, and engine "false" wins.
func DetectCapabilities(s *catalog.Spec, dir string) *Detected {
	engine := engineCapabilities(s)
	var files *Detected
	if complete, _ := s.Installation(dir); complete {
		files = detectFromFiles(s, dir)
	}
	out := Detected{Capabilities: Opinions{}}
	for _, k := range engine.Order {
		if engine.Capabilities[k] {
			out.set(k, true)
		}
	}
	if files != nil {
		for _, k := range files.Order {
			out.set(k, files.Capabilities[k])
		}
		out.Evidence = append(out.Evidence, files.Evidence...)
	}
	for _, k := range engine.Order {
		if !engine.Capabilities[k] {
			out.set(k, false)
		}
	}
	out.InputModalities = engine.InputModalities
	if out.InputModalities == nil && files != nil {
		out.InputModalities = files.InputModalities
	}
	out.Evidence = append(out.Evidence, engine.Evidence...)
	if len(out.Capabilities) == 0 && out.InputModalities == nil && len(out.Evidence) == 0 {
		return nil
	}
	return &out
}

// ResolveCapabilities applies the precedence rules with the spec's own
// unsupported marks. rt may be nil.
func ResolveCapabilities(s *catalog.Spec, det *Detected, rt *store.RuntimeCapabilities) Resolved {
	return ResolveCapabilitiesWith(s, det, rt, s.UnsupportedCapabilities)
}

// ResolveCapabilitiesWith resolves with the effective unsupported marks (the
// spec's, then the feed marks that apply).
func ResolveCapabilitiesWith(s *catalog.Spec, det *Detected, rt *store.RuntimeCapabilities, marks []catalog.Unsupported) Resolved {
	detected, runtimeOp := Opinions{}, Opinions{}
	if det != nil {
		detected = det.Capabilities
	}
	if rt != nil {
		runtimeOp = rt.Capabilities
	}
	verified := Opinions{} // runtime round-trip verification is not recorded yet
	unsupported := map[string]string{}
	for _, u := range marks {
		if _, seen := unsupported[u.ID]; !seen {
			unsupported[u.ID] = u.Reason
		}
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, c := range s.Capabilities {
		add(c.ID)
	}
	for _, u := range marks {
		add(u.ID)
	}
	if det != nil {
		for _, id := range det.Order {
			add(id)
		}
	}
	// Runtime rows are written in this order by both implementations.
	for _, id := range append([]string{"tool-calling", "reasoning", "decision"}, sortedKeys(runtimeOp)...) {
		if _, ok := runtimeOp[id]; ok {
			add(id)
		}
	}
	bp := func(m Opinions, id string) *bool {
		if v, ok := m[id]; ok {
			return &v
		}
		return nil
	}
	var res Resolved
	for _, id := range ids {
		declared := s.HasCapability(id)
		dv, rv, vv := bp(detected, id), bp(runtimeOp, id), bp(verified, id)
		if reason, ok := unsupported[id]; ok {
			effective := vv != nil && *vv
			if effective {
				res.Capabilities = append(res.Capabilities, catalog.Capability{ID: id, Source: "runtime"})
			}
			if effective || (dv != nil && *dv) || (rv != nil && *rv) {
				res.Mismatches = append(res.Mismatches, Mismatch{ID: id, Detected: dv, Runtime: rv, Verified: vv,
					Effective: effective, UnsupportedReason: reason})
			}
			continue
		}
		effective, source := declared, "declared"
		switch {
		case vv != nil:
			effective, source = *vv, "runtime"
		case rv != nil:
			effective, source = *rv, "runtime"
		case dv != nil:
			effective, source = *dv, "detected"
		}
		if effective {
			res.Capabilities = append(res.Capabilities, catalog.Capability{ID: id, Source: source})
		}
		opinions := []bool{declared}
		for _, p := range []*bool{dv, rv, vv} {
			if p != nil {
				opinions = append(opinions, *p)
			}
		}
		for _, o := range opinions[1:] {
			if o != opinions[0] {
				res.Mismatches = append(res.Mismatches, Mismatch{ID: id, Declared: declared, Detected: dv, Runtime: rv,
					Verified: vv, Effective: effective})
				break
			}
		}
	}
	order := map[string]int{}
	for i, c := range s.Capabilities {
		order[c.ID] = i
	}
	pos := func(id string) int {
		if i, ok := order[id]; ok {
			return i
		}
		return 1 << 30
	}
	sortStable(res.Capabilities, func(a, b catalog.Capability) bool { return pos(a.ID) < pos(b.ID) })
	switch {
	case rt != nil && rt.InputModalities != nil:
		res.InputModalities, res.InputSource = rt.InputModalities, "runtime"
	case det != nil && det.InputModalities != nil:
		res.InputModalities, res.InputSource = det.InputModalities, "detected"
	default:
		res.InputModalities, res.InputSource = s.InputModalities, "declared"
	}
	if det != nil {
		res.Evidence = det.Evidence
	}
	if rt != nil {
		res.RuntimeBuild = rt.BuildInfo
	}
	return res
}

// CapabilitiesFor resolves one model from its files and the stored runtime
// report. It never downloads, hashes or launches anything.
func CapabilitiesFor(ctx context.Context, st *store.Store, cat *catalog.Catalog, s *catalog.Spec, dir string) Resolved {
	var rt *store.RuntimeCapabilities
	if sha := s.Primary().SHA256; sha != "" && st != nil {
		rt, _ = st.RuntimeCapabilitiesFor(ctx, s.ModelID, sha)
	}
	marks := s.UnsupportedCapabilities
	if cat != nil {
		marks = cat.EffectiveUnsupported(s)
	}
	return ResolveCapabilitiesWith(s, DetectCapabilities(s, dir), rt, marks)
}

// --- runtime probe ------------------------------------------------------------

// ProbeLlamaServer asks a healthy llama-server what it supports (/props, and
// /v1/models for decision models) and stores the answer.
func ProbeLlamaServer(ctx context.Context, st *store.Store, baseURL string, s *catalog.Spec) (*store.RuntimeCapabilities, error) {
	sha := s.Primary().SHA256
	if sha == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var props struct {
		BuildInfo    string `json:"build_info"`
		ChatTemplate string `json:"chat_template"`
		Modalities   *struct {
			Vision bool `json:"vision"`
			Audio  bool `json:"audio"`
		} `json:"modalities"`
		Caps *struct {
			SupportsToolCalls *bool `json:"supports_tool_calls"`
		} `json:"chat_template_caps"`
	}
	if err := getJSON(ctx, baseURL+"/props", &props); err != nil {
		return nil, err
	}
	rec := store.RuntimeCapabilities{ModelID: s.ModelID, ArtifactSHA256: sha, Capabilities: map[string]bool{},
		InputModalities: []string{"text"}, BuildInfo: props.BuildInfo}
	if rec.BuildInfo == "" {
		rec.BuildInfo = "unknown"
	}
	if !s.IsEmbedding() && !s.IsDecision() {
		if props.Caps != nil && props.Caps.SupportsToolCalls != nil {
			rec.Capabilities["tool-calling"] = *props.Caps.SupportsToolCalls
		}
		if props.ChatTemplate != "" && AnalyzeChatTemplate(props.ChatTemplate)["reasoning"] {
			rec.Capabilities["reasoning"] = true
		}
	}
	if props.Modalities != nil {
		if props.Modalities.Vision {
			rec.InputModalities = append(rec.InputModalities, "image")
		}
		if props.Modalities.Audio {
			rec.InputModalities = append(rec.InputModalities, "audio")
		}
	}
	if s.IsDecision() {
		var models struct {
			Data []struct {
				Architecture *struct {
					Output []string `json:"output_modalities"`
				} `json:"architecture"`
			} `json:"data"`
		}
		if getJSON(ctx, baseURL+"/v1/models", &models) == nil {
			var outs []string
			for _, m := range models.Data {
				if m.Architecture != nil {
					outs = append(outs, m.Architecture.Output...)
				}
			}
			if len(outs) > 0 {
				rec.Capabilities["decision"] = contains(outs, "decisions")
			}
		}
	}
	return &rec, st.RecordRuntimeCapabilities(ctx, rec)
}

func getJSON(ctx context.Context, url string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &httpStatusError{resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "status " + itoa(e.code) }
