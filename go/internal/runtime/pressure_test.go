package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

type pressureStep struct {
	AvailableGiB *float64 `json:"availableGiB"`
	Pressure     string   `json:"pressure"`
	Idle         int      `json:"idle"`
	Grace        bool     `json:"grace"`
}

type pressureEvent struct {
	Severity   string            `json:"severity"`
	Event      string            `json:"event"`
	Attributes map[string]string `json:"attributes"`
}

// sequenceHost replays one step's reading.
type sequenceHost struct{ step *pressureStep }

func (h *sequenceHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 48 * memory.GiB}
}

func (h *sequenceHost) Snapshot(context.Context) memory.Snapshot {
	if h.step.AvailableGiB == nil {
		return memory.Snapshot{Available: -1, Pressure: "unknown"}
	}
	p := h.step.Pressure
	if p == "" {
		p = "normal"
	}
	return memory.Snapshot{Available: int64(math.Round(*h.step.AvailableGiB * float64(memory.GiB))), Pressure: p}
}

// recordingTarget answers each step's idle count and grace and records calls.
type recordingTarget struct {
	step    *pressureStep
	actions []string
	reason  error
}

func (r *recordingTarget) evictIdle() int {
	r.actions = append(r.actions, "evict-idle")
	return r.step.Idle
}

func (r *recordingTarget) evictAll(reason error) {
	r.actions = append(r.actions, "evict-all")
	r.reason = reason
}

func (r *recordingTarget) withinReclaimGrace() bool { return r.step.Grace }

// TestMemoryPressureGolden replays the TS sample sequences
// (tools/golden/memory-pressure.ts) through the controller and the monitor:
// the same hysteresis state, recovery count and action at every sample, the
// same stops, and the same runtime.memory-pressure-changed events.
func TestMemoryPressureGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/memory-pressure.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Sequences map[string][]struct {
			Step          pressureStep    `json:"step"`
			State         string          `json:"state"`
			NormalSamples int             `json:"normalSamples"`
			Action        string          `json:"action"`
			Actions       []string        `json:"actions"`
			Events        []pressureEvent `json:"events"`
		} `json:"sequences"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Sequences) == 0 {
		t.Fatal("no sequences")
	}
	for name, steps := range golden.Sequences {
		t.Run(name, func(t *testing.T) {
			step := &pressureStep{}
			host := &sequenceHost{step: step}
			mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
			target := &recordingTarget{step: step}
			var logs bytes.Buffer
			m := &PressureMonitor{memory: mem, target: target, last: "healthy",
				log: slog.New(slog.NewJSONHandler(&logs, nil)), FailVideo: func() { target.actions = append(target.actions, "fail-video") }}
			for i, want := range steps {
				*step = want.Step
				target.actions = []string{}
				logs.Reset()
				m.Poll()
				tr := m.lastTransition
				if tr.State != want.State || tr.NormalSamples != want.NormalSamples || tr.Action != want.Action {
					t.Fatalf("step %d: transition %+v, want %s/%d/%s", i, tr, want.State, want.NormalSamples, want.Action)
				}
				if !reflect.DeepEqual(target.actions, want.Actions) {
					t.Fatalf("step %d: actions %v, want %v", i, target.actions, want.Actions)
				}
				events := []pressureEvent{}
				sc := bufio.NewScanner(&logs)
				for sc.Scan() {
					var rec struct {
						Level      string            `json:"level"`
						Event      string            `json:"event"`
						Attributes map[string]string `json:"attributes"`
					}
					json.Unmarshal(sc.Bytes(), &rec)
					events = append(events, pressureEvent{Severity: strings.ToLower(rec.Level), Event: rec.Event, Attributes: rec.Attributes})
				}
				if !reflect.DeepEqual(events, want.Events) {
					t.Fatalf("step %d: events %+v, want %+v", i, events, want.Events)
				}
				if want.State == "critical" && len(target.actions) > 0 && !errors.Is(target.reason, ErrMemoryEmergency) {
					t.Fatalf("step %d: queued work rejected with %v", i, target.reason)
				}
			}
		})
	}
}

// TestPressureStopsRuntimes drives a host sampler down through constrained
// into critical with a real LLM and STT runtime (the fake backend). While
// constrained, the idle STT runtime stops and the LLM serving an active
// request keeps running and answers it; on critical, as in TS
// evictAllRuntimes, queued work is rejected with the memory emergency and
// every runtime stops, the active one included. Recovery needs three normal
// samples, after which requests start the runtimes again.
func TestPressureStopsRuntimes(t *testing.T) {
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	llm := newTestLLMWith(t, mem)
	stt, _ := newTestServiceWith(t, mem)
	recovery := &IdleRecovery{Memory: mem}
	recovery.Add(llm)
	recovery.Add(stt)
	var logs syncBuffer
	monitor := NewPressureMonitor(mem, recovery, slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx := context.Background()

	// STT runs and goes idle; the LLM holds an active request.
	sl, err := stt.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	sl.Release()
	active, err := llm.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Release()
	monitor.Poll()
	if stt.Running() != "a" {
		t.Fatal("healthy sample stopped a runtime")
	}

	// Constrained (below 1.25 x the 9.6 GiB reserve): idle runtimes stop.
	host.avail.Store(11 * memory.GiB)
	monitor.Poll()
	if got := stt.Running(); got != "" {
		t.Fatalf("idle STT runtime still running %q", got)
	}
	if id, _, _ := llm.Running(); id != modelA {
		t.Fatalf("active LLM runtime stopped (running %q)", id)
	}
	if !strings.Contains(chat(t, active), "Qwen_Qwen3.5-9B") {
		t.Fatal("active request lost its runtime")
	}
	// Each constrained sample checks again; the busy LLM stays.
	monitor.Poll()
	if id, _, _ := llm.Running(); id != modelA {
		t.Fatal("second constrained sample stopped the active runtime")
	}

	// Critical: queued work is rejected and every runtime stops.
	queued := make(chan error, 1)
	go func() {
		l, err := llm.Acquire(ctx, modelA)
		if l != nil {
			l.Release()
		}
		queued <- err
	}()
	time.Sleep(200 * time.Millisecond) // the request queues behind the active one
	host.avail.Store(5 * memory.GiB)
	monitor.Poll()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrMemoryEmergency) {
			t.Fatalf("queued request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request was not rejected")
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Fatalf("LLM still running %q on critical", id)
	}
	// Admission refuses while critical, and through two normal samples.
	host.avail.Store(40 * memory.GiB)
	for i := 0; i < 2; i++ {
		monitor.Poll()
		if _, err := stt.Acquire(ctx, "a"); !errors.As(err, new(*memory.AdmissionError)) {
			t.Fatalf("recovery sample %d admitted: %v", i, err)
		}
	}
	monitor.Poll()
	sl, err = stt.Acquire(ctx, "a")
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	sl.Release()

	var changes []string
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var rec struct {
			Level      string            `json:"level"`
			Event      string            `json:"event"`
			Attributes map[string]string `json:"attributes"`
		}
		json.Unmarshal(sc.Bytes(), &rec)
		if rec.Event == "runtime.memory-pressure-changed" {
			changes = append(changes, rec.Level+" "+rec.Attributes["previous_state"]+">"+rec.Attributes["current_state"])
		}
	}
	want := []string{"WARN healthy>constrained", "ERROR constrained>critical", "INFO critical>healthy"}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("pressure events %v, want %v", changes, want)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent log writes.
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
