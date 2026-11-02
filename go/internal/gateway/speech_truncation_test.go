package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const speechModel = "qwen3-tts-1.7b-base-q4_k_m"

// newSpeechGateway is a --no-auth gateway whose speech runtime is the fake
// tts-server.
func newSpeechGateway(t *testing.T, logs io.Writer) *httptest.Server {
	t.Helper()
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(speechModel)
	modelsDir := r.ModelsDir("tts")
	os.MkdirAll(modelsDir, 0o700)
	installSparse(t, modelsDir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Active: map[string]string{"tts": speechModel}, Selected: map[string][]string{"tts": {speechModel}}}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	gate := func(id string) (*runtime.Target, error) {
		if id != speechModel {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: reg.Get("tts-server"), Dir: modelsDir}, nil
	}
	// The production log pipeline, which redacts: a plain JSON logger would show
	// fields an operator never sees.
	sink, err := obs.OpenSink(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	sink.Mirror = logs
	log := slog.New(obs.NewHandler(sink, slog.LevelInfo))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "tts", Kind: "tts", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: modelsDir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{"tts-server": fakeBinary},
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	ts := httptest.NewServer(New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log,
		NoAuth: true, MemoryGB: 64, Root: r, Memory: mem, TTS: svc}))
	t.Cleanup(ts.Close)
	return ts
}

// numberList is the input that exposed the cut on the real runtime
// (2026-11-01): "1, 2, 3, ..." to 256 characters, which came back at exactly
// 20.48 s with the end missing.
func numberList() string {
	var b strings.Builder
	for i := 1; b.Len() < 256; i++ {
		b.WriteString(strconv.Itoa(i) + ", ")
	}
	return b.String()[:256]
}

// Prose of the same length fits (14.08 s on the real runtime).
const speechProse = "The old lighthouse stood at the edge of the bay, and every evening the keeper climbed its narrow stairs to light the lamp. " +
	"Ships passing in the dark would see the beam sweep across the water and know that the rocks were near and home was not far away now."

func speechBody(input string, extra string) string {
	b, _ := json.Marshal(input)
	return `{"model":"` + speechModel + `","input":` + string(b) + `,"voice":"default"` + extra + `}`
}

const speechCapBytes = 256 * 1920 * 2

// loggedEvent is the first event of a name in the pipeline's JSONL.
func loggedEvent(t *testing.T, jsonl, name string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(jsonl, "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["eventName"] == name {
			return e
		}
	}
	t.Fatalf("no %s event in:\n%s", name, jsonl)
	return nil
}

// Audio cut at the frame cap is unchanged, and says so: a warn event without
// the text, and a header on every way /v1 can answer.
func TestSpeechTruncationVisible(t *testing.T) {
	logs := &syncBuffer{}
	ts := newSpeechGateway(t, logs)
	numbers := numberList()
	if len(numbers) != 256 {
		t.Fatalf("fixture is %d characters", len(numbers))
	}

	r := post(t, ts, "/v1/audio/speech", speechBody(numbers, ""))
	if r.status != 200 || r.header.Get("X-Localbase-Audio-Truncated") != "true" {
		t.Fatalf("numbers: %d %v", r.status, r.header)
	}
	first := r.header.Get("X-Localbase-Request-Id")
	if len(r.body) != 44+speechCapBytes || !strings.HasPrefix(r.body, "RIFF") {
		t.Errorf("the audio changed: %d bytes", len(r.body))
	}
	for _, extra := range []string{`,"response_format":"pcm"`, `,"stream_format":"audio"`, `,"stream_format":"sse"`} {
		if got := post(t, ts, "/v1/audio/speech", speechBody(numbers, extra)); got.header.Get("X-Localbase-Audio-Truncated") != "true" {
			t.Errorf("%s: no header: %v", extra, got.header)
		}
	}

	event := loggedEvent(t, logs.String(), "speech.output_truncated")
	attrs, _ := event["attributes"].(map[string]any)
	if event["severity"] != "warn" || event["requestId"] != first || attrs["model_id"] != speechModel || attrs["frames"] != 256.0 || attrs["input_chars"] != 256.0 {
		t.Fatalf("event: %v", event)
	}
	if strings.Contains(logs.String(), "1, 2, 3") {
		t.Error("the input text reached the log")
	}

	// Audio that fits is not flagged.
	before := strings.Count(logs.String(), "speech.output_truncated")
	r = post(t, ts, "/v1/audio/speech", speechBody(speechProse, ""))
	if r.status != 200 || r.header.Get("X-Localbase-Audio-Truncated") != "" || len(r.body) >= 44+speechCapBytes {
		t.Errorf("prose: %d, %d bytes, %v", r.status, len(r.body), r.header)
	}
	if got := strings.Count(logs.String(), "speech.output_truncated"); got != before {
		t.Errorf("prose logged a truncation")
	}
}

// Run results keep no headers, so the same flag is result.notes, on the run
// object and on its end event; the audio is the same bytes.
func TestSpeechTruncationInRun(t *testing.T) {
	logs := &syncBuffer{}
	ts := newSpeechGateway(t, logs)
	c := runClient{t: t, base: ts.URL}
	start := func(input string) string {
		code, m := c.do("POST", "/_localbase/runs/v1/audio/speech", speechBody(input, ""))
		if code != 202 {
			t.Fatalf("create: %d %v", code, m)
		}
		return m["id"].(string)
	}
	id := start(numberList())
	m := c.waitStatus(id, "completed")
	notes := func(run map[string]any) []any {
		res, _ := run["result"].(map[string]any)
		n, _ := res["notes"].([]any)
		return n
	}
	if n := notes(m); len(n) != 1 || n[0] != "audio_truncated" {
		t.Fatalf("notes: %v", m)
	}
	evs, _, _ := c.events(id, nil, "", nil)
	var end map[string]any
	json.Unmarshal([]byte(evs[len(evs)-1].data), &end)
	if n := notes(end); evs[len(evs)-1].name != "end" || len(n) != 1 || n[0] != "audio_truncated" {
		t.Errorf("end event: %v", end)
	}
	resp, err := http.Get(ts.URL + "/_localbase/runs/" + id + "/result")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if len(b) != 44+speechCapBytes {
		t.Errorf("result: %d bytes", len(b))
	}
	// The run's size survives the log pipeline too.
	if attrs, _ := loggedEvent(t, logs.String(), "run.completed")["attributes"].(map[string]any); attrs["output_bytes"] != float64(len(b)) {
		t.Errorf("run.completed: %v", attrs)
	}
	id = start(speechProse)
	m = c.waitStatus(id, "completed")
	if res, _ := m["result"].(map[string]any); res == nil || res["notes"] != nil {
		t.Errorf("prose run: %v", m)
	}
}
