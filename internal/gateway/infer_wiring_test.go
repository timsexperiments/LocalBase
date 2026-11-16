package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// An inference is traced by the gateway's exporter: the /v1 routes get the gateway's Otel, and
// their span, localbase.inference, reaches a collector.
func TestAnInferenceIsTraced(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	defer collector.Close()
	cfg, err := obs.ResolveOtel(collector.URL, "", 100, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	rt := otlp.New(cfg, "test", func(string, string, map[string]any) {})
	ts := newTestServerConfigured(t, func(c *Config) { c.Otel = rt })
	if got := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); got.status != 200 {
		t.Fatalf("chat: %d %s", got.status, got.body)
	}
	rt.Shutdown() // sends what is held
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(bytes.Join(bodies, nil), []byte("localbase.inference")) {
		t.Errorf("no localbase.inference span among %d exports", len(bodies))
	}
}

// Speech knows the gateway's stored voices: the /v1 routes get the gateway's voice store, so a
// voice saved through /_localbase/voices is one /v1/audio/speech accepts, and an id that names no
// stored voice is still refused as not found.
func TestSpeechKnowsTheStoredVoices(t *testing.T) {
	ts := openAPIGateway(t)
	_, key := oaKey(t, runServer(ts), "all", auth.Permissions...)
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	w.WriteField("name", "Wiring voice")
	w.WriteField("consent", "true")
	part, _ := w.CreateFormFile("audio", "a.wav")
	part.Write(voiceClipWAV())
	w.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/_localbase/voices", &form)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var made struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&made)
	resp.Body.Close()
	if resp.StatusCode != 201 || made.ID == "" {
		t.Fatalf("storing a voice: %d %q", resp.StatusCode, made.ID)
	}
	c := oaClient{t, ts.URL, key}
	speak := func(voice string) (int, string) {
		status, code, _, _ := c.doBody(`{"model":"`+speechModel+`","input":"hello","voice":"`+voice+`"}`, "POST", "/v1/audio/speech")
		return status, code
	}
	if status, code := speak(made.ID); code == "voice_not_found" {
		t.Errorf("a stored voice was not found: %d %s", status, code)
	}
	if status, code := speak("voice-0123456789ab"); status != 400 || code != "voice_not_found" {
		t.Errorf("a voice that isn't stored: %d %s", status, code)
	}
}
