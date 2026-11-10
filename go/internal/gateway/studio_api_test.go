package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// pngWith is a PNG holding the given text chunks before its image data, in
// the layout sd-server writes.
func pngWith(chunks ...[2]string) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	add := func(kind string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		body := append([]byte(kind), data...)
		b.Write(body)
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(body))
	}
	add("IHDR", make([]byte, 13))
	for _, c := range chunks {
		add("tEXt", []byte(c[0]+"\x00"+c[1]))
	}
	add("IDAT", []byte("x"))
	add("IEND", nil)
	return b.Bytes()
}

// Real sd-server text (the 2026-11-01 run on the pinned build), shortened.
const sdParameters = "a red apple on a table \nSteps: 4, CFG scale: 7.000000, Guidance: 3.500000, Eta: inf, Seed: 43, Size: 256x256, " +
	`Model: DreamShaper_8_pruned.safetensors, RNG: cuda, Sampler: NONE, Version: stable-diffusion.cpp, SDCPP: {"seed":43}`

func TestImageSeedFromPNG(t *testing.T) {
	for name, c := range map[string]struct {
		png  []byte
		seed int64
		ok   bool
	}{
		"sd-server": {pngWith([2]string{"parameters", sdParameters}), 43, true},
		// The prompt comes first, so text in it cannot pose as the seed.
		"prompt says otherwise": {pngWith([2]string{"parameters", "draw Seed: 99, Size: 1x1 on a sign\n" + sdParameters}), 43, true},
		"other text first":      {pngWith([2]string{"Software", "x"}, [2]string{"parameters", sdParameters}), 43, true},
		"no parameters":         {pngWith([2]string{"Software", "Seed: 7, Size: 1x1"}), 0, false},
		"no seed in them":       {pngWith([2]string{"parameters", "a prompt"}), 0, false},
		"not a PNG":             {[]byte("\xff\xd8\xff\xe0 jpeg"), 0, false},
		"cut short":             {pngWith([2]string{"parameters", sdParameters})[:40], 0, false},
		"empty":                 {nil, 0, false},
	} {
		if seed, ok := imageSeedFromPNG(c.png); seed != c.seed || ok != c.ok {
			t.Errorf("%s: %d %v", name, seed, ok)
		}
	}
}

// The file wins; a JPEG falls back to the requested seed + i; with neither,
// the field stays out rather than guessing.
func TestAnnotateImageSeeds(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	jpeg := enc([]byte("\xff\xd8\xff\xe0 jpeg"))
	response := func(images ...string) map[string]any {
		var data []any
		for _, b := range images {
			data = append(data, map[string]any{"b64_json": b})
		}
		return map[string]any{"data": data}
	}
	seeds := func(o map[string]any) []any {
		var out []any
		for _, d := range o["data"].([]any) {
			out = append(out, d.(map[string]any)["seed"])
		}
		return out
	}
	req := int64(7)
	o := response(enc(pngWith([2]string{"parameters", sdParameters})))
	annotateImageSeeds(o, &req)
	if got := seeds(o); got[0] != int64(43) {
		t.Errorf("the file should win over the request: %v", got)
	}
	o = response(jpeg, jpeg)
	annotateImageSeeds(o, &req)
	if got := seeds(o); got[0] != int64(7) || got[1] != int64(8) {
		t.Errorf("jpeg with a seed: %v", got)
	}
	o = response(jpeg)
	annotateImageSeeds(o, nil)
	if got := seeds(o); got[0] != nil {
		t.Errorf("jpeg with no seed: %v", got)
	}
	annotateImageSeeds(map[string]any{"data": []any{"not an object"}}, &req) // no panic
}

