// Package video runs local video generation jobs (TS video-job-manager.ts):
// one job at a time on the stable-diffusion.cpp video runtime, its private
// artifact files, and their bounded retention.
package video

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

const (
	defaultMaxArtifactBytes      = 64 << 20
	defaultMaxArtifactBytesTotal = 256 << 20
	defaultMaxTerminalJobs       = 20
	defaultTerminalTTL           = 15 * time.Minute
	defaultPollInterval          = time.Second
)

// State is a job's lifecycle state.
type State string

const (
	Queued     State = "queued"
	InProgress State = "in_progress"
	Completed  State = "completed"
	Failed     State = "failed"
	Cancelled  State = "cancelled"
)

// Artifact describes a completed job's retained file.
type Artifact struct {
	Bytes    int64
	MimeType string
	Format   string // mp4 | avi | webm | webp
	FPS      int
	Frames   int
}

// Job is a snapshot of a job.
type Job struct {
	ID         string
	State      State
	CreatedAt  time.Time
	TerminalAt time.Time
	Artifact   Artifact // completed
	Failure    error    // failed
	Reason     string   // cancelled: backend | cancelled | deadline | shutdown
}

// Media is a generated video from the backend, or prepared for retention.
type Media struct {
	Data     []byte
	MimeType string
	Format   string
	FPS      int
	Frames   int
}

// BackendJob is the backend's view of a job: queued, generating, completed
// (with Media), failed or cancelled.
type BackendJob struct {
	ID     string
	Status string
	Media  *Media
}

// Backend is the stable-diffusion.cpp video API.
type Backend interface {
	Submit(ctx context.Context, input map[string]any) (BackendJob, error)
	Get(ctx context.Context, id string) (BackendJob, error)
}

// Admission holds the video runtime for a job until it is released.
type Admission interface {
	// Ready waits for the runtime to start.
	Ready(ctx context.Context) error
	Release()
}

// Acquire admits the video runtime for a job; nil without an error means
// it is unavailable.
type Acquire func(ctx context.Context) (Admission, error)

// ErrArtifactLimit: an artifact exceeds the per-job or aggregate limit.
var ErrArtifactLimit = errors.New("video artifact exceeds its limit")

// ErrBackendFailed: the backend reported the job failed.
var ErrBackendFailed = errors.New("Video backend job failed.")

// ErrBusy: a job is already running, or the manager is stopping.
var ErrBusy = errors.New("A local video job is already in progress.")

// Options configure a Manager. Zero limits take the TS defaults.
type Options struct {
	Backend Backend
	// TempDir holds video-jobs/<id>/ (removed when the manager starts).
	TempDir string
	// Prepare converts a backend artifact before retention (the AVI to MP4
	// converter); nil keeps it as generated.
	Prepare func(ctx context.Context, media Media, dir string, maxBytes int64) (Media, error)
	// OnContainmentFailure reports a job that could not be stopped cleanly.
	OnContainmentFailure func(jobID, source string, err error)

	MaxArtifactBytes, MaxArtifactBytesTotal int64
	MaxTerminalJobs                         int
	TerminalTTL, PollInterval               time.Duration
	Now                                     func() time.Time
}

// Manager owns the jobs.
type Manager struct {
	o    Options
	root string

	mu       sync.Mutex
	jobs     map[string]*job
	active   *job
	stopping bool
}

type job struct {
	id, owner string
	created   time.Time
	dir       string
	stop      func() error // stops the runtime under the job (supervised stop)

	ctx    context.Context
	cancel context.CancelFunc

	// guarded by Manager.mu
	state       State
	terminalAt  time.Time
	artifact    *Artifact
	pending     int64 // artifact bytes being written
	failure     error
	reason      string
	backendID   string
	admission   Admission
	terminating chan struct{} // closed when a termination finished; nil if none
	completing  chan struct{} // an artifact being persisted

	admitted chan struct{} // closed once admission settled
	terminal chan Job
	deadline *time.Timer
}

