package gateway

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// steppedHost is a 64 GiB unified host whose available memory tests set.
type steppedHost struct{ avail atomic.Int64 }

func (h *steppedHost) Topology() memory.Topology { return host{}.Topology() }
func (h *steppedHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail.Load(), Pressure: "normal"}
}

// emergencyGateway is a gateway whose host turns critical on demand: critical
// stops the LLM runtime mid-request through the pressure monitor.
func emergencyGateway(t *testing.T) (*httptest.Server, *runtime.LLM, func()) {
	t.Helper()
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, llm, mem := newTestGateway(t, h)
	recovery := &runtime.IdleRecovery{Memory: mem}
	recovery.Add(llm)
	monitor := runtime.NewPressureMonitor(mem, recovery, slog.New(slog.NewTextHandler(io.Discard, nil)))
	critical := func() {
		h.avail.Store(5 * memory.GiB)
		monitor.Poll()
	}
	return ts, llm, critical
}

// waitRunning waits until the LLM has a model running.
func waitRunning(t *testing.T, llm *runtime.LLM) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id, _, _ := llm.Running(); id != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the model did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEmergencyStopAnswers503: a request whose runtime is stopped because
// memory turned critical gets a clean 503 service_unavailable with
// Retry-After, not a truncated 200 or a 502.
func TestEmergencyStopAnswers503(t *testing.T) {
	t.Setenv("FAKE_DELAY_MS", "2000") // the completion is in flight for 2 s
	ts, llm, critical := emergencyGateway(t)
	done := make(chan result, 1)
	go func() {
		done <- post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[{"role":"user","content":"hi"}]}`)
	}()
	waitRunning(t, llm)
	time.Sleep(300 * time.Millisecond)
	critical()
	select {
	case res := <-done:
		if res.status != 503 || res.code() != "service_unavailable" || res.header.Get("Retry-After") != "5" ||
			!strings.Contains(res.body, "Inference service is currently restarting or unavailable") {
			t.Fatalf("%d Retry-After=%q %s", res.status, res.header.Get("Retry-After"), res.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted request hung")
	}
}

// TestEmergencyStopEndsStream: a stream whose runtime is stopped because
// memory turned critical ends promptly with a service_unavailable error event
// after the events already sent; it never hangs or ends silently.
func TestEmergencyStopEndsStream(t *testing.T) {
	ts, llm, critical := emergencyGateway(t)
	words := strings.TrimSpace(strings.Repeat("word ", 500)) // 10 ms per word: 5 s
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:reply `+words+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()
	if first, ok := <-lines; !ok || !strings.Contains(first, "chat.completion.chunk") {
		t.Fatalf("first event %q", first)
	}
	waitRunning(t, llm)
	critical()
	var last string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				if !strings.Contains(last, `"code":"service_unavailable"`) || !strings.Contains(last, `"type":"api_error"`) {
					t.Fatalf("stream ended with %q", last)
				}
				return
			}
			if strings.Contains(l, "[DONE]") {
				t.Fatal("the stream completed: the runtime was not stopped")
			}
			last = l
		case <-timeout:
			t.Fatal("the interrupted stream hung")
		}
	}
}
