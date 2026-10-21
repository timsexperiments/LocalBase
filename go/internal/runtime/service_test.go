package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func newTestService(t *testing.T) (*Service, *[]string) {
	t.Helper()
	mem := memory.NewController(&bigHost{avail: 50 * memory.GiB}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	return newTestServiceWith(t, mem)
}

// newTestServiceWith is newTestService on a given memory controller.
func newTestServiceWith(t *testing.T, mem *memory.Controller) (*Service, *[]string) {
	t.Helper()
	dir := t.TempDir()
	cat, _ := catalog.Load("")
	reg, _ := backends.Load("")
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	modelsDir := filepath.Join(dir, "models", "stt")
	os.MkdirAll(modelsDir, 0o700)
	for _, n := range []string{"a.bin", "b.bin"} {
		os.WriteFile(filepath.Join(modelsDir, n), []byte("ggml"), 0o600)
	}
	cpu := *reg.Get("whisper.cpp-metal")
	cpu.Platforms = []backends.Platform{{OS: "*", Arch: "*", Accelerator: "cpu"}}
	gate := func(id string) (*Target, error) {
		if id != "a" && id != "b" {
			return nil, ErrModelNotFound
		}
		m := &models.Model{ID: id, Tasks: []string{"transcription"}, Variants: []models.Variant{{ID: "v",
			Artifacts: []models.Artifact{{Filename: id + ".bin", Role: models.RoleWeights, Required: true}}}}}
		return &Target{ModelID: id, Model: m, Variant: &m.Variants[0], Backend: &cpu, Dir: modelsDir}, nil
	}
	var mu sync.Mutex
	activated := []string{}
	cfg := &store.Config{Active: map[string]string{"stt": "a"}, Selected: map[string][]string{"stt": {"a", "b"}}}
	s := NewService(ServiceDeps{Modality: "stt", Kind: "stt", Catalog: cat, Store: st, Memory: mem,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func() *store.Config { return cfg }, Gate: gate,
		ModelsDir: modelsDir, Host: "127.0.0.1", Port: FreePort(t), Binaries: map[string]string{"whisper-server": fakeBinary},
		Activate: func(kind, id string) { mu.Lock(); activated = append(activated, kind+":"+id); mu.Unlock() }})
	t.Cleanup(s.Close)
	return s, &activated
}

func TestServiceSwitchSerializeAndRecover(t *testing.T) {
	s, activated := newTestService(t)
	ctx := context.Background()
	if id, err := s.Resolve(""); err != nil || id != "a" {
		t.Fatalf("resolve active: %q %v", id, err)
	}
	l, err := s.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// One request at a time: a second acquire waits for the release.
	got := make(chan time.Time, 1)
	go func() {
		l2, err := s.Acquire(ctx, "a")
		if err != nil {
			t.Error(err)
			return
		}
		got <- time.Now()
		l2.Release()
	}()
	time.Sleep(150 * time.Millisecond)
	released := time.Now()
	l.Release()
	if at := <-got; at.Before(released) {
		t.Error("two requests ran at once")
	}
	// Switching models replaces the backend and activates the new model.
	l, err = s.Acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if s.Running() != "b" {
		t.Fatalf("running %q", s.Running())
	}
	pid := l.b.proc.PID()
	l.Release()
	if len(*activated) == 0 || (*activated)[len(*activated)-1] != "stt:b" {
		t.Errorf("activations %v", *activated)
	}
	// A crash clears the slot; the next request starts it again.
	proc, _ := os.FindProcess(pid)
	proc.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for s.Running() != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Running() != "" {
		t.Fatal("the crashed backend still looks resident")
	}
	l, err = s.Acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if l.b.proc.PID() == pid {
		t.Error("not restarted")
	}
	l.Release()
	if _, err := s.Resolve("nope"); err != ErrModelNotFound {
		t.Errorf("unknown model: %v", err)
	}
}

func TestServiceCloseRejectsQueue(t *testing.T) {
	s, _ := newTestService(t)
	l, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := s.Acquire(context.Background(), "a"); errc <- err }()
	time.Sleep(100 * time.Millisecond)
	go func() { time.Sleep(100 * time.Millisecond); l.Release() }()
	s.Close()
	if err := <-errc; err != ErrUnavailable {
		t.Fatalf("queued request after close: %v", err)
	}
}
