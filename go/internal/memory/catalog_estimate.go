package memory

import (
	_ "embed"
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// Measurement is a checked-in calibration record (memory-measurements.ts),
// embedded from measurements.json (tools/golden/measurements.ts).
type Measurement struct {
	Kind    string `json:"kind"`
	ModelID string `json:"modelId"`
	Shape   struct {
		CtxTokens               int    `json:"ctxTokens"`
		Slots                   int    `json:"slots"`
		CacheTypeK              string `json:"cacheTypeK"`
		CacheTypeV              string `json:"cacheTypeV"`
		Width                   int    `json:"width"`
		Height                  int    `json:"height"`
		Steps                   int    `json:"steps"`
		Frames                  int    `json:"frames"`
		CPUOffload              bool   `json:"cpuOffload"`
		DiffusionFlashAttention bool   `json:"diffusionFlashAttention"`
	} `json:"shape"`
	BackendAcceleratorBytes int64 `json:"backendAcceleratorBytes"`
	HostBufferBytes         int64 `json:"hostBufferBytes"`
	PeakRSSBytes            int64 `json:"peakRssBytes"`
	SystemDeltaBytes        int64 `json:"systemDeltaBytes"`
}

//go:embed measurements.json
var measurementsJSON []byte

// Measurements are every checked-in record, in source order.
var Measurements = func() []Measurement {
	var m []Measurement
	if err := json.Unmarshal(measurementsJSON, &m); err != nil {
		panic(err)
	}
	return m
}()

// byModel is the first record of this kind for the model, as TS finds it.
func byModel(kind, id string) *Measurement {
	for i := range Measurements {
		if m := &Measurements[i]; m.Kind == kind && m.ModelID == id {
			return m
		}
	}
	return nil
}

func roundUp(b, g int64) int64 { return (b + g - 1) / g * g }

const (
	whisperTextCtx           = 448
	whisperAudioCtx          = 1500
	whisperDecoders          = 5
	whisperLiveEncoderActs   = 10
	whisperAcceleratorOH     = 128 * MiB
	ttsContextTokens         = 2048 // SPEECH_CONTEXT_TOKENS
	ttsCodecBytes            = 1 * GiB
	ttsProjectorCopies       = 2
	kokoroSessionBytes       = 512 * MiB
	diffusionActPerTokenDim  = 48
	vaeLiveFeatureMaps       = 13
	sdxlVAELiveFeatureMaps   = 15
	imageVAEChannels         = 128
	textEncoderComputeBytes  = 256 * MiB
	videoVAEChunkFrames      = 4
	wanVAEChannels           = 96
	taeChannels              = 64
	taeLiveFeatureMaps       = 15
	wanVAELiveFeatureMaps    = 23
	videoAcceleratorRounding = 256 * MiB
)

func estimated(acc, host, unified int64, confidence string) Demand {
	return Demand{AcceleratorBytes: acc, HostBytes: host, UnifiedBytes: unified, Confidence: confidence}
}

type whisperShape struct{ decoderLayers, heads, state int64 }

var turboRe = regexp.MustCompile(`(?i)turbo`)

func whisperShapeFor(s *catalog.Spec) whisperShape {
	switch strings.TrimSuffix(strings.ToLower(s.Size), ".en") {
	case "tiny":
		return whisperShape{4, 6, 384}
	case "base":
		return whisperShape{6, 8, 512}
	case "small":
		return whisperShape{12, 12, 768}
	case "medium":
		return whisperShape{24, 16, 1024}
	}
	layers := int64(32)
	for _, a := range s.Artifacts {
		if turboRe.MatchString(a.Filename) {
			layers = 4
		}
	}
	return whisperShape{layers, 20, 1280}
}

// EstimateSTT: whisper-server KV (decoder self + cross) and encoder compute.
func EstimateSTT(s *catalog.Spec) Demand {
	w := s.TotalArtifactBytes()
	sh := whisperShapeFor(s)
	selfKV := whisperDecoders * sh.decoderLayers * whisperTextCtx * sh.state * 2 * 2
	crossKV := sh.decoderLayers * whisperAudioCtx * sh.state * 2 * 2
	compute := whisperAudioCtx*whisperAudioCtx*sh.heads*4 + whisperAudioCtx*sh.state*4*whisperLiveEncoderActs
	kv := selfKV + crossKV
	return estimated(w+kv+compute+whisperAcceleratorOH, w+RuntimeHostOverhead, w+kv+compute+RuntimeHostOverhead, "derived")
}

// EstimateTTS: the Qwen3-TTS talker (f16 KV at the speech context), the
// projector loaded twice and the codec, all in one process. Kokoro is CPU only.
func EstimateTTS(s *catalog.Spec, talker *Geometry) Demand {
	if s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx" {
		total := s.TotalArtifactBytes() + kokoroSessionBytes + RuntimeHostOverhead
		return estimated(0, total, total, "estimated")
	}
	var projector int64
	if s.TTSRuntime != nil {
		projector = s.ArtifactSize(s.TTSRuntime.ProjectorArtifactFilename)
	}
	weights := s.TotalArtifactBytes() - projector
	shape := ResolveShape(s.Size, s.Quant, weights, talker)
	kv, compute, _ := llmTerms(shape, LLMLaunch{CtxTokens: ttsContextTokens, Slots: 1, CacheTypeK: "f16", CacheTypeV: "f16"})
	total := weights + projector*ttsProjectorCopies + kv + compute + ttsCodecBytes + RuntimeHostOverhead
	return estimated(total, total, total, "estimated")
}

