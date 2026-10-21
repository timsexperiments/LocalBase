package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// mediaAdmissionGolden is tools/golden/media-admission.ts: the TS switch
// preflight for catalog image and stt models whose files are missing.
type mediaAdmissionGolden struct {
	CapacityBytes      int64                    `json:"capacityBytes"`
	GPUWorkingSetBytes int64                    `json:"gpuWorkingSetBytes"`
	ReserveBytes       int64                    `json:"reserveBytes"`
	Residents          map[string]memory.Demand `json:"residents"`
	Models             []struct {
		ModelID  string        `json:"modelId"`
		Kind     string        `json:"kind"`
		Demand   memory.Demand `json:"demand"`
		Verdicts []struct {
			AvailableBytes int64  `json:"availableBytes"`
			Verdict        string `json:"verdict"`
		} `json:"verdicts"`
	} `json:"models"`
}

// stepHost is a 64 GiB unified host whose available memory tests set.
type stepHost struct{ avail atomic.Int64 }

func newStepHost(avail int64) *stepHost {
	h := &stepHost{}
	h.avail.Store(avail)
	return h
}

func (h *stepHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 48 * memory.GiB}
}
func (h *stepHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail.Load(), Pressure: "normal"}
}

func loadMediaAdmissionGolden(t *testing.T) mediaAdmissionGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/media-admission.json")
	if err != nil {
		t.Fatal(err)
	}
	var g mediaAdmissionGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestMediaAdmissionGolden pins the demand a missing catalog image or stt
// model is sized with and the admission verdict around its threshold, with
// whisper and an embedding model resident, to the TS numbers.
func TestMediaAdmissionGolden(t *testing.T) {
	g := loadMediaAdmissionGolden(t)
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	reserve := memory.Reserve{Percent: 15, MinimumGB: 8}
	if got := reserve.Bytes(g.CapacityBytes); got != g.ReserveBytes {
		t.Fatalf("reserve %d, TS %d", got, g.ReserveBytes)
	}
	seen := 0
	for _, m := range g.Models {
		spec := cat.ByID(m.ModelID)
		if spec == nil {
			t.Errorf("%s: not in the Go catalog", m.ModelID)
			continue
		}
		if got := catalogServiceDemand(cat, spec, 0); got != m.Demand {
			t.Errorf("%s demand %+v, TS %+v", m.ModelID, got, m.Demand)
		}
		if g.CapacityBytes != 64*memory.GiB || g.GPUWorkingSetBytes != 48*memory.GiB {
			t.Fatalf("golden host %d/%d", g.CapacityBytes, g.GPUWorkingSetBytes)
		}
		host := newStepHost(60 * memory.GiB)
		c := memory.NewController(host, reserve, false)
		c.SetSampler(nil)
		pid := 100
		for id, d := range g.Residents {
			r, err := c.Reserve(context.Background(), id, d)
			if err != nil {
				t.Fatal(err)
			}
			pid++
			r.Materialize(pid)
		}
		for _, v := range m.Verdicts {
			host.avail.Store(v.AvailableBytes)
			got := "admitted"
			if err := c.CheckAdmission(context.Background(), m.Demand, []string{m.Kind}); err != nil {
				var ae *memory.AdmissionError
				if !errors.As(err, &ae) {
					t.Fatalf("%s: %v", m.ModelID, err)
				}
				got = ae.Reason
			}
			if got != v.Verdict {
				t.Errorf("%s at %d bytes available: %s, TS %s", m.ModelID, v.AvailableBytes, got, v.Verdict)
			}
		}
		if m.ModelID == "realvis-xl-v5.0" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatal("the golden lacks realvis-xl-v5.0")
	}
}

const missingImage = "realvis-xl-v5.0"

// missingImageBoundary is the least available memory at which TS admits
// missingImage (reserve + demand).
func missingImageBoundary(t *testing.T) int64 {
	t.Helper()
	g := loadMediaAdmissionGolden(t)
	for _, m := range g.Models {
		if m.ModelID == missingImage {
			return g.ReserveBytes + m.Demand.UnifiedBytes
		}
	}
	t.Fatalf("the golden lacks %s", missingImage)
	return 0
}

