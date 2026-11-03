package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGatewayRoundTrip proves a lab gateway serves every modality over the fake
// backends, reads the fake host, and leaves nothing running.
func TestGatewayRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Build(ctx, false); err != nil {
		t.Fatal(err)
	}
	g, err := w.Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop(20 * time.Second)
	post := func(path, ct string, body []byte) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", g.Base+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+g.Keys.User)
		req.Header.Set("Content-Type", ct)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	js := "application/json"
	for _, c := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"` + ChatSmall + `","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/embeddings", `{"model":"` + Embedding + `","input":"hi"}`},
		{"/v1/systemone", `{"model":"` + Decision + `","state":"s","questions":{"q":{"type":"choice","criteria":{"a":"first","b":"second"}}}}`},
		{"/v1/images/generations", `{"model":"` + Image + `","prompt":"a boat","n":1,"size":"512x512"}`},
		{"/v1/audio/speech", `{"model":"` + TTS + `","input":"hello there","voice":"default"}`},
	} {
		code, body := post(c.path, js, []byte(c.body))
		if code != 200 {
			t.Errorf("%s: %d %.300s", c.path, code, body)
		}
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("model", STT)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(WAV(8000))
	mw.Close()
	if code, body := post("/v1/audio/transcriptions", mw.FormDataContentType(), b.Bytes()); code != 200 || !strings.Contains(body, "harbor") {
		t.Errorf("transcription: %d %.300s", code, body)
	}
	s, err := g.Stats(ctx)
	if err != nil || s.Goroutines == 0 || s.FDs <= 0 || s.RSS <= 0 {
		t.Errorf("stats: %+v %v", s, err)
	}
	var meta struct {
		Host struct {
			Memory struct {
				System struct {
					CapacityBytes  int64 `json:"capacityBytes"`
					AvailableBytes int64 `json:"availableBytes"`
				} `json:"system"`
			} `json:"memory"`
		} `json:"host"`
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", g.Base+"/_localbase/models", nil)
	req.Header.Set("Authorization", "Bearer "+g.Keys.User)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if meta.Host.Memory.System.CapacityBytes != 64*gib || meta.Host.Memory.System.AvailableBytes != 48*gib {
		t.Errorf("the gateway does not read the fake host: %+v", meta)
	}
	if procs, _ := w.Backends(); len(procs) == 0 {
		t.Errorf("no backends running after requests to every modality")
	}
	took, err := g.Stop(30 * time.Second)
	if err != nil {
		t.Errorf("stop: %v", err)
	}
	t.Logf("stopped in %v", took)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if procs, _ := w.Backends(); len(procs) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if procs, _ := w.Backends(); len(procs) != 0 {
		t.Errorf("left running after a clean stop: %+v", procs)
	}
	if os.Getenv("LAB_PRINT_LOG") != "" {
		log, _ := os.ReadFile(g.LogPath)
		fmt.Printf("%s\n", log)
	}
}

func TestPercentile(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[float64]time.Duration{50: 50 * time.Millisecond, 99: 99 * time.Millisecond, 100: 100 * time.Millisecond} {
		if got := Percentile(d, p); got != want {
			t.Errorf("p%v = %v, expected %v", p, got, want)
		}
	}
	if Percentile(nil, 50) != 0 {
		t.Error("empty")
	}
}

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]float64{"0:01.50": 1.5, "12:03.00": 723, "1:00:00": 3600, "1-00:00:01": 86401} {
		if got := parseCPUTime(in); got != want {
			t.Errorf("%q = %v, expected %v", in, got, want)
		}
	}
}

// No real sockets: whether a real port is free depends on whatever else is running on
// the machine, which may well be a lab.
func TestPortBlock(t *testing.T) {
	const busy = 29102
	b := &portBlock{low: 29100, high: 29104, taken: map[int]bool{}, free: func(p int) bool { return p != busy }}
	got, err := b.take(3)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, p := range got {
		if p < 29100 || p > 29104 || p == busy || seen[p] {
			t.Errorf("port %d: out of the block, busy or repeated (%v)", p, got)
		}
		seen[p] = true
	}
	if _, err := b.take(2); err == nil {
		t.Error("the block has only four free ports, and three are taken")
	}
}

func TestPortBlockFromEnvironment(t *testing.T) {
	t.Setenv("LOCALBASE_LAB_PORTS", "29100-29104")
	if b := newPortBlock(); b.low != 29100 || b.high != 29104 {
		t.Errorf("block %d-%d", b.low, b.high)
	}
	t.Setenv("LOCALBASE_LAB_PORTS", "nonsense")
	if b := newPortBlock(); b.low != defaultPortLow || b.high != defaultPortHigh {
		t.Errorf("a bad value gave block %d-%d", b.low, b.high)
	}
}

func TestRefusesTheInstalledRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for _, p := range []string{home + "/.local/share/local-base", home + "/.local/share/local-base/x", home + "/.local/bin/y"} {
		if refuseLive(p) == nil {
			t.Errorf("%s was not refused", p)
		}
	}
	if err := refuseLive(t.TempDir()); err != nil {
		t.Error(err)
	}
}

func TestParseProcStat(t *testing.T) {
	line := "4242 (a (weird) name) S 99 1 1 0 -1 0 0 0 0 0 150 50 0 0 20 0 1 0 1 0 2000 0 0"
	r, err := parseProcStat([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if r.ppid != 99 || r.state != "S" || r.cpu != 2 || r.rss != 2000*int64(os.Getpagesize()) {
		t.Errorf("%+v", r)
	}
	if _, err := parseProcStat([]byte("4242 no parens")); err == nil {
		t.Error("a line without a program name")
	}
}

func TestProcessTableFindsThisProcess(t *testing.T) {
	r, ok := rowOf(os.Getpid())
	if !ok || r.rss <= 0 || r.command == "" {
		t.Fatalf("%+v %v", r, ok)
	}
	if procs, err := procsUnder(filepath.Dir(r.command)); err != nil || len(procs) == 0 {
		t.Errorf("procsUnder: %+v %v", procs, err)
	}
}
