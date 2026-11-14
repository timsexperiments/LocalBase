package system

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

// TestSpeechKokoroCrispASR synthesizes speech with Kokoro through CrispASR
// on the Go gateway, checks every output shape, and transcribes the result
// with whisper tiny.en to confirm it says the input.
func TestSpeechKokoroCrispASR(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_CRISPASR") == "" || os.Getenv("LOCALBASE_CONTRACT_STT_MODELS") == "" {
		t.Skip("needs LOCALBASE_CONTRACT_CRISPASR and LOCALBASE_CONTRACT_STT_MODELS")
	}
	var tg *harness.Target
	for _, x := range harness.Targets(t) {
		if x.Name == "go" {
			tg = x
		}
	}
	if tg == nil {
		t.Skip("needs the go target")
	}
	sel := harness.Do(t, "GET", tg.BaseURL+"/_localbase/model-backend?model=kokoro-82m-v1.0-q8", harness.Bearer(tg.Admin), "")
	if s, _ := sel.JSON(t)["selection"].(map[string]any); s == nil || s["backendId"] != "crispasr-metal" || s["variantId"] != "crispasr-f16" {
		t.Fatalf("automatic selection: %d %s", sel.Status, sel.Body)
	}
	h := harness.Bearer(tg.Key)
	h["Content-Type"] = "application/json"
	speak := func(extra string) *harness.Resp {
		return harness.Do(t, "POST", tg.BaseURL+"/v1/audio/speech", h,
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
	if r := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/speech", h, `{"model":"kokoro-82m-v1.0-q8","input":"hi","voice":"af_bella"}`); r.Status != 400 || r.ErrorCode(t) != "voice_not_supported" {
		t.Errorf("af_bella: %d %s", r.Status, r.Body)
	}

	// Round trip: whisper hears the input.
	in := filepath.Join(fsguard.ScratchDir(t), "speech.wav")
	out := filepath.Join(fsguard.ScratchDir(t), "speech-16k.wav")
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
	sh := harness.Bearer(tg.Key)
	sh["Content-Type"] = w.FormDataContentType()
	tr := harness.Do(t, "POST", tg.BaseURL+"/v1/audio/transcriptions", sh, b.String())
	text := strings.ToLower(string(tr.Body))
	if tr.Status != 200 || !strings.Contains(text, "weather") || !strings.Contains(text, "lovely") {
		t.Fatalf("transcript: %d %q", tr.Status, tr.Body)
	}
	t.Logf("heard: %s", tr.Body)

	// Marking: on by default (the watermark is detectable); `speech marking
	// off` restarts the server without it on the next request.
	detector := filepath.Join(os.Getenv("LOCALBASE_CONTRACT_CRISPASR"), "rt", "crispasr-macos-arm64", "crispasr")
	marked := func(wav []byte) bool {
		p := filepath.Join(fsguard.ScratchDir(t), "check.wav")
		fsguard.WriteFile(p, wav, 0o600)
		out, _ := exec.Command(detector, "--detect-watermark", p).CombinedOutput()
		return strings.Contains(string(out), "WATERMARK DETECTED")
	}
	if !marked(wav) {
		t.Error("default output carries no AI-content watermark")
	}
	if r := runCLI(t, tg.Root, "--json", "speech", "marking", "off"); r.code != 0 {
		t.Fatalf("speech marking off: %s %s", r.stdout, r.stderr)
	}
	defer runCLI(t, tg.Root, "speech", "marking", "on")
	if r := speak(""); r.Status != 200 || marked(r.Body) {
		t.Errorf("marking off: %d, still marked %v", r.Status, r.Status == 200)
	}
}
