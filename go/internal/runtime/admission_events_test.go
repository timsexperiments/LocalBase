package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

type loggedEvent struct {
	Level      string         `json:"level"`
	Msg        string         `json:"msg"`
	Event      string         `json:"event"`
	Component  string         `json:"component"`
	Runtime    string         `json:"runtime"`
	Attributes map[string]any `json:"attributes"`
}

func events(t *testing.T, logs *syncBuffer, names ...string) []loggedEvent {
	t.Helper()
	var out []loggedEvent
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var e loggedEvent
		json.Unmarshal(sc.Bytes(), &e)
		for _, n := range names {
			if e.Event == n {
				out = append(out, e)
			}
		}
	}
	return out
}

// diagnosticKeys are the TS memoryAdmissionRejectedAttributes keys.
var diagnosticKeys = []string{"reason", "pool", "measured_available_bytes", "reserve_bytes", "pending_bytes",
	"requested_bytes", "effective_available_bytes", "sample_captured_at_ms", "sample_age_ms", "measured_pressure",
	"safety_state", "recovery_samples", "demand_confidence"}

func checkRejected(t *testing.T, e loggedEvent, reason string, context map[string]any) {
	t.Helper()
	if e.Level != "WARN" || e.Msg != "Insufficient memory to start the requested runtime." {
		t.Errorf("event %s %q", e.Level, e.Msg)
	}
	for _, k := range diagnosticKeys {
		if _, ok := e.Attributes[k]; !ok {
			t.Errorf("missing %s in %v", k, e.Attributes)
		}
	}
	if e.Attributes["reason"] != reason {
		t.Errorf("reason %v, want %s", e.Attributes["reason"], reason)
	}
	for k, v := range context {
		if e.Attributes[k] != v {
			t.Errorf("%s = %v, want %v", k, e.Attributes[k], v)
		}
	}
	if len(e.Attributes) != len(diagnosticKeys)+len(context) {
		t.Errorf("attributes %v", e.Attributes)
	}
}

// newLoggedService is the test STT service on host, logging to logs, whose
// activation updates the configuration as the gateway's does.
func newLoggedService(t *testing.T, host *stepHost, logs *syncBuffer) *Service {
	t.Helper()
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	s, _ := newTestServiceWith(t, mem)
	s.d.Log = slog.New(slog.NewJSONHandler(logs, nil))
	s.d.Activate = func(kind, id string) { s.d.Config().Active[kind] = id }
	return s
}

// TestSwitchPreflightRejectedEvent: a media switch refused before anything
// stops logs runtime.memory-admission-rejected with TS's diagnostics and the
// switch-preflight stage, and the current model keeps running.
func TestSwitchPreflightRejectedEvent(t *testing.T) {
	host := newStepHost(30 * memory.GiB)
	var logs syncBuffer
	s := newLoggedService(t, host, &logs)
	ctx := context.Background()
	l, err := s.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	host.avail.Store(10 * memory.GiB) // with a's credit, still under 1.25 x reserve
	if _, err := s.Acquire(ctx, "b"); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("switch: %v", err)
	}
	if s.Running() != "a" {
		t.Fatalf("running %q", s.Running())
	}
	got := events(t, &logs, "runtime.memory-admission-rejected")
	if len(got) != 1 {
		t.Fatalf("events %+v", got)
	}
	e := got[0]
	if e.Component != "whisper-server" || e.Runtime != "stt" {
		t.Errorf("component %s runtime %s", e.Component, e.Runtime)
	}
	checkRejected(t, e, "memory-pressure", map[string]any{"stage": "switch-preflight", "from_model": "a", "to_model": "b"})
	// The preflight credits the running model: measured = sample + a.
	if e.Attributes["measured_available_bytes"] != float64(10*memory.GiB+memory.GiB+4) || e.Attributes["pool"] != "system" ||
		e.Attributes["safety_state"] != "healthy" || e.Attributes["demand_confidence"] != "estimated" {
		t.Errorf("diagnostics %v", e.Attributes)
	}
}

