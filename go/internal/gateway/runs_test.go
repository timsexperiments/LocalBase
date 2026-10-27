package gateway

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink safe for the gateway's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// Today's contract, kept (DESIGN §21): a client that goes away mid-stream
// cancels its /v1/chat/completions generation, upstream included.
func TestChatDisconnectCancelsUpstream(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	logs := &syncBuffer{}
	testLog = logs
	ts := newTestServer(t)
	testLog = nil

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 200"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line %q", line)
	}
	cancel() // the browser tab goes away
	resp.Body.Close()

	if !waitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Fatal("the backend kept generating after the client went away")
	}
	if !waitFor(5*time.Second, func() bool {
		return strings.Contains(logs.String(), `"terminal_source":"request_aborted"`)
	}) {
		t.Errorf("no cancelled inference.completed in the log:\n%s", logs.String())
	}
}
