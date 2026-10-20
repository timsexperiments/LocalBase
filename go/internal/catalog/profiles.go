package catalog

// Runtime profiles, decoded strictly. Each struct is the union of the TS
// discriminated variants; Kind/Mode says which fields apply.

type TTSRuntime struct {
	Kind                      string           `json:"kind"` // llama-tts | kokoro-onnx
	ProjectorArtifactFilename string           `json:"projectorArtifactFilename,omitempty"`
	ReferenceVoices           []ReferenceVoice `json:"referenceVoices,omitempty"`
	Artifacts                 *KokoroFiles     `json:"artifacts,omitempty"`
	Voices                    []struct {
		Name string `json:"name"`
	} `json:"voices,omitempty"`
	DefaultVoice string `json:"defaultVoice,omitempty"`
}

type ReferenceVoice struct {
	Name             string `json:"name"`
	ArtifactFilename string `json:"artifactFilename"`
	License          string `json:"license"`
	ProvenanceURL    string `json:"provenanceUrl"`
}

type KokoroFiles struct {
	Model           string `json:"model"`
	Config          string `json:"config"`
	Tokenizer       string `json:"tokenizer"`
	TokenizerConfig string `json:"tokenizerConfig"`
}

type ImageRuntime struct {
	Kind      string `json:"kind"` // diffusion-qwen3 | diffusion-flux1 | checkpoint
	Artifacts struct {
		DiffusionModel string `json:"diffusionModel"`
		VAE            string `json:"vae,omitempty"`
		TextEncoder    string `json:"textEncoder,omitempty"`
		ClipL          string `json:"clipL,omitempty"`
		T5XXL          string `json:"t5xxl,omitempty"`
	} `json:"artifacts"`
	Generation Generation `json:"generation"`
}

type Generation struct {
	Sampler   string  `json:"sampler"`
	Scheduler string  `json:"scheduler,omitempty"`
	Steps     int     `json:"steps"`
	CfgScale  float64 `json:"cfgScale"`
	FlowShift float64 `json:"flowShift,omitempty"`
	Seed      *int64  `json:"seed,omitempty"`
}

type ImageLaunchOptions struct {
	DiffusionFlashAttention bool       `json:"diffusionFlashAttention"`
	Prediction              string     `json:"prediction,omitempty"`
	Lora                    *ImageLora `json:"lora,omitempty"`
}

type ImageLora struct {
	ArtifactFilename string  `json:"artifactFilename"`
	DefaultStrength  float64 `json:"defaultStrength"`
	Trigger          string  `json:"trigger,omitempty"`
}

type VideoRuntime struct {
	Mode          string `json:"mode"` // t2v | ti2v | s2v
	Qualification struct {
		MaxWidth      int        `json:"maxWidth"`
		MaxHeight     int        `json:"maxHeight"`
		MaxFrames     int        `json:"maxFrames"`
		FPS           int        `json:"fps"`
		JobDeadlineMs int        `json:"jobDeadlineMs"`
		Generation    Generation `json:"generation"`
		LaunchOptions struct {
			CPUOffload              bool `json:"cpuOffload"`
			DiffusionFlashAttention bool `json:"diffusionFlashAttention"`
			VAEConvDirect           bool `json:"vaeConvDirect"`
		} `json:"launchOptions"`
	} `json:"qualification"`
	SupportedTargets []struct {
		Platform     string `json:"platform"`
		Architecture string `json:"architecture"`
		Accelerator  string `json:"accelerator"`
	} `json:"supportedTargets"`
	Artifacts struct {
		DiffusionModel string `json:"diffusionModel"`
		TextEncoder    string `json:"textEncoder"`
		Decoder        struct {
			Kind             string `json:"kind"` // vae | tae
			ArtifactFilename string `json:"artifactFilename"`
		} `json:"decoder"`
		AudioEncoder string `json:"audioEncoder,omitempty"`
	} `json:"artifacts"`
}

// ArtifactSize is the expected size of the named artifact (0 if unknown).
func (s *Spec) ArtifactSize(filename string) int64 {
	for _, a := range s.Artifacts {
		if a.Filename == filename {
			return a.ExpectedSizeBytes
		}
	}
	return 0
}
