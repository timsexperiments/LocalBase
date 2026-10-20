package runtime

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// Speech backends (TS speech-supervisor.ts and the 2026-10-20 CrispASR
// screen). Both keep the model resident and answer one request at a time.

const (
	// SpeechContextTokens and SpeechMaxFrames bound the Qwen TTS talker (TS
	// SPEECH_CONTEXT_TOKENS, SPEECH_MAX_FRAMES).
	SpeechContextTokens = 2048
	SpeechMaxFrames     = 256
	SpeechSampleRate    = 24000
)

// SpeakerFiles are a tts-server model's voices: the catalog reference clips
// plus stored custom voices whose recording matches its checksum.
func (s *Service) SpeakerFiles(ctx context.Context, t *Target) map[string]string {
	out := map[string]string{}
	if spec := s.d.Catalog.ByID(t.ModelID); spec != nil && spec.TTSRuntime != nil {
		for _, v := range spec.TTSRuntime.ReferenceVoices {
			if p := filepath.Join(t.Dir, v.ArtifactFilename); fileExists(p) {
				out[v.Name] = p
			}
		}
	}
	if s.d.Store != nil && s.d.RootDir != "" {
		// Only recordings that still match their checksum (TS referenceFiles).
		if files, err := voices.New(s.d.Store, s.d.RootDir).ReferenceFiles(ctx); err == nil {
			maps.Copy(out, files)
		}
	}
	return out
}

func (s *Service) ttsServerCommand(t *Target, host string, port int, weights string) (*serviceCommand, error) {
	projector := t.artifact(models.RoleProjector)
	if projector == "" {
		return nil, fmt.Errorf("%s needs the model's projector file", t.Backend.Name)
	}
	bin, err := s.binary("tts-server", t)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on this device: %v", t.Backend.Name, err)
	}
	args := []string{"--host", host, "--port", strconv.Itoa(port), "--offline", "-m", weights, "-mm", projector,
		"-c", strconv.Itoa(SpeechContextTokens), "-n", strconv.Itoa(SpeechMaxFrames), "-t", "4", "-tb", "4", "--tts-lang", "en"}
	speakers := s.SpeakerFiles(context.Background(), t)
	names := make([]string, 0, len(speakers))
	for n := range speakers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args = append(args, "--voice", n+"="+speakers[n])
	}
	return &serviceCommand{name: "tts-server", bin: bin, args: args, health: "/health"}, nil
}

// misakiDict is the support file the managed CrispASR package carries.
const misakiDict = "misaki-merged.json"

// crispasrCommand starts CrispASR's HTTP server for a Kokoro variant, with
// the configuration the TTS screen qualified:
//   - the pinned Misaki dictionary with builtin-only G2P;
//   - no punctuation model and no network access (blocked proxies), so it
//     never fetches anything on its own;
//   - voices resolved from the model directory.
//
// CrispASR's AI-content watermark and C2PA provenance stay on: LocalBase
// passes none of its marking opt-outs.
func (s *Service) crispasrCommand(t *Target, host string, port int, weights string) (*serviceCommand, error) {
	arch := t.Variant.Architecture
	if !arch.Known() {
		return nil, fmt.Errorf("%s needs the model's architecture (kokoro or qwen3-tts), which is unknown", t.Backend.Name)
	}
	if arch.Value != "kokoro" {
		return nil, fmt.Errorf("%s runs %s only with a reference voice and consent attestation, which LocalBase does not pass yet", t.Backend.Name, arch.Value)
	}
	bin, err := s.binary("crispasr", t)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on this device: %v", t.Backend.Name, err)
	}
	voice := t.defaultVoice()
	if voice == "" {
		return nil, fmt.Errorf("%s needs a Kokoro voice file in the variant", t.Backend.Name)
	}
	cache := filepath.Join(s.d.CacheDir, "crispasr")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, err
	}
	args := []string{"--server", "--host", host, "--port", strconv.Itoa(port), "--backend", "kokoro", "-m", weights,
		"--voice", voice, "--voice-dir", t.Dir, "-l", "en", "-t", "4", "--cache-dir", cache, "--no-punctuation"}
	if acc := gpuAccelerator(t); acc == "" {
		args = append(args, "--no-gpu")
	} else {
		args = append(args, "--gpu-backend", acc)
	}
	env := append(os.Environ(),
		"CRISPASR_KOKORO_G2P=builtin-only",
		"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost", "XDG_CACHE_HOME="+cache)
	// Without the dictionary CrispASR's builtin G2P mispronounces common
	// words, so a missing one is an install fault, not a fallback.
	dict := SupportPath(s.d.RuntimesDir, "crispasr", misakiDict)
	if dict == "" || !fileExists(dict) {
		dict = filepath.Join(filepath.Dir(bin), misakiDict) // an operator-provided build
	}
	if !fileExists(dict) {
		return nil, fmt.Errorf("%s needs the pinned Misaki dictionary (%s), which is not installed", t.Backend.Name, misakiDict)
	}
	env = append(env, "CRISPASR_MISAKI_DICT_PATH="+dict)
	return &serviceCommand{name: "crispasr", bin: bin, dir: t.Dir, args: args, env: env, health: "/health"}, nil
}

// gpuAccelerator is the definition's accelerator on this OS ("" for a CPU build).
func gpuAccelerator(t *Target) string {
	for _, p := range t.Backend.Platforms {
		if (p.OS == goruntime.GOOS || p.OS == "*") && p.Accelerator != "cpu" {
			return p.Accelerator
		}
	}
	return ""
}

// defaultVoice is the variant's first voice file (absolute path).
func (t *Target) defaultVoice() string {
	for _, a := range t.Variant.Artifacts {
		if a.Role == models.RoleVoice {
			return filepath.Join(t.Dir, a.Filename)
		}
	}
	return ""
}

// VoiceFile maps a request voice to a file name the CrispASR server resolves
// in the model directory: a variant voice artifact named after the voice
// ("" when the variant has none).
func (t *Target) VoiceFile(voice string) string {
	for _, a := range t.Variant.Artifacts {
		if a.Role != models.RoleVoice {
			continue
		}
		base := strings.TrimSuffix(a.Filename, filepath.Ext(a.Filename))
		if base == voice || strings.HasSuffix(base, "-"+voice) || strings.HasSuffix(base, "_"+voice) {
			return a.Filename
		}
	}
	return ""
}

// Voices lists the variant's voice names.
func (t *Target) Voices() []string {
	var out []string
	for _, a := range t.Variant.Artifacts {
		if a.Role == models.RoleVoice {
			base := strings.TrimSuffix(a.Filename, filepath.Ext(a.Filename))
			if i := strings.LastIndex(base, "voice-"); i >= 0 {
				base = base[i+len("voice-"):]
			}
			out = append(out, base)
		}
	}
	return out
}
