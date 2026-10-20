// Command fakebackend speaks enough of llama-server's HTTP API for supervisor
// and gateway tests: /health, /props, /v1/chat/completions (streaming and
// not) and /v1/embeddings. It accepts llama-server's flags and ignores the
// ones it does not need.
//
// Environment: FAKE_FAIL_MODEL=<substring> exits during startup when -m
// contains it; FAKE_DELAY_MS delays each completion.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	host, port, model := "127.0.0.1", "8080", ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			i++
			host = args[i]
		case "--port":
			i++
			port = args[i]
		case "-m":
			i++
			model = args[i]
		}
	}
	if f := os.Getenv("FAKE_FAIL_MODEL"); f != "" && strings.Contains(model, f) {
		fmt.Fprintln(os.Stderr, "fake: refusing to load", model)
		os.Exit(3)
	}
	delay, _ := strconv.Atoi(os.Getenv("FAKE_DELAY_MS"))
	name := model[strings.LastIndex(model, "/")+1:]
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"model_path": model, "build_info": "fake"})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		time.Sleep(time.Duration(delay) * time.Millisecond)
		maxTokens, _ := req["max_tokens"].(float64)
		content := fmt.Sprintf("hello from %s (max_tokens=%d)", name, int(maxTokens))
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range strings.Fields(content) {
				chunk := map[string]any{"object": "chat.completion.chunk", "model": name,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part + " "}}}}
				b, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
				time.Sleep(10 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "created": 1, "model": name,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content}}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": name,
			"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
			"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2}})
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	http.Serve(ln, mux)
}
