package contract

import (
	"bytes"
	"encoding/binary"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestDifferentialSpeechRejections sends invalid speech requests to both
// gateways and requires the same status, type, code, param and message.
// None of them reaches a backend.
func TestDifferentialSpeechRejections(t *testing.T) {
	tgs := pinnedGateways(t)
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
		{"chat model", `{"model":"` + harness.ChatModel + `","input":"hi","voice":"default"}`},
		{"missing custom voice", `{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"hi","voice":"voice-0123456789ab"}`},
		{"voice not on model", `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"harbor"}`},
	}
	for _, c := range cases {
		var first *harness.Resp
		var firstName string
		for _, tg := range tgs {
			h := harness.Bearer(tg.Key)
			h["Content-Type"] = "application/json"
			r := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/speech", h, c.body)
			if r.Status < 400 {
				t.Errorf("%s %s: %d %s", tg.Name, c.name, r.Status, r.Body)
				continue
			}
			r.ErrorCode(t)
			pinError(t, c.name, tg.Name, r)
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
	checkWAV := func(label string, r *harness.Resp) []byte {
		t.Helper()
		b := r.Body
		if r.Status != 200 || len(b) < 44+24000 || string(b[:4]) != "RIFF" || binary.LittleEndian.Uint32(b[24:]) != 24000 {
			t.Fatalf("%s: %d %.300s", label, r.Status, b)
		}
		return b
	}
	outputs := map[string][2][]byte{}
	for _, tg := range harness.Targets(t) {
		h := harness.Bearer(tg.Key)
		h["Content-Type"] = "application/json"
		ref := checkWAV(tg.Name+" reference voice", harness.Do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Good morning, this is a test.","voice":"harbor"}`))

		body, ct := voiceForm(voicePart{name: "name", value: "Cloned"}, voicePart{name: "consent", value: "true"}, voicePart{name: "audio", file: clip})
		ah := harness.Bearer(tg.Admin)
		ah["Content-Type"] = ct
		created := harness.Do(t, "POST", tg.BaseURL+"/_localbase/voices", ah, body)
		if created.Status != 201 {
			t.Fatalf("%s create voice: %d %s", tg.Name, created.Status, created.Body)
		}
		id := created.JSON(t)["id"].(string)
		// The warm server loads voices at launch: the new voice restarts it.
		custom := checkWAV(tg.Name+" custom voice", harness.Do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
			`{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Good morning, this is a test.","voice":"`+id+`"}`))
		harness.Do(t, "DELETE", tg.BaseURL+"/_localbase/voices/"+id, harness.Bearer(tg.Admin), "")
		outputs[tg.Name] = [2][]byte{ref, custom}
	}
	// Sampling differs run to run, so compare what was said: whisper must hear
	// the sentence in every output.
	if os.Getenv("LOCALBASE_CONTRACT_STT_MODELS") != "" {
		goTarget := harness.Targets(t)[len(harness.Targets(t))-1]
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
	if len(harness.Targets(t)) == 2 {
		a, b := outputs["ts"], outputs["go"]
		t.Logf("reference voice identical: %v (%d vs %d bytes); custom voice identical: %v (%d vs %d bytes)",
			bytes.Equal(a[0], b[0]), len(a[0]), len(b[0]), bytes.Equal(a[1], b[1]), len(a[1]), len(b[1]))
	}
}

// transcribe runs whisper tiny.en on a 24 kHz WAV through tg (afconvert to 16 kHz).
func transcribe(t *testing.T, tg *harness.Target, wav []byte) string {
	t.Helper()
	in := filepath.Join(fsguard.ScratchDir(t), "in.wav")
	out := filepath.Join(fsguard.ScratchDir(t), "out.wav")
	fsguard.WriteFile(in, wav, 0o600)
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
	h := harness.Bearer(tg.Key)
	h["Content-Type"] = w.FormDataContentType()
	r := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", h, b.String())
	if r.Status != 200 {
		t.Fatalf("transcribe: %d %s", r.Status, r.Body)
	}
	return string(r.Body)
}
