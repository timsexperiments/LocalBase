package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Every backend stop is logged with its reason and the memory it frees:
// runtime.evicted for memory, runtime.stopped otherwise (operator
// observability audit: a model swap used to show only a start).
func TestStopEvents(t *testing.T) {
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := newTestLLMWith(t, mem)
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	acquire := func(id string) {
		t.Helper()
		l, err := m.Acquire(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		l.Release()
	}
	acquire(modelA)
	acquire(modelB) // switch: A stops
	m.mu.Lock()
	pid := m.cur.proc.PID()
	m.mu.Unlock()
	syscall.Kill(pid, syscall.SIGKILL) // crash: B exits on its own
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), `"reason":"crash"`) {
		time.Sleep(20 * time.Millisecond)
	}
	acquire(modelA)
	m.EmergencyStop(errors.New("critical")) // evicted for memory
	acquire(modelB)
	m.Close() // shutdown

	type ev struct{ event, model, reason string }
	var got []ev
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		name, _ := e["event"].(string)
		if name != "runtime.stopped" && name != "runtime.evicted" {
			continue
		}
		if e["runtime"] != "llm" || !(e["memory_freed_bytes"].(float64) > 0) || e["pid"] == nil {
			t.Errorf("event fields: %s", sc.Text())
		}
		got = append(got, ev{name, e["model_id"].(string), e["reason"].(string)})
	}
	want := []ev{
		{"runtime.stopped", modelA, "switch"},
		{"runtime.stopped", modelB, "crash"},
		{"runtime.evicted", modelA, "memory"},
		{"runtime.stopped", modelB, "shutdown"},
	}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %v, want %v", i, got[i], want[i])
		}
	}
}

// On a host with an NVIDIA GPU, an admission rejection names the pool it
// used (system memory) and says VRAM admission is not qualified.
func TestAdmissionRejectionNamesPoolWithNVIDIA(t *testing.T) {
	defer memory.OverrideNVIDIA([]memory.NvidiaGPU{{Name: "NVIDIA GeForce RTX 3060", MemoryMiB: 12288}})()
	host := newStepHost(memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := newTestLLMWith(t, mem)
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	if _, err := m.Acquire(context.Background(), modelA); err == nil {
		t.Fatal("admitted on 1 GiB")
	}
	var found bool
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var e struct {
			Event string         `json:"event"`
			Attrs map[string]any `json:"attributes"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Event != "runtime.memory-admission-rejected" {
			continue
		}
		found = true
		if e.Attrs["pool"] != "system" || e.Attrs["nvidia_gpu_present"] != true || e.Attrs["vram_admission"] != "not_qualified" {
			t.Errorf("attributes %v", e.Attrs)
		}
	}
	if !found {
		t.Fatalf("no rejection event in:\n%s", logs.String())
	}
}
