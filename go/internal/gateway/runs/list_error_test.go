package runs

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// listEntry is the run with this id in the run list.
func listEntry(t *testing.T, c gatewaytest.RunClient, id string) map[string]any {
	t.Helper()
	code, body := c.Do("GET", "/_localbase/runs", "")
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	data, _ := body["data"].([]any)
	for _, d := range data {
		if item, _ := d.(map[string]any); item["id"] == id {
			return item
		}
	}
	t.Fatalf("%s isn't in the list: %v", id, body)
	return nil
}

// A run the memory limit stopped says why in the list too, with the same error as its own read, so a
// page that reads only the list (Studio's tray) can show what to do. A run that finished has none.
func TestTheRunListGivesAStoppedRunsError(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	done := c.Start("hello")
	c.WaitStatus(done, "completed")
	mgr.maxHeldTotal = mgr.held.Load() + 1 // Full, with a finished run holding it.
	stopped := c.Start("fake:slow 200")
	own := c.WaitStatus(stopped, "failed")
	want, _ := own["error"].(map[string]any)
	if want == nil || want["code"] != CodeMemoryFull {
		t.Fatalf("the run's own read: %v", own)
	}
	got, _ := listEntry(t, c, stopped)["error"].(map[string]any)
	if got == nil {
		t.Fatalf("the list has no error for the stopped run")
	}
	for _, k := range []string{"type", "code", "message", "heldBytes", "limitBytes", "finishedBytes", "oldestExpiresAt"} {
		if got[k] != want[k] {
			t.Errorf("%s: the list has %v, the run's read %v", k, got[k], want[k])
		}
	}
	if e, ok := listEntry(t, c, done)["error"]; ok {
		t.Errorf("a finished run has an error in the list: %v", e)
	}
}

// The list carries every run a person has, and an error can come from a backend as it sent it. So
// each entry keeps only an error's own fields, with every text bounded and redacted.
func TestTheRunListBoundsAnError(t *testing.T) {
	long := "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789 " + strings.Repeat("x", 5000)
	got := listError(map[string]any{"type": "server_error", "code": "upstream_error", "message": long,
		"param": "messages", "heldBytes": int64(5), "detail": map[string]any{"huge": strings.Repeat("y", 5000)},
		"whatever": strings.Repeat("z", 5000)})
	msg, _ := got["message"].(string)
	if n := utf8.RuneCountInString(msg); n > maxListErrorText || !strings.HasSuffix(msg, "…") {
		t.Errorf("message of %d characters, bound %d: %.40q…", n, maxListErrorText, msg)
	}
	if strings.Contains(msg, "sk-abcdefghij") {
		t.Errorf("a credential reached the list: %.60q", msg)
	}
	if got["type"] != "server_error" || got["code"] != "upstream_error" || got["param"] != "messages" || got["heldBytes"] != int64(5) {
		t.Errorf("known fields: %v", got)
	}
	for _, k := range []string{"detail", "whatever"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the list", k)
		}
	}
	if listError(nil) != nil {
		t.Error("no error should give none")
	}
}

// A run's own read bounds its error the same way, so the read and the list agree exactly, and a
// backend's error can't bring a credential or anything else it sent into the API.
func TestARunsOwnReadBoundsItsError(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("hello")
	c.WaitStatus(id, "completed")
	mgr.mu.Lock()
	r := mgr.runs[id]
	mgr.mu.Unlock()
	r.mu.Lock()
	r.Status = runFailed
	r.Error = map[string]any{"type": "server_error", "code": "upstream_error",
		"message": "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789 " + strings.Repeat("x", 5000),
		"detail":  map[string]any{"prompt": strings.Repeat("y", 5000)}}
	r.mu.Unlock()
	own, _ := c.Get(id)["error"].(map[string]any)
	msg, _ := own["message"].(string)
	if n := utf8.RuneCountInString(msg); n > maxListErrorText || strings.Contains(msg, "sk-abcdefghij") {
		t.Errorf("the run's own read: %d characters, %.50q", n, msg)
	}
	if _, ok := own["detail"]; ok {
		t.Error("the backend's own field reached the run's read")
	}
	listed, _ := listEntry(t, c, id)["error"].(map[string]any)
	if len(listed) != len(own) {
		t.Errorf("the list has %v, the run's read %v", listed, own)
	}
	for k, v := range own {
		if listed[k] != v {
			t.Errorf("%s: the list has %v, the run's read %v", k, listed[k], v)
		}
	}
}
