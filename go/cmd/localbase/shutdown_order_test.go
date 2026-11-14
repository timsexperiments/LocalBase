package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// TestRuntimesStopBeforeGatewayStopped runs a real gateway with a backend up for
// every modality and stops it: "LocalBase stopped." must be the last word about
// them. The media runtimes used to stop after it, so a reader of the log, or a
// service manager waiting on it, saw a stopped gateway with backends running.
func TestRuntimesStopBeforeGatewayStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	w, err := lab.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Build(ctx, false); err != nil {
		t.Fatal(err)
	}
	g, err := w.Start(ctx, lab.Options{CtxSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop(30 * time.Second)
	c := lab.NewClient(g.Base)
	for _, r := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"` + lab.ChatSmall + `","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/images/generations", `{"model":"` + lab.Image + `","prompt":"a boat","n":1,"size":"512x512"}`},
		{"/v1/audio/speech", `{"model":"` + lab.TTS + `","input":"hello there","voice":"default"}`},
	} {
		if res := c.PostJSON(ctx, r.path, g.Keys.User, r.body); res.Err != nil || res.Status != http.StatusOK {
			t.Fatalf("%s: %d %v %.300s", r.path, res.Status, res.Err, res.Body)
		}
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	mw.WriteField("model", lab.STT)
	file, _ := mw.CreateFormFile("file", "a.wav")
	file.Write(lab.WAV(8000))
	mw.Close()
	if res := c.Do(ctx, "POST", "/v1/audio/transcriptions", g.Keys.User, mw.FormDataContentType(), form.Bytes()); res.Err != nil || res.Status != http.StatusOK {
		t.Fatalf("transcription: %d %v %.300s", res.Status, res.Err, res.Body)
	}
	if _, err := g.Stop(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	stopped := map[string]bool{}
	gatewayStopped := false
	tail := &lab.LogTail{Path: g.LogPath, Keep: func(lab.Event) bool { return false }, Seen: func(e lab.Event) {
		switch e.Name {
		case "gateway.stopped":
			gatewayStopped = true
		case "runtime.stopped":
			if gatewayStopped {
				t.Errorf("the %s runtime stopped after gateway.stopped", e.Runtime)
			}
			stopped[e.Runtime] = true
		}
	}}
	if err := tail.Read(); err != nil {
		t.Fatal(err)
	}
	if !gatewayStopped {
		t.Error("no gateway.stopped event")
	}
	for _, runtime := range []string{"llm", "image", "tts", "stt"} {
		if !stopped[runtime] {
			t.Errorf("no runtime.stopped for %s: %v", runtime, stopped)
		}
	}
}