func imageSeeds(t *testing.T, body []byte) []float64 {
	t.Helper()
	var r struct {
		Data []struct {
			Seed *float64 `json:"seed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %.200s", err, body)
	}
	var out []float64
	for _, d := range r.Data {
		if d.Seed == nil {
			t.Fatalf("no seed in %.200s", body)
		}
		out = append(out, *d.Seed)
	}
	return out
}

func equalSeeds(a []float64, b ...float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// data[i].seed through /v1 and through a run: seeds are base + i, the base is
// the runtime's default (42) when the request sets none, and all n images
// come back in one result.
func TestImageSeedEcho(t *testing.T) {
	ts := newImageGateway(t, imageModel)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	r := post(t, ts, "/v1/images/generations", `{"prompt":"an apple","n":2}`)
	if got := imageSeeds(t, []byte(r.body)); r.status != 200 || !equalSeeds(got, 42, 43) {
		t.Errorf("no seed: %d %v", r.status, got)
	}
	r = post(t, ts, "/v1/images/generations", `{"prompt":"an apple","seed":100,"n":3}`)
	if got := imageSeeds(t, []byte(r.body)); !equalSeeds(got, 100, 101, 102) {
		t.Errorf("seed 100: %v", got)
	}
	code, m := c.Do("POST", "/_localbase/runs/v1/images/generations", `{"prompt":"an apple","seed":7,"n":4}`)
	if code != 202 {
		t.Fatalf("run: %d %v", code, m)
	}
	id := m["id"].(string)
	c.WaitStatus(id, "completed")
	resp, err := http.Get(ts.URL + "/_localbase/runs/" + id + "/result")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if got := imageSeeds(t, b); !equalSeeds(got, 7, 8, 9, 10) {
		t.Errorf("through a run: %v", got)
	}
}

// A decision is a run target: the page can leave while the model loads, the
// result is the /v1 body, and scope and refusals are /v1's.
func TestRunDecision(t *testing.T) {
	ts := newTestServer(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	body := `{"model":"` + decisionModel + `","state":"x","questions":{"done":{"type":"noul"}}}`
	code, m := c.Do("POST", "/_localbase/runs/v1/systemone", body)
	if code != 202 || m["label"] != "Decision" || m["model"] != decisionModel {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	m = c.WaitStatus(id, "completed")
	res, _ := m["result"].(map[string]any)
	if res == nil || m["output"] != nil || !strings.HasPrefix(res["contentType"].(string), "application/json") {
		t.Fatalf("run: %v", m)
	}
	resp, err := http.Get(ts.URL + "/_localbase/runs/" + id + "/result")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"answers"`) || !strings.Contains(string(b), `"noul"`) {
		t.Errorf("result: %s", b)
	}
	// Refused before the queue exactly as /v1 refuses.
	bad := `{"model":"` + gatewaytest.ChatModel + `","state":null,"questions":{"q":{"type":"noul"}}}`
	v1 := post(t, ts, "/v1/systemone", bad)
	got := post(t, ts, "/_localbase/runs/v1/systemone", bad)
	if got.status != v1.status || got.status == 202 || got.body != v1.body {
		t.Errorf("refusal: run %d %s, /v1 %d %s", got.status, got.body, v1.status, v1.body)
	}

	s := runServer(ts)
	s.NoAuth = false
	_, chatOnly, _ := s.Store.CreateAPIKey(t.Context(), "chat", []string{"inference:chat"})
	if code, _ := (gatewaytest.RunClient{T: t, Base: ts.URL, Key: chatOnly}).Do("POST", "/_localbase/runs/v1/systemone", body); code != 403 {
		t.Errorf("no inference:decision: %d", code)
	}
}

// Translation is a run target on the transcription handler; with no runtime
// it is refused as /v1 refuses it. (The real path was run on whisper-server;
// DESIGN §24.)
func TestRunTranslationNotConfigured(t *testing.T) {
	ts := newTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("RIFF"))
	mw.Close()
	send := func(path string) (int, string) {
		resp, err := http.Post(ts.URL+path, mw.FormDataContentType(), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	v1, v1Body := send("/v1/audio/translations")
	code, body := send("/_localbase/runs/v1/audio/translations")
	if code != v1 || code == 202 || body != v1Body {
		t.Errorf("run %d %s, /v1 %d %s", code, body, v1, v1Body)
	}
}
