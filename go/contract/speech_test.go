package contract

import (
	"bytes"
	"encoding/binary"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// TestDifferentialSpeechRejections sends invalid speech requests to both
// gateways and requires the same status, type, code, param and message.
// None of them reaches a backend.
func TestDifferentialSpeechRejections(t *testing.T) {
	long := strings.Repeat("a", 257)
	cases := []struct{ name, body string }{
		{"not json", `{`},
		{"array", `[]`},
		{"string", `"hi"`},
		{"null", `null`},
		{"missing model", `{"input":"hi","voice":"default"}`},
		{"blank model", `{"model":" ","input":"hi","voice":"default"}`},
		{"empty input", `{"model":"kokoro-82m-v1.0-q8","input":"","voice":"default"}`},
		{"long input", `{"model":"kokoro-82m-v1.0-q8","input":"` + long + `","voice":"default"}`},
		{"missing voice", `{"model":"kokoro-82m-v1.0-q8","input":"hi"}`},
		{"bad voice", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"alloy"}`},
		{"bad format", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"default","response_format":"mp3"}`},
		{"bad stream", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"default","stream_format":"ws"}`},
		{"speed", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"default","speed":2}`},
		{"instructions", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"default","instructions":"calm"}`},
		{"unknown key", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"default","pitch":1}`},
		{"unknown model", `{"model":"no-such-model","input":"hi","voice":"default"}`},
		{"chat model", `{"model":"` + chatModel + `","input":"hi","voice":"default"}`},
		{"missing custom voice", `{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"hi","voice":"voice-0123456789ab"}`},
		{"voice not on model", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"harbor"}`},
	}
	for _, c := range cases {
		var first *Resp
		var firstName string
		for _, tg := range targets {
			h := bearer(tg.Key)
			h["Content-Type"] = "application/json"
			r := do(t, "POST", tg.BaseURL+"/v1/audio/speech", h, c.body)
			if r.Status < 400 {
				t.Errorf("%s %s: %d %s", tg.Name, c.name, r.Status, r.Body)
				continue
			}
			r.ErrorCode(t)
			if first == nil {
				first, firstName = r, tg.Name
				continue
			}
			ea, eb := first.JSON(t)["error"].(map[string]any), r.JSON(t)["error"].(map[string]any)
			for _, k := range []string{"type", "code", "param", "message"} {
				if ea[k] != eb[k] || first.Status != r.Status {
					t.Errorf("%s:\n %s %d %s\n %s %d %s", c.name, firstName, first.Status, first.Body, tg.Name, r.Status, r.Body)
					break
				}
			}
		}
	}
}

// linkCrispASR prepares Kokoro through CrispASR in a scratch root from
// LOCALBASE_CONTRACT_CRISPASR: rt/crispasr-macos-arm64/ (the pinned release,
// unpacked), misaki-merged.json and models/kokoro-*.gguf. The runtime is
// copied into the managed layout; the weights are cloned (APFS) read-only.
func linkCrispASR(r *root.Root) error {
	src := os.Getenv("LOCALBASE_CONTRACT_CRISPASR")
	if src == "" || goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
		return nil
	}
	pkg := filepath.Join(r.RuntimesDir(), "crispasr", crispasrPin)
	if err := mustMkdirAll(pkg, 0o700); err != nil {
		return err
	}
	if err := guardedCopyDir(filepath.Join(src, "rt", "crispasr-macos-arm64"), pkg); err != nil {
		return err
	}
	if err := cloneModel(filepath.Join(src, "misaki-merged.json"), filepath.Join(pkg, "misaki-merged.json")); err != nil {
		return err
	}
	if err := mustMkdirAll(r.ModelsDir("tts"), 0o700); err != nil {
		return err
	}
	for _, f := range []string{"kokoro-82m-f16.gguf", "kokoro-voice-af_heart.gguf"} {
		if err := cloneModel(filepath.Join(src, "models", f), filepath.Join(r.ModelsDir("tts"), f)); err != nil {
			return err
		}
	}
	return nil
}

// crispasrPin is the managed CrispASR package directory (its archive sha256).
const crispasrPin = "5c13fbbc12eb45c3f01823a6bbfbbb22e0682bc76c6b78a0d9511da1aaf88831"

// TestSpeechKokoroCrispASR synthesizes speech with Kokoro through CrispASR
// on the Go gateway, checks every output shape, and transcribes the result
// with whisper tiny.en to confirm it says the input.
func TestSpeechKokoroCrispASR(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_CRISPASR") == "" || os.Getenv("LOCALBASE_CONTRACT_STT_MODELS") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_CRISPASR and LOCALBASE_CONTRACT_STT_MODELS")
	}
	var tg *Target
	for _, x := range targets {
		if x.Name == "go" {
			tg = x
		}
	}
	if tg == nil {
		t.Skip("needs the go target")
	}
	sel := do(t, "GET", tg.BaseURL+"/_localbase/model-backend?model=kokoro-82m-v1.0-q8", bearer(tg.Admin), "")
	if s, _ := sel.JSON(t)["selection"].(map[string]any); s == nil || s["backendId"] != "crispasr-metal" || s["variantId"] != "crispasr-f16" {
		t.Fatalf("automatic selection: %d %s", sel.Status, sel.Body)
	}
	h := bearer(tg.Key)
	h["Content-Type"] = "application/json"
	speak := func(extra string) *Resp {
		return do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
			`{"model":"kokoro-82m-v1.0-q8","input":"The weather is lovely today.","voice":"af_heart"`+extra+`}`)
	}
	r := speak("")
	if r.Status != 200 || r.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("wav: %d %s %.200s", r.Status, r.Header.Get("Content-Type"), r.Body)
	}
	wav := r.Body
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		binary.LittleEndian.Uint16(wav[22:]) != 1 || binary.LittleEndian.Uint32(wav[24:]) != 24000 ||
		binary.LittleEndian.Uint16(wav[34:]) != 16 || int(binary.LittleEndian.Uint32(wav[40:])) != len(wav)-44 {
		t.Fatalf("not 24 kHz mono 16-bit WAV: % x", wav[:min(len(wav), 44)])
	}
	if secs := float64(len(wav)-44) / 48000; secs < 0.8 || secs > 10 {
		t.Fatalf("speech is %.2fs", secs)
	}
	if r := speak(`,"response_format":"pcm"`); r.Status != 200 || len(r.Body)%2 != 0 || len(r.Body) < 24000 {
		t.Errorf("pcm: %d %d bytes", r.Status, len(r.Body))
	}
	if r := speak(`,"stream_format":"sse"`); r.Status != 200 || !strings.Contains(string(r.Body), "speech.audio.done") {
		t.Errorf("sse: %d %.300s", r.Status, r.Body)
	}
	// A catalog voice the pinned variant does not carry.
	if r := do(t, "POST", tg.BaseURL+"/v1/audio/speech", h, `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"af_bella"}`); r.Status != 400 || r.ErrorCode(t) != "voice_not_supported" {
		t.Errorf("af_bella: %d %s", r.Status, r.Body)
	}

	// Round trip: whisper hears the input.
	in := filepath.Join(scratchDir(t), "speech.wav")
	out := filepath.Join(scratchDir(t), "speech-16k.wav")
	mustWriteFile(in, wav, 0o600)
	if b, err := exec.Command("afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", in, out).CombinedOutput(); err != nil {
		t.Fatalf("afconvert: %v %s", err, b)
	}
	clip, _ := os.ReadFile(out)
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, _ := w.CreateFormFile("file", "speech.wav")
	fw.Write(clip)
	w.WriteField("model", "whisper-tiny-en-q8_0")
	w.WriteField("response_format", "text")
	w.Close()
	sh := bearer(tg.Key)
	sh["Content-Type"] = w.FormDataContentType()
	tr := do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", sh, b.String())
	text := strings.ToLower(string(tr.Body))
	if tr.Status != 200 || !strings.Contains(text, "weather") || !strings.Contains(text, "lovely") {
		t.Fatalf("transcript: %d %q", tr.Status, tr.Body)
	}
	t.Logf("heard: %s", tr.Body)

	// Marking: on by default (the watermark is detectable); `speech marking
	// off` restarts the server without it on the next request.
	detector := filepath.Join(os.Getenv("LOCALBASE_CONTRACT_CRISPASR"), "rt", "crispasr-macos-arm64", "crispasr")
	marked := func(wav []byte) bool {
		p := filepath.Join(scratchDir(t), "check.wav")
		mustWriteFile(p, wav, 0o600)
		out, _ := exec.Command(detector, "--detect-watermark", p).CombinedOutput()
		return strings.Contains(string(out), "WATERMARK DETECTED")
	}
	if !marked(wav) {
		t.Error("default output carries no AI-content watermark")
	}
	if r := runCLI(t, "go", tg.Root, "--json", "speech", "marking", "off"); r.code != 0 {
		t.Fatalf("speech marking off: %s %s", r.stdout, r.stderr)
	}
	defer runCLI(t, "go", tg.Root, "speech", "marking", "on")
	if r := speak(""); r.Status != 200 || marked(r.Body) {
		t.Errorf("marking off: %d, still marked %v", r.Status, r.Status == 200)
	}
}

