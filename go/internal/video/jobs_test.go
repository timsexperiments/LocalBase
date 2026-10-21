package video

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// scriptBackend completes each job with media of the requested size after
// one poll, unless the prompt asks it to keep generating.
type scriptBackend struct {
	mu   sync.Mutex
	n    int
	size map[string]int
	slow map[string]bool
}

func (b *scriptBackend) Submit(_ context.Context, in map[string]any) (BackendJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	id := "job" + string(rune('a'+b.n))
	if b.size == nil {
		b.size, b.slow = map[string]int{}, map[string]bool{}
	}
	size, _ := in["size"].(int)
	b.size[id], b.slow[id] = size, in["prompt"] == "slow"
	return BackendJob{ID: id, Status: "queued"}, nil
}

func (b *scriptBackend) Get(_ context.Context, id string) (BackendJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.slow[id] {
		return BackendJob{ID: id, Status: "generating"}, nil
	}
	data := make([]byte, b.size[id])
	return BackendJob{ID: id, Status: "completed", Media: &Media{Data: data, MimeType: "video/x-msvideo", Format: "avi", FPS: 16, Frames: 33}}, nil
}

type noopAdmission struct{ released *int }

func (a noopAdmission) Ready(context.Context) error { return nil }
func (a noopAdmission) Release()                    { *a.released++ }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestManager(t *testing.T, o Options) (*Manager, *int) {
	t.Helper()
	o.TempDir = t.TempDir()
	if o.Backend == nil {
		o.Backend = &scriptBackend{}
	}
	o.PollInterval = 5 * time.Millisecond
	m, err := NewManager(o)
	if err != nil {
		t.Fatal(err)
	}
	released := 0
	return m, &released
}

func start(t *testing.T, m *Manager, released *int, owner, prompt string, size int, deadline time.Duration) (Job, <-chan Job) {
	t.Helper()
	acquire := func(context.Context) (Admission, error) { return noopAdmission{released}, nil }
	j, done, err := m.Start(owner, map[string]any{"prompt": prompt, "size": size}, deadline, acquire, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return j, done
}

func wait(t *testing.T, done <-chan Job) Job {
	t.Helper()
	select {
	case j := <-done:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	return Job{}
}

func TestJobOwnersAndRetention(t *testing.T) {
	m, released := newTestManager(t, Options{})
	j, done := start(t, m, released, "api-key:a", "ok", 100, time.Minute)
	if got := wait(t, done); got.State != Completed || got.Artifact.Bytes != 100 {
		t.Fatalf("job %+v", got)
	}
	if *released != 1 {
		t.Fatalf("admission released %d times", *released)
	}
	if _, ok := m.Get("api-key:b", j.ID); ok {
		t.Fatal("another owner sees the job")
	}
	if _, _, ok := m.Artifact("api-key:b", j.ID); ok {
		t.Fatal("another owner reads the artifact")
	}
	if m.Delete("api-key:b", j.ID) {
		t.Fatal("another owner deleted the job")
	}
	if path, a, ok := m.Artifact("api-key:a", j.ID); !ok || a.Bytes != 100 || path == "" {
		t.Fatal("owner cannot read the artifact")
	}
}

func TestJobDeadlineAndShutdown(t *testing.T) {
	m, released := newTestManager(t, Options{})
	_, done := start(t, m, released, "o", "slow", 1, 50*time.Millisecond)
	if got := wait(t, done); got.State != Cancelled || got.Reason != "deadline" {
		t.Fatalf("deadline %+v", got)
	}
	_, done = start(t, m, released, "o", "slow", 1, time.Minute)
	if _, _, err := m.Start("o", map[string]any{}, time.Minute, nil, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second job: %v", err)
	}
	m.Shutdown()
	if got := wait(t, done); got.State != Cancelled || got.Reason != "shutdown" {
		t.Fatalf("shutdown %+v", got)
	}
	if _, _, err := m.Start("o", map[string]any{}, time.Minute, nil, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("after shutdown: %v", err)
	}
}

func TestJobArtifactLimits(t *testing.T) {
	m, released := newTestManager(t, Options{MaxArtifactBytes: 1000, MaxArtifactBytesTotal: 2500, MaxTerminalJobs: 10})
	_, done := start(t, m, released, "o", "ok", 1001, time.Minute)
	if got := wait(t, done); got.State != Failed || !errors.Is(got.Failure, ErrArtifactLimit) {
		t.Fatalf("over the per-job limit %+v", got)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		j, done := start(t, m, released, "o", "ok", 1000, time.Minute)
		wait(t, done)
		ids = append(ids, j.ID)
	}
	// 3 x 1000 exceeds 2500: the oldest artifact was evicted for the third.
	if _, _, ok := m.Artifact("o", ids[0]); ok {
		t.Fatal("the oldest artifact was kept past the aggregate limit")
	}
	for _, id := range ids[1:] {
		if _, _, ok := m.Artifact("o", id); !ok {
			t.Fatalf("artifact %s evicted", id)
		}
	}
}

func TestJobTTL(t *testing.T) {
	c := &clock{now: time.Unix(1_760_000_000, 0)}
	m, released := newTestManager(t, Options{Now: c.Now})
	j, done := start(t, m, released, "o", "ok", 10, time.Minute)
	wait(t, done)
	c.add(15*time.Minute - time.Second)
	if _, ok := m.Get("o", j.ID); !ok {
		t.Fatal("pruned early")
	}
	c.add(time.Second)
	if _, ok := m.Get("o", j.ID); ok {
		t.Fatal("kept past the 15 minute TTL")
	}
}

func TestPressureFailsActiveJob(t *testing.T) {
	m, released := newTestManager(t, Options{})
	_, done := start(t, m, released, "o", "slow", 1, time.Minute)
	time.Sleep(30 * time.Millisecond)
	m.FailActiveForMemoryPressure()
	got := wait(t, done)
	var ae *memory.AdmissionError
	if got.State != Failed || !errors.As(got.Failure, &ae) || ae.Reason != "memory-pressure" {
		t.Fatalf("pressure %+v", got)
	}
}

// A supervised stop that fails leaves the job running; a later cancel
// retries it (TS contain resets the termination).
func TestFailedStopRetries(t *testing.T) {
	m, released := newTestManager(t, Options{})
	fail := true
	acquire := func(context.Context) (Admission, error) { return noopAdmission{released}, nil }
	j, done, err := m.Start("o", map[string]any{"prompt": "slow"}, time.Minute, acquire, func() error {
		if fail {
			return errors.New("still running")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, err := m.Cancel("o", j.ID); err == nil {
		t.Fatal("a failed stop was reported as success")
	}
	if got, _ := m.Get("o", j.ID); got.State == Cancelled {
		t.Fatal("cancelled while its runtime may still run")
	}
	fail = false
	if _, _, err := m.Cancel("o", j.ID); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, done); got.State != Cancelled {
		t.Fatalf("retry %+v", got)
	}
}
