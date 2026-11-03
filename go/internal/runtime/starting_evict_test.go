package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

var errCritical = errors.New("critical")

func acquireLater(m interface {
	Acquire(context.Context, string) (*Lease, error)
}, id string) chan error {
	done := make(chan error, 1)
	go func() {
		l, err := m.Acquire(context.Background(), id)
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	return done
}

func waitLoading(t *testing.T, loading func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !loading(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the start never began")
		}
	}
	time.Sleep(200 * time.Millisecond) // past the spawn, into the wait for health
}

// Memory turned critical while a model was starting. It was not in cur, so the
// eviction missed it, and when ready it installed itself into the emergency.
func TestEmergencyStopEndsAStartingLLM(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "1500")
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	done := acquireLater(m, modelA)
	waitLoading(t, func() bool { return m.Loading(modelA) })
	m.EmergencyStop(errCritical)
	if err := <-done; !errors.Is(err, errCritical) {
		t.Errorf("the waiting request: %v", err)
	}
	time.Sleep(2 * time.Second) // past when the start would have finished
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("%s came up after the emergency", id)
	}
	if snap := m.Snapshot(); snap.State == "failed" {
		t.Errorf("an eviction is reported as a failed start: %+v", snap)
	}
	// The eviction leaves nothing stuck: the next request starts it.
	t.Setenv("FAKE_STARTUP_MS", "0")
	if err := <-acquireLater(m, modelA); err != nil {
		t.Errorf("after the emergency: %v", err)
	}
}

func TestEmergencyStopEndsAStartingService(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "1500")
	s, _ := newTestService(t)
	done := make(chan error, 1)
	go func() {
		l, err := s.Acquire(context.Background(), "a")
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	waitLoading(t, func() bool { return s.Snapshot().State == "starting" })
	s.EmergencyStop(errCritical)
	if err := <-done; !errors.Is(err, errCritical) {
		t.Errorf("the waiting request: %v", err)
	}
	time.Sleep(2 * time.Second)
	if id := s.Running(); id != "" {
		t.Errorf("%s came up after the emergency", id)
	}
}

// Shutdown does not sit out a startup budget of minutes.
func TestCloseCancelsAStart(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "60000")
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	done := acquireLater(m, modelA)
	waitLoading(t, func() bool { return m.Loading(modelA) })
	began := time.Now()
	m.Close()
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("Close took %v during a start", d)
	}
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Errorf("the waiting request: %v", err)
	}
}

// Shutdown does not sit out a download either: the install gets a context that ends.
func TestCloseCancelsAnInstall(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	installing := make(chan struct{})
	m.d.Install = func(ctx context.Context, _ string) error {
		close(installing)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			return errors.New("the install was never cancelled")
		}
	}
	m.d.Catalog.ByID(modelA).Artifacts[0].Filename += ".missing"
	done := acquireLater(m, modelA)
	select {
	case <-installing:
	case <-time.After(5 * time.Second):
		t.Fatal("no install began")
	}
	began := time.Now()
	m.Close()
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("Close took %v during an install", d)
	}
	<-done
}

// The pinned runtime download is part of a start, so an ended switch ends it too.
func TestServiceBinaryDownloadHonorsTheSwitch(t *testing.T) {
	s, _ := newTestService(t)
	s.d.Binaries = nil
	s.d.RuntimesDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tgt, err := s.d.Gate("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.binary(ctx, "crispasr", tgt); !errors.Is(err, context.Canceled) {
		t.Errorf("want the cancel: %v", err)
	}
}

// Ready just as the switch was ended: install is the last check, and it stops the
// backend rather than making it current.
func TestInstallRefusesAnEndedSwitch(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	if err := <-acquireLater(m, modelA); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	b := m.cur
	m.cur = nil
	m.mu.Unlock()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errCritical)
	if err := m.install(ctx, b); !errors.Is(err, errCritical) {
		t.Errorf("install: %v", err)
	}
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("%s became current", id)
	}
	select {
	case <-b.proc.Done():
	case <-time.After(5 * time.Second):
		t.Error("the backend is still running")
	}
}