func attentionActivation(tokens, heads, width int64, flash bool) int64 {
	var scores int64
	if !flash {
		scores = tokens * tokens * heads * 4
	}
	return scores + tokens*width*diffusionActPerTokenDim
}

type imageFamily struct{ pixels, tokens, heads, width, vaeMaps int64 }

var (
	zImageRe = regexp.MustCompile(`(?i)z.?image`)
	nineBRe  = regexp.MustCompile(`(?i)9b`)
)

func imageFamilyFor(s *catalog.Spec) imageFamily {
	mega := int64(1024 * 1024)
	switch {
	case s.ImageRuntime != nil && s.ImageRuntime.Kind == "diffusion-flux1":
		return imageFamily{mega, 4096 + 256, 24, 3072, vaeLiveFeatureMaps}
	case s.ImageRuntime != nil && s.ImageRuntime.Kind == "diffusion-qwen3":
		if zImageRe.MatchString(s.Family) {
			return imageFamily{mega, 4096 + 512, 30, 3840, vaeLiveFeatureMaps}
		}
		if nineBRe.MatchString(s.Size) {
			return imageFamily{mega, 4096 + 512, 32, 4096, vaeLiveFeatureMaps}
		}
		return imageFamily{mega, 4096 + 512, 24, 3072, vaeLiveFeatureMaps}
	case strings.Contains(strings.ToLower(s.Family), "xl"):
		return imageFamily{mega, 4096, 10, 640, sdxlVAELiveFeatureMaps}
	}
	return imageFamily{512 * 512, 4096, 8, 320, vaeLiveFeatureMaps}
}

// EstimateImage: sd-server weights plus the largest stage (attention,
// untiled VAE decode, text encoder); flux1 keeps its encoders on the host.
func EstimateImage(s *catalog.Spec) Demand {
	f := imageFamilyFor(s)
	weights := s.TotalArtifactBytes()
	var hostOnly int64
	if rt := s.ImageRuntime; rt != nil && rt.Kind == "diffusion-flux1" {
		hostOnly = s.ArtifactSize(rt.Artifacts.ClipL) + s.ArtifactSize(rt.Artifacts.T5XXL)
	}
	flash := s.ImageLaunchOptions != nil && s.ImageLaunchOptions.DiffusionFlashAttention
	activation := attentionActivation(f.tokens, f.heads, f.width, flash)
	vae := f.pixels * imageVAEChannels * 4 * f.vaeMaps
	compute := max(activation, vae, textEncoderComputeBytes)
	return estimated(weights-hostOnly+compute+AcceleratorOverhead, weights+RuntimeHostOverhead,
		weights+compute+RuntimeHostOverhead, "derived")
}

type videoFamily struct{ spatial, temporal, heads, width int64 }

var (
	fiveBRe     = regexp.MustCompile(`5b`)
	fourteenBRe = regexp.MustCompile(`14b`)
)

func videoFamilyFor(s *catalog.Spec) videoFamily {
	id := strings.ToLower(s.ModelID)
	switch {
	case strings.HasPrefix(id, "ltx"):
		return videoFamily{32, 8, 32, 4096}
	case strings.HasPrefix(id, "hunyuan"):
		return videoFamily{16, 4, 16, 2048}
	case fiveBRe.MatchString(id):
		return videoFamily{32, 4, 24, 3072}
	case fourteenBRe.MatchString(id):
		return videoFamily{16, 4, 40, 5120}
	}
	return videoFamily{16, 4, 12, 1536}
}

func ceilDiv(a, b int64) int64 { return int64(math.Ceil(float64(a) / float64(b))) }

// VideoLatentTokens is the patchified latent token count of a clip.
func videoLatentTokens(f videoFamily, width, height, frames int64) int64 {
	return ceilDiv(width, f.spatial) * ceilDiv(height, f.spatial) * (1 + ceilDiv(frames-1, f.temporal))
}

