package gateway

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/runs"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// setFor sets a package limit for one test.
func setFor[T any](t *testing.T, v *T, to T) {
	t.Helper()
	before := *v
	*v = to
	t.Cleanup(func() { *v = before })
}

// streamChat posts a streamed chat and reads all of what comes back, or fails once the client has
// waited 30 s.
func streamChat(t *testing.T, base, content string) (int, string) {
	t.Helper()
	body := `{"model":"` + gatewaytest.ChatModel + `","stream":true,"messages":[{"role":"user","content":"` + content + `"}]}`
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("the stream: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the stream was still open after 30 s, with: %q (%v)", b, err)
	}
	return resp.StatusCode, string(b)
}

// Review 02, M6: an answer over what is read whole says so, rather than reading as malformed JSON.
func TestABufferedAnswerOverTheLimitSaysSo(t *testing.T) {
	small := newTestServerConfigured(t, func(c *Config) { c.relayBodyLimit = 64 })
	r := post(t, small, "/v1/chat/completions", gatewaytest.ChatBody("hi"))
	if r.status != 502 || r.code() != "upstream_error" ||
		!strings.Contains(r.body, "The upstream service returned a response larger than 64 bytes.") {
		t.Errorf("an answer over the limit: %d %s", r.status, r.body)
	}
	ts := newTestServer(t) // the default limit
	if r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); r.status != 200 {
		t.Errorf("an answer under it: %d %s", r.status, r.body)
	}
	if got := memory.SizeText(64 << 20); got != "64 MiB" {
		t.Errorf("the limit reads %q", got)
	}
}

// Review 02, M6: a stream that goes quiet after it started ends, with an error event, and the
// backend call ends with it.
func TestAStreamThatGoesQuietEnds(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts := newTestServerConfigured(t, func(c *Config) { c.streamIdleLimit = 300 * time.Millisecond })
	status, body := streamChat(t, ts.URL, "fake:stall")
	if status != 200 || !strings.Contains(body, `"content":"hello`) {
		t.Errorf("what came before it went quiet: %d %q", status, body)
	}
	if !strings.Contains(body, "The model stopped sending output for 300ms.") || !strings.Contains(body, `"code":"upstream_error"`) {
		t.Errorf("no error event: %q", body)
	}
	if !gatewaytest.WaitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the backend call went on")
	}
}

// No limit before a stream's first byte: a long prompt can take many minutes to process.
func TestAStreamMayTakeItsTimeToStart(t *testing.T) {
	t.Setenv("FAKE_DELAY_MS", "900")
	ts := newTestServerConfigured(t, func(c *Config) { c.streamIdleLimit = 300 * time.Millisecond })
	status, body := streamChat(t, ts.URL, "fake:burst 5")
	if status != 200 || !strings.Contains(body, "[DONE]") || strings.Contains(body, "stopped sending output") {
		t.Errorf("a stream that started late: %d %q", status, body)
	}
}

// Review 02, M9: an in-app call holds no more than a run's media result.
func TestAnInAppCallOverTheLimitIsRefused(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	p := &auth.Principal{Kind: "browser-session", ID: "no-auth", Permissions: auth.Permissions}
	setFor(t, &callBodyLimit, 64)
	got := s.Call(context.Background(), p, "GET", "/v1/models", "", nil)
	if got.Status != 502 || !strings.Contains(string(got.Body), `"code":"upstream_error"`) ||
		!strings.Contains(string(got.Body), "The response is larger than 64 bytes, more than an in-app call can hold.") {
		t.Errorf("a response over the limit: %d %s", got.Status, got.Body)
	}
	callBodyLimit = runs.MaxResult
	if got := s.Call(context.Background(), p, "GET", "/v1/models", "", nil); got.Status != 200 {
		t.Errorf("a response under it: %d %s", got.Status, got.Body)
	}
}
