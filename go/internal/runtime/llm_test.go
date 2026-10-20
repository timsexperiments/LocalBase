package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var fakeBinary string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "lbgo-fake-")
	fakeBinary = filepath.Join(dir, "llama-server")
	if out, err := exec.Command("go", "build", "-o", fakeBinary, "./fakebackend").CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(out))
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// FreePort returns a free port in the test range 24000-29999, excluding
// ports other local services use.
func FreePort(t testing.TB) int {
	for i := 0; i < 200; i++ {
		p := 24000 + rand.IntN(6000)
		if p == 24173 || (p >= 25611 && p <= 25613) {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

type bigHost struct{ avail int64 }

func (b *bigHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 48 * memory.GiB}
}
func (b *bigHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: b.avail, Pressure: "normal"}
}

const (
	modelA = "qwen3.5-9b-q4_k_m"
	modelB = "qwen3.5-4b-q4_k_m"
)

func newTestLLM(t *testing.T, host *bigHost) *LLM {
	t.Helper()
	dir := t.TempDir()
	models := filepath.Join(dir, "models", "llm")
	os.MkdirAll(models, 0o700)
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{modelA, modelB} {
		os.WriteFile(filepath.Join(models, cat.ByID(id).Primary().Filename), []byte("not a real gguf"), 0o600)
	}
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Host: "127.0.0.1", Port: FreePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active: map[string]string{"llm": modelA}, Selected: map[string][]string{"llm": {modelA, modelB}}}
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := NewLLM(LLMDeps{Root: dir, ModelsDir: models, Store: st, Catalog: cat, Memory: mem,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MemoryGB: 64, Binary: fakeBinary,
		QueueCapacity: 4, QueueTimeout: 3 * time.Second}, cfg)
	t.Cleanup(m.Close)
	return m
}

func chat(t *testing.T, l *Lease) string {
	resp, err := http.Post(l.BaseURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestColdStartAndSwitch(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat(t, l), "Qwen_Qwen3.5-9B") {
		t.Fatal("wrong backend model")
	}
	// A request for B waits for A's lease (one slot) to drain, then switches.
	got := make(chan *Lease)
	go func() {
		lb, err := m.Acquire(context.Background(), modelB)
		if err != nil {
			t.Error(err)
		}
		got <- lb
	}()
	select {
	case <-got:
		t.Fatal("switched while a request was active")
	case <-time.After(200 * time.Millisecond):
	}
	l.Release()
	lb := <-got
	if id, _, _ := m.Running(); id != modelB {
		t.Fatalf("running %s", id)
	}
	lb.Release()
}

func TestFIFOAcrossSwitch(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, _ := m.Acquire(context.Background(), modelA)
	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, id := range []string{modelB, modelA} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			lease, err := m.Acquire(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			lease.Release()
		}()
	}
	time.Sleep(150 * time.Millisecond)
	l.Release()
	wg.Wait()
	// The later request for the current model must not bypass the switch.
	if len(order) != 2 || order[0] != modelB {
		t.Fatalf("order = %v", order)
	}
}

func TestQueueFullAndTimeout(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	m.d.QueueTimeout = 300 * time.Millisecond
	l, _ := m.Acquire(context.Background(), modelA)
	defer l.Release()
	if _, err := m.Acquire(context.Background(), modelA); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	m.d.QueueCapacity = 1
	go m.Acquire(context.Background(), modelA)
	time.Sleep(50 * time.Millisecond)
	if _, err := m.Acquire(context.Background(), modelA); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want full, got %v", err)
	}
}

// #296: a model that fails to start after the old one stopped restores it.
func TestSwitchFailureRestoresPrevious(t *testing.T) {
	t.Setenv("FAKE_FAIL_MODEL", "Qwen3.5-4B")
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, _ := m.Acquire(context.Background(), modelA)
	l.Release()
	if _, err := m.Acquire(context.Background(), modelB); !errors.Is(err, ErrStartupFailed) {
		t.Fatalf("want startup failure, got %v", err)
	}
	if id, _, _ := m.Running(); id != modelA {
		t.Fatalf("running %q, want %s restored", id, modelA)
	}
}

// #355: a switch that fails preflight never stops the current model.
func TestPreflightRejectionKeepsCurrent(t *testing.T) {
	host := &bigHost{avail: 50 * memory.GiB}
	m := newTestLLM(t, host)
	l, _ := m.Acquire(context.Background(), modelA)
	l.Release()
	host.avail = 4 * memory.GiB // 4 + A credit (~8.7) - B (~5.5) is below the 9.6 GiB reserve
	_, err := m.Acquire(context.Background(), modelB)
	var ae *memory.AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("want admission error, got %v", err)
	}
	if id, _, _ := m.Running(); id != modelA {
		t.Fatalf("running %q", id)
	}
}

func TestCrashedBackendRestartsOnNextRequest(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, _ := m.Acquire(context.Background(), modelA)
	l.Release()
	m.mu.Lock()
	pid := m.cur.proc.PID()
	m.mu.Unlock()
	syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if id, _, _ := m.Running(); id == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crash not noticed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	l2, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}