// EstimateVideo: sd-server video demand at the qualification bounds; with
// --offload-to-cpu only the largest stage is resident on the accelerator.
func EstimateVideo(s *catalog.Spec) Demand {
	f := videoFamilyFor(s)
	rt := s.VideoRuntime
	width, height, frames := int64(832), int64(480), int64(81)
	flash, offload := true, false
	if rt != nil {
		q := rt.Qualification
		width, height, frames = int64(q.MaxWidth), int64(q.MaxHeight), int64(q.MaxFrames)
		flash, offload = q.LaunchOptions.DiffusionFlashAttention, q.LaunchOptions.CPUOffload
	}
	activation := attentionActivation(videoLatentTokens(f, width, height, frames), f.heads, f.width, flash)
	tae := rt != nil && rt.Artifacts.Decoder.Kind == "tae"
	channels, maps := int64(wanVAEChannels), int64(wanVAELiveFeatureMaps)
	if tae {
		channels, maps = taeChannels, taeLiveFeatureMaps
	}
	vae := width * height * videoVAEChunkFrames * channels * 4 * maps
	var diffusion, text, decoder, audio int64
	if rt != nil {
		diffusion = s.ArtifactSize(rt.Artifacts.DiffusionModel)
		text = s.ArtifactSize(rt.Artifacts.TextEncoder)
		decoder = s.ArtifactSize(rt.Artifacts.Decoder.ArtifactFilename)
		if rt.Mode == "s2v" {
			audio = s.ArtifactSize(rt.Artifacts.AudioEncoder)
		}
	} else {
		diffusion = s.TotalArtifactBytes()
		id := strings.ToLower(s.ModelID)
		switch {
		case strings.HasPrefix(id, "ltx"):
			text = int64(math.Round(6.8 * float64(GiB)))
		case strings.HasPrefix(id, "hunyuan"):
			text = int64(math.Round(4.4 * float64(GiB)))
		default:
			text = 6_044_158_944
		}
	}
	weights := diffusion + text + decoder + audio
	compute := max(activation, vae, textEncoderComputeBytes)
	resident := weights + compute
	if offload {
		resident = max(text+textEncoderComputeBytes, diffusion+activation, decoder+vae, audio)
	}
	conf := "estimated"
	if rt != nil {
		conf = "derived"
	}
	return estimated(roundUp(resident+AcceleratorOverhead, videoAcceleratorRounding), weights+RuntimeHostOverhead,
		weights+compute+RuntimeHostOverhead, conf)
}

// LLMReference is the catalog fit launch: 8K (or the model window), one
// slot, the launcher's KV type, no prompt cache.
func LLMReference(s *catalog.Spec, g *Geometry) (weights int64, shape LLMShape, l LLMLaunch) {
	weights = s.TotalArtifactBytes()
	shape = ResolveShape(s.Size, s.Quant, weights, g)
	kv := "q8_0"
	if s.IsEmbedding() || !shape.Geometry.Q8Compatible {
		kv = "f16"
	}
	ctx := LLMReferenceContext
	if s.ContextWindowTokens != nil {
		ctx = min(ctx, *s.ContextWindowTokens)
	}
	return weights, shape, LLMLaunch{CtxTokens: ctx, Slots: 1, CacheTypeK: kv, CacheTypeV: kv, Decision: s.IsDecision()}
}

// EstimateCatalog is a catalog model's estimate at its reference shape.
// geometry is the installed GGUF's (LLM, or the TTS talker), or nil.
func EstimateCatalog(s *catalog.Spec, geometry *Geometry) Demand {
	switch s.Kind {
	case "llm":
		w, shape, l := LLMReference(s, geometry)
		return EstimateLLM(w, shape, l)
	case "stt":
		return EstimateSTT(s)
	case "tts":
		return EstimateTTS(s, geometry)
	case "image":
		return EstimateImage(s)
	}
	return EstimateVideo(s)
}

// MeasuredCatalog is the measured demand at the reference shape, when the
// model is qualified and was measured there.
func MeasuredCatalog(s *catalog.Spec, geometry *Geometry) (Demand, bool) {
	if !s.Qualified() {
		return Demand{}, false
	}
	weights := s.TotalArtifactBytes()
	var r *Measurement
	overhead, rounding := AcceleratorOverhead, int64(0)
	switch s.Kind {
	case "llm":
		w, _, l := LLMReference(s, geometry)
		return MeasuredLLMDemand(s.ModelID, l, w)
	case "stt":
		r, overhead = byModel("stt", s.ModelID), whisperAcceleratorOH
	case "tts":
		r = byModel("tts", s.ModelID)
		if s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx" {
			overhead = 0
		}
	case "image":
		flash := s.ImageLaunchOptions != nil && s.ImageLaunchOptions.DiffusionFlashAttention
		if m := byModel("image", s.ModelID); m != nil && m.Shape.DiffusionFlashAttention == flash {
			r = m
		}
	case "video":
		rounding = videoAcceleratorRounding
		if rt := s.VideoRuntime; rt != nil {
			q := rt.Qualification
			if m := byModel("video", s.ModelID); m != nil && m.Shape.Width == q.MaxWidth && m.Shape.Height == q.MaxHeight &&
				m.Shape.Frames == q.MaxFrames && m.Shape.CPUOffload == q.LaunchOptions.CPUOffload &&
				m.Shape.DiffusionFlashAttention == q.LaunchOptions.DiffusionFlashAttention {
				r = m
			}
		}
	}
	if r == nil {
		return Demand{}, false
	}
	return measuredDemand(*r, weights, 0, overhead, rounding), true
}
