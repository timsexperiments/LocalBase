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

// Each field is kept only as its own kind, so a backend's error can't break the schema: texts as
// texts, the byte figures as whole numbers, and code as a text or a whole number.
func TestTheRunErrorKeepsEachFieldAsItsOwnKind(t *testing.T) {
	got := listError(map[string]any{"type": 5, "message": 5, "param": true, "oldestExpiresAt": 1.0,
		"heldBytes": 1.5, "limitBytes": "7", "finishedBytes": true, "code": 1.5})
	if len(got) != 3 || got["type"] != "server_error" || got["code"] != "upstream_error" {
		t.Errorf("only wrong kinds should give the no-result error: %v", got)
	}
	got = listError(map[string]any{"type": "t", "message": "m", "param": "p", "oldestExpiresAt": "2026-11-12T00:00:00Z",
		"heldBytes": float64(1 << 20), "limitBytes": int64(2 << 20), "finishedBytes": 3, "code": float64(400)})
	want := map[string]any{"type": "t", "message": "m", "param": "p", "oldestExpiresAt": "2026-11-12T00:00:00Z",
		"heldBytes": float64(1 << 20), "limitBytes": int64(2 << 20), "finishedBytes": 3, "code": float64(400)}
	if len(got) != len(want) {
		t.Errorf("right kinds: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %v (%T), want %v", k, got[k], got[k], v)
		}
	}
	for _, c := range []struct {
		field string
		value any
	}{
		{"type", 5}, {"message", 5.0}, {"param", false}, {"oldestExpiresAt", int64(1)},
		{"heldBytes", 1.5}, {"limitBytes", "7"}, {"finishedBytes", true},
		{"code", 1.5}, {"code", true}, {"code", map[string]any{}},
	} {
		got := listError(map[string]any{"type": "t", "message": "m", c.field: c.value})
		if c.field == "type" || c.field == "message" {
			got = listError(map[string]any{"param": "p", c.field: c.value})
		}
		if v, ok := got[c.field]; ok {
			t.Errorf("%s as %T was kept: %v", c.field, c.value, v)
		}
	}
	if got := listError(map[string]any{"code": "x"}); got["code"] != "x" || len(got) != 1 {
		t.Errorf("a text code: %v", got)
	}
}

// An error with nothing to keep, such as a backend's {"detail": "x"}, says the run ended without a
// result, so a failed run always says why.
func TestARunErrorWithNothingToKeepSaysTheRunEndedWithoutAResult(t *testing.T) {
	for _, e := range []map[string]any{{}, {"detail": "x"}, {"error": map[string]any{"message": "nested"}}} {
		got := listError(e)
		if got["type"] != "server_error" || got["code"] != "upstream_error" || got["message"] != "The run ended without a result." || len(got) != 3 {
			t.Errorf("%v gave %v", e, got)
		}
	}
}

// A backend's error event in the stream, as llama-server sends it and the relay forwards it: code
// is a number, and the backend's own fields come too. The list and the read give the same error,
// with code 400 kept and the rest of the backend's fields, and a field of the wrong kind, left out.
func TestAStreamedBackendErrorComesOutInItsOwnKinds(t *testing.T) {
	ts, _ := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start(streamErr(`{"error":{"code":400,"type":"exceed_context_size_error","message":"the request exceeds the available context size","n_prompt_tokens":5000,"n_ctx":4096,"param":7}}`))
	own := c.WaitStatus(id, "failed")
	e, _ := own["error"].(map[string]any)
	want := map[string]any{"type": "exceed_context_size_error", "code": float64(400), "message": "the request exceeds the available context size"}
	if len(e) != len(want) {
		t.Errorf("the run's read: %v", e)
	}
	listed, _ := listEntry(t, c, id)["error"].(map[string]any)
	for k, v := range want {
		if e[k] != v || listed[k] != v {
			t.Errorf("%s: the read has %v, the list %v, want %v", k, e[k], listed[k], v)
		}
	}
	if len(listed) != len(want) {
		t.Errorf("the list: %v", listed)
	}
}

// A streamed error with nothing LocalBase keeps still says why the run failed, in the list and the read.
func TestAStreamedErrorWithNothingToKeepSaysWhy(t *testing.T) {
	ts, _ := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start(streamErr(`{"error":{"detail":"x"}}`))
	own := c.WaitStatus(id, "failed")
	for where, got := range map[string]any{"read": own["error"], "list": listEntry(t, c, id)["error"]} {
		e, _ := got.(map[string]any)
		if e["code"] != "upstream_error" || e["message"] != "The run ended without a result." {
			t.Errorf("the %s: %v", where, got)
		}
	}
}

// streamErr is the chat content that makes the fake backend send payload as a stream event. The
// test client puts content into its JSON as it is, so the payload's quotes are escaped here.
func streamErr(payload string) string {
	return "fake:streamerr " + strings.ReplaceAll(payload, `"`, `\"`)
}