// NewManager removes leftovers from an earlier run and returns a manager.
func NewManager(o Options) (*Manager, error) {
	if o.MaxArtifactBytes <= 0 {
		o.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if o.MaxArtifactBytesTotal <= 0 {
		o.MaxArtifactBytesTotal = defaultMaxArtifactBytesTotal
	}
	if o.MaxTerminalJobs <= 0 {
		o.MaxTerminalJobs = defaultMaxTerminalJobs
	}
	if o.TerminalTTL <= 0 {
		o.TerminalTTL = defaultTerminalTTL
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Manager{o: o, root: filepath.Join(o.TempDir, "video-jobs"), jobs: map[string]*job{}}
	if err := os.RemoveAll(m.root); err != nil {
		return nil, err
	}
	return m, nil
}

// Start accepts a job and runs it. The returned channel receives the job's
// terminal snapshot. ErrBusy when a job is active.
func (m *Manager) Start(owner string, input map[string]any, deadline time.Duration, acquire Acquire, stop func() error) (Job, <-chan Job, error) {
	m.mu.Lock()
	m.prune()
	if m.stopping || m.active != nil {
		m.mu.Unlock()
		return Job{}, nil, ErrBusy
	}
	j, err := m.create(owner, stop)
	if err != nil {
		m.mu.Unlock()
		return Job{}, nil, err
	}
	m.active = j
	m.jobs[j.id] = j
	snap := m.snapshot(j)
	m.mu.Unlock()
	j.deadline = time.AfterFunc(deadline, func() { m.expire(j) })
	go m.run(j, input, acquire)
	return snap, j.terminal, nil
}

// Get returns the owner's job.
func (m *Manager) Get(owner, id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	j := m.jobs[id]
	if j == nil || j.owner != owner {
		return Job{}, false
	}
	return m.snapshot(j), true
}

// Artifact returns the path and metadata of the owner's completed job.
func (m *Manager) Artifact(owner, id string) (string, Artifact, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	j := m.jobs[id]
	if j == nil || j.owner != owner || j.artifact == nil {
		return "", Artifact{}, false
	}
	return filepath.Join(j.dir, "artifact"), *j.artifact, true
}

// Cancel stops the owner's job and returns its snapshot.
func (m *Manager) Cancel(owner, id string) (Job, bool, error) {
	m.mu.Lock()
	m.prune()
	j := m.jobs[id]
	if j == nil || j.owner != owner {
		m.mu.Unlock()
		return Job{}, false, nil
	}
	m.mu.Unlock()
	err := m.terminate(j, "cancelled", nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot(j), true, err
}

// Delete removes the owner's finished job and its files.
func (m *Manager) Delete(owner, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	j := m.jobs[id]
	if j == nil || j.owner != owner || j.terminalAt.IsZero() {
		return false
	}
	m.remove(j)
	return true
}

// Shutdown refuses new jobs and stops the active one.
func (m *Manager) Shutdown() error {
	m.mu.Lock()
	m.prune()
	m.stopping = true
	a := m.active
	m.mu.Unlock()
	if a == nil {
		return nil
	}
	return m.terminate(a, "shutdown", nil)
}

// CancelActive stops the active job (its runtime is being replaced).
func (m *Manager) CancelActive() error {
	m.mu.Lock()
	m.prune()
	a := m.active
	m.mu.Unlock()
	if a == nil {
		return nil
	}
	return m.terminate(a, "cancelled", nil)
}

// FailActiveForMemoryPressure fails the active job with a memory-pressure
// refusal (TS failActiveForMemoryPressure).
func (m *Manager) FailActiveForMemoryPressure() error {
	m.mu.Lock()
	m.prune()
	a := m.active
	m.mu.Unlock()
	if a == nil {
		return nil
	}
	return m.terminate(a, "", &memory.AdmissionError{Reason: "memory-pressure", PoolID: "system"})
}

// --- lifecycle --------------------------------------------------------------------------

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (m *Manager) create(owner string, stop func() error) (*job, error) {
	id := newID()
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(m.root, 0o700)
	dir := filepath.Join(m.root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &job{id: id, owner: owner, created: m.o.Now(), dir: dir, stop: stop, ctx: ctx, cancel: cancel,
		state: Queued, admitted: make(chan struct{}), terminal: make(chan Job, 1)}, nil
}

// run admits the runtime, submits the job and polls it to the end.
func (m *Manager) run(j *job, input map[string]any, acquire Acquire) {
	err := func() error {
		adm, err := acquire(j.ctx)
		m.mu.Lock()
		j.admission = adm
		m.mu.Unlock()
		close(j.admitted)
		if err != nil {
			return err
		}
		if m.stopped(j) {
			return nil
		}
		if adm == nil {
			return errors.New("Video runtime admission is unavailable.")
		}
		if err := adm.Ready(j.ctx); err != nil {
			return err
		}
		if m.stopped(j) {
			return nil
		}
		sub, err := m.o.Backend.Submit(j.ctx, input)
		if err != nil {
			return err
		}
		m.mu.Lock()
		if !j.terminalAt.IsZero() || j.terminating != nil || j.ctx.Err() != nil {
			m.mu.Unlock()
			return nil
		}
		j.backendID = sub.ID
		j.state = Queued
		if sub.Status == "generating" {
			j.state = InProgress
		}
		m.mu.Unlock()
		for !m.stopped(j) {
			update, err := m.o.Backend.Get(j.ctx, sub.ID)
			if err != nil {
				return err
			}
			if err := m.apply(j, update); err != nil {
				return err
			}
			if m.stopped(j) {
				return nil
			}
			select {
			case <-j.ctx.Done():
				return nil
			case <-time.After(m.o.PollInterval):
			}
		}
		return nil
	}()
	if err == nil || j.ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	terminating := j.terminating != nil
	m.mu.Unlock()
	if terminating {
		return
	}
	if cerr := m.terminate(j, "", err); cerr != nil {
		m.report(j, "runner", cerr)
	}
}

// stopped reports whether the job finished or is being stopped.
func (m *Manager) stopped(j *job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !j.terminalAt.IsZero() || j.terminating != nil
}

func (m *Manager) apply(j *job, update BackendJob) error {
	m.mu.Lock()
	if !j.terminalAt.IsZero() || j.terminating != nil || j.ctx.Err() != nil {
		m.mu.Unlock()
		return nil
	}
	if update.ID != j.backendID {
		m.mu.Unlock()
		return errors.New("Video backend returned a mismatched job ID.")
	}
	switch update.Status {
	case "queued", "generating":
		j.state = Queued
		if update.Status == "generating" {
			j.state = InProgress
		}
		m.mu.Unlock()
		return nil
	case "completed":
		j.state = InProgress
		done := make(chan struct{})
		j.completing = done
		m.mu.Unlock()
		a, err := m.persist(j, update.Media)
		m.mu.Lock()
		j.completing = nil
		close(done)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		if j.terminalAt.IsZero() && j.terminating == nil {
			j.artifact = &a
			j.state = Completed
			m.finish(j)
		}
		m.mu.Unlock()
		return nil
	case "failed":
		j.failure, j.state = ErrBackendFailed, Failed
		m.finish(j)
	default:
		j.reason, j.state = "backend", Cancelled
		m.finish(j)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) expire(j *job) {
	m.mu.Lock()
	done := !j.terminalAt.IsZero() || j.terminating != nil
	m.mu.Unlock()
	if done {
		return
	}
	if err := m.terminate(j, "deadline", nil); err != nil {
		m.report(j, "deadline", err)
	}
}

// terminate stops a job: cancelled with reason, or failed with failure. The
// runtime is stopped (supervised stop) before files are removed; a stop that
// fails leaves the job running so it can be retried.
func (m *Manager) terminate(j *job, reason string, failure error) error {
	m.mu.Lock()
	if !j.terminalAt.IsZero() {
		m.mu.Unlock()
		return nil
	}
	if t := j.terminating; t != nil {
		m.mu.Unlock()
		<-t
		return nil
	}
	t := make(chan struct{})
	j.terminating = t
	m.mu.Unlock()
	defer close(t)

	j.cancel()
	<-j.admitted
	m.mu.Lock()
	completing := j.completing
	m.mu.Unlock()
	if err := j.stop(); err != nil {
		m.mu.Lock()
		j.terminating = nil
		m.mu.Unlock()
		return err
	}
	if completing != nil {
		<-completing
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !j.terminalAt.IsZero() {
		return nil
	}
	if err := os.Remove(filepath.Join(j.dir, "artifact")); err != nil && !os.IsNotExist(err) {
		j.failure, j.state = err, Failed
		m.finish(j)
		return nil
	}
	j.pending = 0
	if failure != nil {
		j.failure, j.state = failure, Failed
	} else {
		j.reason, j.state = reason, Cancelled
	}
	m.finish(j)
	return nil
}

func (m *Manager) report(j *job, source string, err error) {
	if m.o.OnContainmentFailure != nil {
		m.o.OnContainmentFailure(j.id, source, err)
	}
}

// persist writes the (prepared) artifact privately, within the limits.
func (m *Manager) persist(j *job, media *Media) (Artifact, error) {
	if media == nil {
		return Artifact{}, errors.New("Video backend returned no media.")
	}
	if int64(len(media.Data)) > m.o.MaxArtifactBytes {
		return Artifact{}, fmt.Errorf("%w: per job", ErrArtifactLimit)
	}
	prepared := *media
	if m.o.Prepare != nil {
		var err error
		if prepared, err = m.o.Prepare(j.ctx, *media, j.dir, m.o.MaxArtifactBytes); err != nil {
			return Artifact{}, err
		}
	}
	if err := j.ctx.Err(); err != nil {
		return Artifact{}, err
	}
	size := int64(len(prepared.Data))
	if size > m.o.MaxArtifactBytes {
		return Artifact{}, fmt.Errorf("%w: prepared, per job", ErrArtifactLimit)
	}
	m.mu.Lock()
	err := m.makeRoom(size)
	if err == nil {
		j.pending = size
	}
	m.mu.Unlock()
	if err != nil {
		return Artifact{}, err
	}
	path := filepath.Join(j.dir, "artifact")
	if err := os.WriteFile(path, prepared.Data, 0o600); err != nil {
		return Artifact{}, err
	}
	os.Chmod(path, 0o600)
	return Artifact{Bytes: size, MimeType: prepared.MimeType, Format: prepared.Format, FPS: prepared.FPS, Frames: prepared.Frames}, nil
}

// makeRoom evicts the oldest finished jobs for a new artifact. Called with mu.
func (m *Manager) makeRoom(size int64) error {
	if size > m.o.MaxArtifactBytesTotal {
		return fmt.Errorf("%w: aggregate", ErrArtifactLimit)
	}
	m.prune()
	for {
		terminal := m.terminalJobs()
		if len(terminal) < m.o.MaxTerminalJobs && m.artifactBytes()+size <= m.o.MaxArtifactBytesTotal {
			return nil
		}
		if len(terminal) == 0 {
			return fmt.Errorf("%w: aggregate", ErrArtifactLimit)
		}
		m.remove(terminal[0])
	}
}

// finish records the terminal time and hands the job back. Called with mu.
func (m *Manager) finish(j *job) {
	if !j.terminalAt.IsZero() {
		return
	}
	j.terminalAt = m.o.Now()
	if j.deadline != nil {
		j.deadline.Stop()
	}
	if j.admission != nil {
		j.admission.Release()
	}
	if m.active == j {
		m.active = nil
	}
	j.terminal <- m.snapshot(j)
	m.prune()
}

func (m *Manager) snapshot(j *job) Job {
	s := Job{ID: j.id, State: j.state, CreatedAt: j.created, TerminalAt: j.terminalAt, Failure: j.failure, Reason: j.reason}
	if j.artifact != nil {
		s.Artifact = *j.artifact
	}
	return s
}

// prune drops finished jobs past the TTL or beyond the count. Called with mu.
func (m *Manager) prune() {
	now := m.o.Now()
	for _, j := range m.terminalJobs() {
		if now.Sub(j.terminalAt) >= m.o.TerminalTTL {
			m.remove(j)
		}
	}
	for terminal := m.terminalJobs(); len(terminal) > m.o.MaxTerminalJobs; terminal = m.terminalJobs() {
		m.remove(terminal[0])
	}
}

func (m *Manager) terminalJobs() []*job {
	var out []*job
	for _, j := range m.jobs {
		if !j.terminalAt.IsZero() {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].terminalAt.Before(out[b].terminalAt) })
	return out
}

func (m *Manager) artifactBytes() int64 {
	var n int64
	for _, j := range m.terminalJobs() {
		if j.artifact != nil {
			n += j.artifact.Bytes
		} else {
			n += j.pending
		}
	}
	return n
}

func (m *Manager) remove(j *job) {
	os.RemoveAll(j.dir)
	delete(m.jobs, j.id)
}