// TestSpeechQwenReal runs Qwen3-TTS through tts-server on both gateways: a
// reference voice, then a custom voice cloned from an uploaded recording.
func TestSpeechQwenReal(t *testing.T) {
	d := os.Getenv("LOCALBASE_CONTRACT_TTS_MODELS")
	if d == "" || os.Getenv("LOCALBASE_CONTRACT_RUNTIMES") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_TTS_MODELS and LOCALBASE_CONTRACT_RUNTIMES")
	}
	clip, err := os.ReadFile(filepath.Join(d, "qwen3-tts-harbor.wav"))
	if err != nil {
		t.Skip("no reference clip")
	}
	checkWAV := func(label string, r *Resp) []byte {
		t.Helper()
		b := r.Body
		if r.Status != 200 || len(b) < 44+24000 || string(b[:4]) != "RIFF" || binary.LittleEndian.Uint32(b[24:]) != 24000 {
			t.Fatalf("%s: %d %.300s", label, r.Status, b)
		}
		return b
	}
	outputs := map[string][2][]byte{}
	for _, tg := range targets {
		h := bearer(tg.Key)
		h["Content-Type"] = "application/json"
		ref := checkWAV(tg.Name+" reference voice", do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Good morning, this is a test.","voice":"harbor"}`))

		body, ct := voiceForm(voicePart{name: "name", value: "Cloned"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
		ah := bearer(tg.Admin)
		ah["Content-Type"] = ct
		created := do(t, "POST", tg.BaseURL+"/_localbase/voices", ah, body)
		if created.Status != 201 {
			t.Fatalf("%s create voice: %d %s", tg.Name, created.Status, created.Body)
		}
		id := created.JSON(t)["id"].(string)
		// The warm server loads voices at launch: the new voice restarts it.
		custom := checkWAV(tg.Name+" custom voice", do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Good morning, this is a test.","voice":"`+id+`"}`))
		do(t, "DELETE", tg.BaseURL+"/_localbase/voices/"+id, bearer(tg.Admin), "")
		outputs[tg.Name] = [2][]byte{ref, custom}
	}
	// Sampling differs run to run, so compare what was said: whisper must hear
	// the sentence in every output.
	if os.Getenv("LOCALBASE_CONTRACT_STT_MODELS") != "" {
		goTarget := targets[len(targets)-1]
		for name, outs := range outputs {
			for i, wav := range outs {
				heard := transcribe(t, goTarget, wav)
				t.Logf("%s output %d (%.1f s): %s", name, i, float64(len(wav)-44)/48000, heard)
				if !strings.Contains(strings.ToLower(heard), "good morning") {
					t.Errorf("%s output %d does not say the input: %q", name, i, heard)
				}
			}
		}
	}
	if len(targets) == 2 {
		a, b := outputs["ts"], outputs["go"]
		t.Logf("reference voice identical: %v (%d vs %d bytes); custom voice identical: %v (%d vs %d bytes)",
			bytes.Equal(a[0], b[0]), len(a[0]), len(b[0]), bytes.Equal(a[1], b[1]), len(a[1]), len(b[1]))
	}
}

// transcribe runs whisper tiny.en on a 24 kHz WAV through tg (afconvert to 16 kHz).
func transcribe(t *testing.T, tg *Target, wav []byte) string {
	t.Helper()
	in := filepath.Join(scratchDir(t), "in.wav")
	out := filepath.Join(scratchDir(t), "out.wav")
	mustWriteFile(in, wav, 0o600)
	if b, err := exec.Command("afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", in, out).CombinedOutput(); err != nil {
		t.Fatalf("afconvert: %v %s", err, b)
	}
	clip, _ := os.ReadFile(out)
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, _ := w.CreateFormFile("file", "speech.wav")
	fw.Write(clip)
	w.WriteField("model", "whisper-tiny-en-q8_0")
	w.WriteField("response_format", "text")
	w.Close()
	h := bearer(tg.Key)
	h["Content-Type"] = w.FormDataContentType()
	r := do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", h, b.String())
	if r.Status != 200 {
		t.Fatalf("transcribe: %d %s", r.Status, r.Body)
	}
	return string(r.Body)
}