// TestPostStopRollback: a switch whose preflight passed but whose memory
// does not settle after the previous model stopped is rolled back (TS
// rollBackRejectedSwitch): post-stop runtime.memory-admission-rejected and
// model.switch-rolled-back, the previous model active again and reloaded
// once memory admits it.
func TestPostStopRollback(t *testing.T) {
	defer func(d, i time.Duration) { postStopSettle, postStopInterval = d, i }(postStopSettle, postStopInterval)
	postStopSettle, postStopInterval = 300*time.Millisecond, 20*time.Millisecond
	host := newStepHost(30 * memory.GiB)
	var logs syncBuffer
	s := newLoggedService(t, host, &logs)
	ctx := context.Background()
	l, err := s.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	// Passes the preflight only with a's credit; the host never shows it.
	host.avail.Store(11*memory.GiB + memory.GiB/2)
	if _, err := s.Acquire(ctx, "b"); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("switch: %v", err)
	}
	if active := s.d.Config().Active["stt"]; active != "a" {
		t.Fatalf("active %q after rollback", active)
	}
	host.avail.Store(30 * memory.GiB) // memory returns: the restored model reloads
	deadline := time.Now().Add(5 * time.Second)
	for s.Running() != "a" {
		if time.Now().After(deadline) {
			t.Fatalf("restored model not reloaded (running %q)", s.Running())
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := events(t, &logs, "runtime.memory-admission-rejected", "model.switch-rolled-back")
	if len(got) != 2 || got[0].Event != "runtime.memory-admission-rejected" || got[1].Event != "model.switch-rolled-back" {
		t.Fatalf("events %+v", got)
	}
	checkRejected(t, got[0], "memory-pressure", map[string]any{"stage": "post-stop", "from_model": "a", "to_model": "b"})
	if got[0].Attributes["measured_available_bytes"] != float64(11*memory.GiB+memory.GiB/2) {
		t.Errorf("post-stop sample is uncredited: %v", got[0].Attributes)
	}
	rb := got[1]
	if rb.Component != "whisper-server" || rb.Attributes["from_model"] != "a" || rb.Attributes["to_model"] != "b" || rb.Attributes["reload"] != true {
		t.Errorf("rolled back %+v", rb)
	}
}

// TestStartRejectedEvent: a start refused for memory logs the supervisor's
// event (no stage), named for the launched server.
func TestStartRejectedEvent(t *testing.T) {
	host := newStepHost(10 * memory.GiB)
	var logs syncBuffer
	s := newLoggedService(t, host, &logs)
	if _, err := s.Acquire(context.Background(), "a"); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("start: %v", err)
	}
	got := events(t, &logs, "runtime.memory-admission-rejected")
	if len(got) != 1 {
		t.Fatalf("events %+v", got)
	}
	checkRejected(t, got[0], "memory-pressure", nil)
}

// TestLLMSwitchRejectedEvents: the LLM logs the same switch-preflight and
// post-stop refusals as the media runtimes.
func TestLLMSwitchRejectedEvents(t *testing.T) {
	defer func(d, i time.Duration) { postStopSettle, postStopInterval = d, i }(postStopSettle, postStopInterval)
	postStopSettle, postStopInterval = 300*time.Millisecond, 20*time.Millisecond
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := newTestLLMWith(t, mem)
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()
	l, err := m.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()

	host.avail.Store(2 * memory.GiB) // refused even with A's credit
	if _, err := m.Acquire(ctx, modelB); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("switch: %v", err)
	}
	if id, _, _ := m.Running(); id != modelA {
		t.Fatalf("running %q", id)
	}
	host.avail.Store(11 * memory.GiB) // passes with A's credit, never settles
	if _, err := m.Acquire(ctx, modelB); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("switch: %v", err)
	}
	got := events(t, &logs, "runtime.memory-admission-rejected", "model.switch-rolled-back")
	if len(got) < 3 {
		t.Fatalf("events %+v", got)
	}
	if got[0].Attributes["stage"] != "switch-preflight" || got[0].Component != "llama-server" {
		t.Errorf("preflight %+v", got[0])
	}
	checkRejected(t, got[0], got[0].Attributes["reason"].(string), map[string]any{"stage": "switch-preflight", "from_model": modelA, "to_model": modelB})
	checkRejected(t, got[1], "memory-pressure", map[string]any{"stage": "post-stop", "from_model": modelA, "to_model": modelB})
	if got[2].Event != "model.switch-rolled-back" || got[2].Attributes["reload"] != true {
		t.Errorf("rolled back %+v", got[2])
	}
}
