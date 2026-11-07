package runs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// A finished run is deleted with its checkpoint after its retention.
func TestRunExpiry(t *testing.T) {
	logs := &gatewaytest.SyncBuffer{}
	gatewaytest.Log = logs
	ts, mgr := newRunHarness(t)
	gatewaytest.Log = nil
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:reply done")
	c.WaitStatus(id, "completed")
	dir := mgr.dir
	fi, err := os.Stat(filepath.Join(dir, id+".json"))
	di, _ := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("checkpoint modes: %v %v %v", err, fi, di)
	}
	mgr.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if _, m := c.Do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("expired run listed: %v", m)
	}
	if code, _ := c.Do("GET", "/_localbase/runs/"+id, ""); code != 404 {
		t.Errorf("expired run: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Errorf("expired checkpoint: %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"run.expired"`) {
		t.Error("no run.expired event")
	}
	// The startup sweep removes expired files too.
	os.WriteFile(filepath.Join(dir, "run_00.json"), []byte(`{"id":"run_00","owner":"local","status":"completed","expiresAt":"2020-01-01T00:00:00Z"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "junk.json"), []byte(`x`), 0o600)
	NewManager(stubOptions(dir)).Shutdown(0)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left after the startup sweep: %v", left)
	}
}

// Per-principal active runs and buffered output are capped.
func TestRunLimits(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	var ids []string
	for range ActivePerOwner {
		ids = append(ids, c.Start("fake:slow 200"))
	}
	code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi"))
	if code != 429 || m["error"].(map[string]any)["code"] != "too_many_runs" {
		t.Errorf("over the active cap: %d %v", code, m)
	}
	for _, id := range ids {
		c.Do("POST", "/_localbase/runs/"+id+"/cancel", "")
	}
	os.Remove(cancelLog)
	mgr.maxOutput = 2000
	id := c.Start("fake:slow 200")
	m = c.WaitStatus(id, "failed")
	if m["reason"] != "output_limit" || gatewaytest.Content(m) == "" {
		t.Errorf("over the output cap: %v", m)
	}
	if !gatewaytest.WaitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the backend kept generating past the output cap")
	}
}

// A quiet run keeps its stream alive with comments, and the stream headers
// suit Cloudflare and Safari.
func TestRunKeepAlive(t *testing.T) {
	t.Setenv("FAKE_DELAY_MS", "1500") // nothing to say for 1.5 s
	ts, mgr := newRunHarness(t)
	mgr.ping = 100 * time.Millisecond
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:reply quiet")
	_, comments, h := c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return e == nil && false })
	if comments < 5 {
		t.Errorf("%d keep-alive comments over a quiet 1.5 s", comments)
	}
	if h.Get("Cache-Control") != "no-store, no-transform" || h.Get("X-Accel-Buffering") != "no" || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Errorf("headers: %v", h)
	}
}
