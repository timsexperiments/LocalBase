// Command fakebackend speaks enough of llama-server's HTTP API for supervisor
// and gateway tests: /health, /props, /v1/chat/completions (streaming and
// not) and /v1/embeddings. It accepts llama-server's flags and ignores the
// ones it does not need.
//
// Environment: FAKE_FAIL_MODEL=<substring> exits during startup when -m
// contains it; FAKE_DELAY_MS delays each completion.
//
// A last user message starting with "fake:" selects a behavior: "fake:reply <text>"
// answers <text>, "fake:context" fails like an overflowing prompt,
// "fake:overflow" fails the same way only while the request has more than
// four messages (so a compacted retry succeeds),
// "fake:reasoning" adds reasoning_content, "fake:truncated" ends a stream
// without [DONE]. "fake:batch" (as the chat message, the embeddings input or
// the decision state) fails like an input over the physical batch size, with
// llama-server's exact 500; "fake:batch-plain" does so without the numbers.
// "fake:slow <n>" streams n words 50 ms apart; when the client goes away
// first it appends "cancelled" to the file FAKE_CANCEL_LOG names.
//
// For a GGUF model it prints llama.cpp's placement lines (DESIGN §20.5): an
// override per expert tensor of the first --n-cpu-moe layers and the
// offloaded layer count. FAKE_PLACEMENT_BUG=cuda0 puts the first expert on
// CUDA0; FAKE_COMPUTE_MIB sets the CUDA0 compute buffer (default 100).
// FAKE_STARTUP_MS delays listening, like a slow model load.
package main

import (
	"encoding/json"
	"regexp"

	"fmt"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	host, port, model := "127.0.0.1", "8080", ""
	ngl, cpuMoE := -1, 0
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host", "--listen-ip":
			i++
			host = args[i]
		case "--port", "--listen-port":
			i++
			port = args[i]
		case "--diffusion-model":
			i++
			model = args[i]
		case "-m":
			i++
			model = args[i]
		case "-ngl":
			i++
			ngl, _ = strconv.Atoi(args[i])
		case "--n-cpu-moe":
			i++
			cpuMoE, _ = strconv.Atoi(args[i])
		}
	}
	placementLines(model, ngl, cpuMoE)
	if ms, _ := strconv.Atoi(os.Getenv("FAKE_STARTUP_MS")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond) // a slow model load
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
	addVideo(mux)
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"model_path": model, "build_info": "fake",
			"chat_template": "{% if tools %}{{ tools }}{% endif %}<think>", "modalities": map[string]any{"vision": false},
			"chat_template_caps": map[string]any{"supports_tool_calls": strings.Contains(name, "tools")}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		time.Sleep(time.Duration(delay) * time.Millisecond)
		maxTokens, _ := req["max_tokens"].(float64)
		content := fmt.Sprintf("hello from %s (max_tokens=%d)", name, int(maxTokens))
		mode := lastUser(req)
		if batchOverflow(w, mode) {
			return
		}
		switch {
		case mode == "fake:context", mode == "fake:overflow" && messageCount(req) > 4:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":400,"message":"request (9000 tokens) exceeds the available context size (8192 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":9000,"n_ctx":8192}}`))
			return
		case strings.HasPrefix(mode, "fake:reply "):
			content = strings.TrimPrefix(mode, "fake:reply ")
		}
		pause := 10 * time.Millisecond
		if n, ok := strings.CutPrefix(mode, "fake:slow "); ok {
			words, _ := strconv.Atoi(n)
			var b strings.Builder
			for i := range words {
				fmt.Fprintf(&b, "w%d ", i+1)
			}
			content, pause = b.String(), 50*time.Millisecond
		}
		if stream, _ := req["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range strings.Fields(content) {
				if r.Context().Err() != nil {
					if f := os.Getenv("FAKE_CANCEL_LOG"); f != "" {
						if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
							fmt.Fprintln(fh, "cancelled")
							fh.Close()
						}
					}
					return
				}
				delta := map[string]any{"content": part + " "}
				if mode == "fake:reasoning" {
					delta["reasoning_content"] = "hm"
				}
				chunk := map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 1, "model": name,
					"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
				b, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
				time.Sleep(pause)
			}
			if mode == "fake:truncated" {
				return
			}
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", name)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "created": 1, "model": name,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": message(mode, content)}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if in, _ := req["input"].(string); batchOverflow(w, in) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": name,
			"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
			"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2}})
	})
	mux.HandleFunc("POST /v1/systemone", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string                    `json:"state"`
			Questions map[string]map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if batchOverflow(w, req.State) {
			return
		}
		answers := map[string]any{}
		for id := range req.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": 0.5}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": name, "answers": answers,
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 1}})
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	http.Serve(ln, mux)
}

func messageCount(req map[string]any) int {
	msgs, _ := req["messages"].([]any)
	return len(msgs)
}

func lastUser(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		if m, _ := msgs[i].(map[string]any); m["role"] == "user" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

func message(mode, content string) map[string]any {
	m := map[string]any{"role": "assistant", "content": content}
	if mode == "fake:reasoning" {
		m["reasoning_content"] = "hm"
	}
	return m
}

// batchOverflow answers llama-server's error for an input over the physical
// batch size (-b/-ub), which it reports as a 500 server_error.
func batchOverflow(w http.ResponseWriter, mode string) bool {
	msg := ""
	switch mode {
	case "fake:batch":
		msg = "input (27634 tokens) is too large to process. increase the physical batch size (current batch size: 8192)"
	case "fake:batch-plain":
		msg = "input is too large to process. increase the physical batch size"
	default:
		return false
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 500, "message": msg, "type": "server_error"}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	w.Write(b)
	return true
}

var fakeExpertRe = regexp.MustCompile(`^blk\.(\d+)\.ffn_(up|down|gate|gate_up)_(ch|)exps`)

func placementLines(model string, ngl, cpuMoE int) {
	keys, err := memory.ReadGGUFHeader(model, func(k string) bool { return strings.HasSuffix(k, ".block_count") })
	if err != nil {
		return
	}
	blocks := 0
	for _, v := range keys {
		if f, ok := v.(float64); ok {
			blocks = int(f)
		}
	}
	tensors, _ := memory.ReadTensorTable(model)
	bug := os.Getenv("FAKE_PLACEMENT_BUG") == "cuda0"
	for _, t := range tensors {
		m := fakeExpertRe.FindStringSubmatch(t.Name)
		if m == nil {
			continue
		}
		if l, _ := strconv.Atoi(m[1]); l < cpuMoE {
			buft := "CPU"
			if bug {
				buft, bug = "CUDA0", false
			}
			fmt.Fprintf(os.Stderr, "tensor %s (%v f32) buffer type overridden to %s\n", t.Name, t.Dims, buft)
		}
	}
	total := blocks + 1
	if ngl < 0 || ngl > total {
		ngl = total
	}
	fmt.Fprintf(os.Stderr, "load_tensors: offloaded %d/%d layers to GPU\n", ngl, total)
	compute := os.Getenv("FAKE_COMPUTE_MIB")
	if compute == "" {
		compute = "100"
	}
	fmt.Fprintf(os.Stderr, "sched_reserve:      CUDA0 compute buffer size = %s MiB\n", compute)
	os.Stderr.Write([]byte("\xff\xfe invalid bytes in the startup log\n"))
}
