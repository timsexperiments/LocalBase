package runs

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// The runs tests run against a Manager whose inner requests go to stubInner, not to a gateway: what
// is tested is what runs do with an answer, and the stub gives the answers they need. A few tests
// in gateway/runs_e2e_test.go keep a whole gateway, with the fake backend, so the stub can't drift from what
// the real routes send unnoticed.

// newRunHarness serves a Manager's routes, as the gateway does for one caller, "local".
func newRunHarness(t *testing.T) (*httptest.Server, *Manager) {
	t.Helper()
	m := NewManager(stubOptions(filepath.Join(t.TempDir(), "runs")))
	go m.Janitor()
	mux := http.NewServeMux()
	mux.HandleFunc("/_localbase/runs", m.List)
	mux.HandleFunc("/_localbase/runs/{rest...}", m.Route)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		m.Shutdown(time.Second)
	})
	return ts, m
}

// stubOptions are a Manager's options with stubInner behind it, for one caller, "local", keeping
// its runs in dir.
func stubOptions(dir string) Options {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if gatewaytest.Log != nil { // as newTestGateway does
		log = slog.New(slog.NewJSONHandler(gatewaytest.Log, nil))
	}
	return Options{
		Handler: stubInner(),
		Owner:   func(*http.Request) string { return "local" },
		Log:     func() *slog.Logger { return log },
		Otel:    func() *otlp.Runtime { return nil },
		Dir:     dir,
	}
}

// stubInner answers a run's inner request the way the gateway's /v1 routes do, for what the runs
// tests send:
//   - a chat (POST /v1/chat/completions, streamed, as runs always ask): the last message's
//     content says what comes back. "fake:slow <n>" streams n words 50 ms apart, "fake:burst <n>"
//     streams n words at once, "fake:reply <text>" streams the text, "fake:context" is refused
//     with 400 and the backend's context error; anything else streams "ok".
//     A model other than the catalog's chat model is refused with 404 before the queue.
//   - an image (POST /v1/images/generations): a JSON result, as the image route answers.
//
// Like the real routes, it tells the run when its request is queued and admitted (RunHooks), once
// the body is accepted, and how the answer ended: the run waits for "queued" before it answers its
// own caller. A chat stream stops when the request's context ends, as the relay's does.
func stubInner() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input"}}`))
			return
		}
		if req.Model != gatewaytest.ChatModel { // refused before the queue, as the chat route refuses it
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"The model does not exist."}}`))
			return
		}
		hooks := httpx.RunHooksFrom(r.Context())
		hooks.Queue()
		hooks.Admit()
		mode := req.Messages[len(req.Messages)-1].Content
		if mode == "fake:context" {
			hooks.Finish("") // an upstream error status, as relayUpstreamError ends it
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"The request exceeds the context size."}}`))
			return
		}
		words, pause := []string{"ok"}, time.Duration(0)
		words2 := func(n string) []string {
			k, _ := strconv.Atoi(n)
			out := make([]string, k)
			for i := range out {
				out[i] = fmt.Sprintf("w%d ", i+1)
			}
			return out
		}
		switch {
		case strings.HasPrefix(mode, "fake:slow "):
			words, pause = words2(strings.TrimPrefix(mode, "fake:slow ")), 50*time.Millisecond
		case strings.HasPrefix(mode, "fake:burst "):
			words = words2(strings.TrimPrefix(mode, "fake:burst "))
		case strings.HasPrefix(mode, "fake:reply "):
			words = []string{strings.TrimPrefix(mode, "fake:reply ")}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) {
			b, _ := json.Marshal(map[string]any{"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": gatewaytest.ChatModel,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flush()
		}
		// When the request ends early, the backend is told to stop: the fake backend notes it in
		// FAKE_CANCEL_LOG, and so does this.
		cancelled := func() {
			hooks.Finish("request_aborted")
			if f := os.Getenv("FAKE_CANCEL_LOG"); f != "" {
				if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					fmt.Fprintln(fh, "cancelled")
					fh.Close()
				}
			}
		}
		// FAKE_DELAY_MS keeps the backend quiet that long first, as the fake backend's does.
		if d, _ := strconv.Atoi(os.Getenv("FAKE_DELAY_MS")); d > 0 {
			select {
			case <-r.Context().Done():
				cancelled()
				return
			case <-time.After(time.Duration(d) * time.Millisecond):
			}
		}
		chunk(map[string]any{"role": "assistant"}, nil)
		for _, word := range words {
			if pause > 0 {
				select {
				case <-r.Context().Done():
					cancelled()
					return
				case <-time.After(pause):
				}
			}
			if r.Context().Err() != nil {
				cancelled()
				return
			}
			chunk(map[string]any{"content": word}, nil)
		}
		chunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
		hooks.Finish("finish_reason")
	})
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hooks := httpx.RunHooksFrom(r.Context())
		hooks.Queue()
		hooks.Admit()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":1,"data":[{"b64_json":"iVBORw0KGgo="}]}`))
	})
	return mux
}