// newMissingImageService serves image with missingImage selected and its
// files absent, as the contract fixture does.
func newMissingImageService(t *testing.T, host *stepHost, recovery *IdleRecovery) (*Service, *memory.Controller, func() []string) {
	t.Helper()
	cat, _ := catalog.Load("")
	if spec := cat.ByID(missingImage); spec == nil || spec.ImageRuntime != nil {
		t.Fatalf("%s must be a catalog image model without an imageRuntime profile", missingImage)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	modelsDir := filepath.Join(dir, "models", "image")
	os.MkdirAll(modelsDir, 0o700)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	if recovery != nil {
		recovery.Memory = mem
	}
	reg, _ := backends.Load("")
	sd := reg.Get("stable-diffusion.cpp-metal")
	gate := func(string) (*Target, error) {
		m := &models.Model{ID: missingImage, Tasks: []string{"image"}, Variants: []models.Variant{{ID: "default"}}}
		return &Target{ModelID: missingImage, Model: m, Variant: &m.Variants[0], Backend: sd, Dir: modelsDir}, nil
	}
	var mu sync.Mutex
	var activated []string
	cfg := &store.Config{Active: map[string]string{}, Selected: map[string][]string{"image": {missingImage}}}
	s := NewService(ServiceDeps{Modality: "image", Kind: "image", Catalog: cat, Store: st, Memory: mem,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func() *store.Config { return cfg }, Gate: gate,
		ModelsDir: modelsDir, Host: "127.0.0.1", Port: FreePort(t), Recovery: recovery,
		Activate: func(kind, id string) { mu.Lock(); activated = append(activated, kind+":"+id); mu.Unlock() }})
	t.Cleanup(s.Close)
	return s, mem, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), activated...) }
}

// TestServiceMissingCatalogImageChecksFitFirst: as the TS switch preflight,
// a catalog image model whose files are missing (no imageRuntime profile) is
// sized from the catalog before the install check. Too little memory answers
// insufficient memory and leaves the model inactive; enough memory reaches
// the install check, which activates it and then asks for consent.
func TestServiceMissingCatalogImageChecksFitFirst(t *testing.T) {
	boundary := missingImageBoundary(t)
	for _, tc := range []struct {
		avail   int64
		wantFit bool
	}{
		{avail: boundary - 1},
		{avail: 41 * memory.GiB / 2},
		{avail: boundary, wantFit: true},
		{avail: 27 * memory.GiB, wantFit: true},
	} {
		s, _, activated := newMissingImageService(t, newStepHost(tc.avail), nil)
		_, err := s.Acquire(context.Background(), missingImage)
		var ae *memory.AdmissionError
		var ni *NotInstalledError
		switch {
		case !tc.wantFit && !errors.As(err, &ae):
			t.Errorf("%d available: want insufficient memory, got %v", tc.avail, err)
		case tc.wantFit && !errors.As(err, &ni):
			t.Errorf("%d available: want install consent, got %v", tc.avail, err)
		}
		if got := len(activated()) > 0; got != tc.wantFit {
			t.Errorf("%d available: activated %v", tc.avail, activated())
		}
	}
}

// fakePeer is an idle runtime of another modality holding a resident
// reservation; stopping it returns its memory to the host.
type fakePeer struct {
	id      string
	host    *stepHost
	res     *memory.Reservation
	bytes   int64
	idle    bool
	mu      sync.Mutex
	claimed bool
	stopped bool
	freed   int64 // what the host gets back (a footprint below the projection)
}

func newFakePeer(t *testing.T, mem *memory.Controller, host *stepHost, id string, bytes int64, idle bool) *fakePeer {
	t.Helper()
	res, err := mem.Reserve(context.Background(), id, memory.Demand{UnifiedBytes: bytes, HostBytes: bytes, AcceleratorBytes: bytes, Confidence: "measured"})
	if err != nil {
		t.Fatal(err)
	}
	res.Materialize(4242)
	return &fakePeer{id: id, host: host, res: res, bytes: bytes, idle: idle, freed: bytes}
}

func (p *fakePeer) runtimeID() string { return p.id }
func (p *fakePeer) claimIdle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.idle || p.stopped {
		return false
	}
	p.claimed = true
	return true
}
func (p *fakePeer) stopClaimed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.res.Release()
	p.host.avail.Add(p.freed)
}
func (p *fakePeer) unclaim() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claimed = false
}

