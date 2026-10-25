package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// 4xx and 5xx http.request logs name the error envelope's type and code,
// never its message (operator observability audit).
func TestHTTPRequestLogsErrorKind(t *testing.T) {
	buf := &lockedBuffer{}
	testLog = buf
	defer func() { testLog = nil }()
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, _, _ := newTestGateway(t, h)
	testLog = nil
	post(t, ts, "/v1/systemone", `{"model":"`+decisionModel+`","state":"fake:batch","questions":{"ok":{"type":"noul"}}}`)
	post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[]}`)
	post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[{"role":"user","content":"hi"}]}`)
	want := map[string][2]string{
		"POST /v1/systemone -> 400":        {"invalid_request_error", "context_length_exceeded"},
		"POST /v1/chat/completions -> 400": {"invalid_request_error", "validation_failed"},
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil || e["event"] != "http.request" {
			continue
		}
		msg, _ := e["msg"].(string)
		if strings.Contains(sc.Text(), "27,634") || strings.Contains(sc.Text(), "Too small") {
			t.Errorf("an error message reached the log: %s", sc.Text())
		}
		if w, ok := want[msg]; ok {
			seen[msg] = true
			if e["error_type"] != w[0] || e["error_code"] != w[1] {
				t.Errorf("%s: error_type %v error_code %v, want %v", msg, e["error_type"], e["error_code"], w)
			}
		}
		if msg == "POST /v1/chat/completions -> 200" && (e["error_type"] != nil || e["error_code"] != nil) {
			t.Errorf("a success carries error attributes: %s", sc.Text())
		}
	}
	for msg := range want {
		if !seen[msg] {
			t.Errorf("no log line %q in:\n%s", msg, buf.String())
		}
	}
}
