package main

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// freeRun finds n free ports in a row, in this test process's part of the test block. They are
// bound only when each runtime starts: a run from another process's part lost its LLM port to that
// process's backends now and then, and the chat got a 503.
func freeRun(t *testing.T, n int) int {
	t.Helper()
	base, ok := testutil.FreeRun(n)
	if !ok {
		t.Skip("no free run of ports")
	}
	return base
}

// The flag reaches every runtime it can start here, on a real gateway: a scratch
// gateway must not need five flags to stay off another gateway's ports.
func TestRuntimePortBaseServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := lab.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Build(ctx, false); err != nil {
		t.Fatal(err)
	}
	base := freeRun(t, 5)
	g, err := w.Start(ctx, lab.Options{CtxSize: 8192, RuntimePortBase: base})
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
	for i, name := range []string{"llm", "stt", "tts", "image"} {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", base+i), time.Second)
		if err != nil {
			t.Errorf("%s is not on port %d: %v", name, base+i, err)
			continue
		}
		conn.Close()
	}
}
