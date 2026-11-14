package contract

import (
	"bytes"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

type formCase struct {
	name   string
	fields [][2]string // name, value; a value of "@file" adds a WAV-named file part
	raw    string      // when set, sent as the body with a JSON content type
}

func multipartBody(fields [][2]string) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, f := range fields {
		if f[1] == "@file" {
			fw, _ := w.CreateFormFile(f[0], "a.wav")
			fw.Write([]byte("RIFF0000WAVEfmt "))
			continue
		}
		w.WriteField(f[0], f[1])
	}
	w.Close()
	return b.String(), w.FormDataContentType()
}

// TestTranscriptionRejections sends invalid transcription requests and requires an
// error envelope for each. (Until step 7 TS and Go had to answer each the same.)
func TestTranscriptionRejections(t *testing.T) {
	tgs := pinnedGateways(t)
	cases := []formCase{
		{name: "no file", fields: [][2]string{{"model", "whisper-base-en-q8_0"}}},
		{name: "file as text", fields: [][2]string{{"file", "not a file"}}},
		{name: "bad format", fields: [][2]string{{"file", "@file"}, {"response_format", "mp3"}}},
		{name: "temperature high", fields: [][2]string{{"file", "@file"}, {"temperature", "2"}}},
		{name: "temperature text", fields: [][2]string{{"file", "@file"}, {"temperature", "hot"}}},
		{name: "blank model", fields: [][2]string{{"file", "@file"}, {"model", "  "}}},
		{name: "bad granularity", fields: [][2]string{{"file", "@file"}, {"timestamp_granularities[]", "char"}}},
		{name: "unknown model", fields: [][2]string{{"file", "@file"}, {"model", "no-such-model"}}},
		{name: "json body", raw: `{"model":"whisper-base-en-q8_0"}`},
	}
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
		for _, c := range cases {
			for _, tg := range tgs {
				h := harness.Bearer(tg.Key)
				var body string
				if c.raw != "" {
					body = c.raw
					h["Content-Type"] = "application/json"
				} else {
					var ct string
					body, ct = multipartBody(c.fields)
					h["Content-Type"] = ct
				}
				r := harness.Do(t, "POST", tg.BaseURL+path, h, body)
				if r.Status < 400 {
					t.Errorf("%s %s %s: %d %s", tg.Name, path, c.name, r.Status, r.Body)
					continue
				}
				r.ErrorCode(t)
				pinError(t, path+" "+c.name, r)
			}
		}
	}
	// Method and permission.
	each(t, func(t *testing.T, tg *harness.Target) {
		// POST only (DESIGN §13.6; TS read any method's body as a form).
		const wantGET = 405
		if r := harness.Do(t, "GET", tg.BaseURL+"/v1/audio/transcriptions", harness.Bearer(tg.Key), ""); r.Status != wantGET {
			t.Errorf("GET: %d, want %d", r.Status, wantGET)
		}
		body, ct := multipartBody([][2]string{{"file", "@file"}})
		h := harness.Bearer(tg.ChatOnly)
		h["Content-Type"] = ct
		if r := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", h, body); r.Status != 403 {
			t.Errorf("chat-only key: %d %s", r.Status, r.Body)
		}
	})
}

// speechClip is a 16 kHz mono WAV of spoken English: a copy of a reference
// voice clip from the live install (read only), converted with afconvert.
func speechClip(t *testing.T) []byte {
	t.Helper()
	src := filepath.Join(os.Getenv("HOME"), ".local/share/local-base/models/tts/qwen3-tts-harbor.wav")
	if _, err := os.Stat(src); err != nil {
		t.Skip("no speech clip available")
	}
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert is not available")
	}
	out := filepath.Join(fsguard.ScratchDir(t), "speech-16k.wav")
	if b, err := exec.Command("afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", src, out).CombinedOutput(); err != nil {
		t.Fatalf("afconvert: %v %s", err, b)
	}
	b, _ := os.ReadFile(out)
	return b
}

// TestTranscriptionReal transcribes a clip with whisper tiny.en in every response format and
// requires a transcript.
func TestTranscriptionReal(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_STT_MODELS") == "" || os.Getenv("LOCALBASE_CONTRACT_RUNTIMES") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_STT_MODELS and LOCALBASE_CONTRACT_RUNTIMES")
	}
	clip := speechClip(t)
	results := map[string]map[string]string{}
	for _, tg := range harness.Targets(t) {
		results[tg.Name] = map[string]string{}
		for _, format := range []string{"json", "text", "verbose_json", "srt", "vtt"} {
			var b bytes.Buffer
			w := multipart.NewWriter(&b)
			fw, _ := w.CreateFormFile("file", "speech.wav")
			fw.Write(clip)
			w.WriteField("model", "whisper-tiny-en-q8_0")
			w.WriteField("response_format", format)
			w.Close()
			h := harness.Bearer(tg.Key)
			h["Content-Type"] = w.FormDataContentType()
			r := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", h, b.String())
			if r.Status != 200 {
				t.Fatalf("%s %s: %d %s", tg.Name, format, r.Status, r.Body)
			}
			results[tg.Name][format] = string(r.Body)
		}
		if !strings.Contains(strings.ToLower(results[tg.Name]["text"]), " ") {
			t.Errorf("%s: transcript looks empty: %q", tg.Name, results[tg.Name]["text"])
		}
	}
	t.Logf("transcript: %s", results[harness.Targets(t)[0].Name]["text"])
}