// TestIdleRecoveryStopsIdlePeers is TS admitModelWithIdleRecovery: a model
// refused for memory stops the idle runtimes of other modalities when that
// makes it fit, waits for the host to show the memory, and is switched to
// (here reaching the install check). Busy runtimes are never stopped.
func TestIdleRecoveryStopsIdlePeers(t *testing.T) {
	boundary := missingImageBoundary(t)
	host := newStepHost(boundary - memory.GiB)
	recovery := &IdleRecovery{}
	s, mem, activated := newMissingImageService(t, host, recovery)
	stt := newFakePeer(t, mem, host, "stt", 600<<20, true)
	llm := newFakePeer(t, mem, host, "llm", 2*memory.GiB, true)
	busy := newFakePeer(t, mem, host, "tts", 4*memory.GiB, false)
	for _, p := range []*fakePeer{stt, llm, busy} {
		recovery.Add(p)
	}
	_, err := s.Acquire(context.Background(), missingImage)
	var ni *NotInstalledError
	if !errors.As(err, &ni) {
		t.Fatalf("after recovery: %v", err)
	}
	if !stt.stopped || !llm.stopped || busy.stopped {
		t.Errorf("stopped stt=%v llm=%v busy tts=%v", stt.stopped, llm.stopped, busy.stopped)
	}
	if stt.claimed || llm.claimed {
		t.Error("claims were not released")
	}
	if a := activated(); len(a) != 1 || a[0] != "image:"+missingImage {
		t.Errorf("activated %v", a)
	}
}

// TestIdleRecoveryKeepsPeersWhenItCannotHelp: when stopping every idle peer
// would still not make room, nothing is stopped and the first refusal is
// answered; when the host does not show the freed memory within the settle
// window, the first refusal is answered too.
func TestIdleRecoveryKeepsPeersWhenItCannotHelp(t *testing.T) {
	boundary := missingImageBoundary(t)
	t.Run("not enough to free", func(t *testing.T) {
		host := newStepHost(boundary - 4*memory.GiB)
		recovery := &IdleRecovery{}
		s, mem, activated := newMissingImageService(t, host, recovery)
		stt := newFakePeer(t, mem, host, "stt", 600<<20, true)
		recovery.Add(stt)
		_, err := s.Acquire(context.Background(), missingImage)
		var ae *memory.AdmissionError
		if !errors.As(err, &ae) {
			t.Fatalf("want insufficient memory, got %v", err)
		}
		if stt.stopped || stt.claimed {
			t.Errorf("stt stopped=%v claimed=%v", stt.stopped, stt.claimed)
		}
		if len(activated()) != 0 {
			t.Errorf("activated %v", activated())
		}
	})
	t.Run("memory does not settle", func(t *testing.T) {
		host := newStepHost(boundary - memory.GiB)
		recovery := &IdleRecovery{settleTimeout: 300 * time.Millisecond, settleInterval: 20 * time.Millisecond}
		s, mem, _ := newMissingImageService(t, host, recovery)
		llm := newFakePeer(t, mem, host, "llm", 2*memory.GiB, true)
		llm.freed = 0
		recovery.Add(llm)
		_, err := s.Acquire(context.Background(), missingImage)
		var ae *memory.AdmissionError
		if !errors.As(err, &ae) || !llm.stopped {
			t.Fatalf("want insufficient memory after stopping the peer, got %v (stopped %v)", err, llm.stopped)
		}
	})
}

// TestServiceClaimHoldsRequests: a claimed Service takes no request until
// it is unclaimed; then the request runs (restarting a stopped backend).
func TestServiceClaimHoldsRequests(t *testing.T) {
	s, _ := newTestService(t)
	l, err := s.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.claimIdle() {
		t.Fatal("claimed a busy service")
	}
	l.Release()
	if !s.claimIdle() {
		t.Fatal("could not claim an idle service")
	}
	got := make(chan error, 1)
	go func() {
		l, err := s.Acquire(context.Background(), "a")
		if err == nil {
			l.Release()
		}
		got <- err
	}()
	s.stopClaimed()
	select {
	case err := <-got:
		t.Fatalf("a request ran while claimed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if s.Running() != "" {
		t.Fatal("still running after stopClaimed")
	}
	s.unclaim()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if s.Running() != "a" {
		t.Errorf("running %q", s.Running())
	}
}

// TestLLMClaimHoldsRequests: the LLM manager as an idle-recovery peer.
func TestLLMClaimHoldsRequests(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if m.claimIdle() {
		t.Fatal("claimed an LLM with an active request")
	}
	l.Release()
	if !m.claimIdle() {
		t.Fatal("could not claim an idle LLM")
	}
	got := make(chan error, 1)
	go func() {
		l, err := m.Acquire(context.Background(), modelA)
		if err == nil {
			l.Release()
		}
		got <- err
	}()
	m.stopClaimed()
	select {
	case err := <-got:
		t.Fatalf("a request ran while claimed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if id, _, _ := m.Running(); id != "" {
		t.Fatalf("still running %s after stopClaimed", id)
	}
	m.unclaim()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if id, _, _ := m.Running(); id != modelA {
		t.Errorf("running %q", id)
	}
}
func (p *fakePeer) EmergencyStop(error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		p.res.Release()
		p.host.avail.Add(p.freed)
	}
}
