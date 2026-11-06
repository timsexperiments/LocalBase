package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Job is an import running in the gateway. Its fields never contain header
// values, tokens or signed URLs.
type Job struct {
	ID        string              `json:"id"`
	State     string              `json:"state"` // running | done | failed | cancelled
	Progress  acquire.Progress    `json:"progress"`
	Error     string              `json:"error,omitempty"`
	Detail    string              `json:"detail,omitempty"`    // what happened, when it wasn't a failure
	ErrorCode string              `json:"errorCode,omitempty"` // a stable code when there is one
	Model     *models.Model       `json:"model,omitempty"`
	Selection *backends.Selection `json:"selection,omitempty"`
	Started   time.Time           `json:"started"`
	Plan      *acquire.Plan       `json:"plan"`

	cancel    context.CancelFunc
	cancelled bool // by a person, not by a shutdown
}

type jobs struct {
	mu   sync.Mutex
	byID map[string]*Job
	wg   sync.WaitGroup
}

const (
	// maxActiveImports bounds downloads running at once.
	maxActiveImports = 4
	// keptFinishedImports is how many finished jobs stay readable.
	keptFinishedImports = 64
)

// Why an import did not start.
var (
	ErrImportsBusy   = errors.New("Too many imports are running. Wait for one to finish, or cancel one.")
	ErrImportRunning = errors.New("This model is already being imported.")
)

// How an import takes the root, and how long it waits between tries (tests change them).
var (
	withLease       = root.WithOperation
	leaseRetryEvery = time.Second
	// publishHook marks the steps of publishing, for a test to end the process there.
	publishHook = func(string) {}
)

// underLease runs fn holding the root, for as long as ctx allows. By this point a
// download may have taken hours, and something else holding the root for a while is no
// reason to fail it.
func (l *Library) underLease(ctx context.Context, fn func() error) error {
	for {
		err := withLease(l.Root.Dir, "install model", fn)
		if !errors.Is(err, root.ErrOperationBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(leaseRetryEvery):
		}
	}
}

// reclaim removes files under an imported model's directory that nothing records. A
// process killed between moving the files into place and recording them leaves those:
// no list shows them, so without this they would only block importing that id again.
// They are never reused: another source may be importing the id now. Hold the root.
func (l *Library) reclaim(ctx context.Context, id string) error {
	dest := acquire.ImportedDir(l.Root.Dir, id)
	if _, err := os.Stat(dest); err != nil {
		return nil
	}
	recorded, err := l.Store.ImportedModels(ctx)
	if err != nil {
		return err
	}
	for _, m := range recorded {
		if m.ID == id {
			return nil // a real model: Fetch and Publish refuse to replace it
		}
	}
	return os.RemoveAll(dest)
}

// PrepareImport clears what an earlier crash left of this import. Call it before Fetch.
func (l *Library) PrepareImport(ctx context.Context, plan *acquire.Plan) error {
	if _, err := os.Stat(acquire.ImportedDir(l.Root.Dir, plan.ID)); err != nil {
		return nil
	}
	return l.underLease(ctx, func() error { return l.reclaim(ctx, plan.ID) })
}

// PublishImport moves a fetched import into place and records it, holding the root
// across both, so a model is listed only once its files are all there. A kill between
// the two leaves files nothing lists, which the next try removes (reclaim).
func (l *Library) PublishImport(ctx context.Context, fetched *acquire.Fetched, plan *acquire.Plan, backend string) (*acquire.Result, backends.Selection, error) {
	var res *acquire.Result
	var sel backends.Selection
	err := l.underLease(ctx, func() error {
		if err := l.reclaim(ctx, plan.ID); err != nil {
			return err
		}
		publishHook("before-publish")
		var err error
		if res, err = fetched.Publish(); err != nil {
			return err
		}
		publishHook("after-publish")
		// Once the files are in place, finish: a cancel from here on would leave them.
		if sel, err = l.SaveImport(context.WithoutCancel(ctx), res, backend); err != nil {
			return err
		}
		publishHook("after-record")
		return nil
	})
	return res, sel, err
}

// ImportLimits gives a plan the limits this root sets, and says how long the import may
// run. A value that can't be read counts as unset, so the defaults hold.
func (l *Library) ImportLimits(ctx context.Context, plan *acquire.Plan) time.Duration {
	if n, ok := l.Store.SettingInt(ctx, store.ImportMaxUnknownKey); ok {
		plan.MaxUnknownBytes = int64(n) << 30
	}
	if n, ok := l.Store.SettingInt(ctx, store.ImportDeadlineKey); ok {
		return time.Duration(n) * time.Hour
	}
	return acquire.ImportDeadline
}

// StartImport runs an import in the background. The download holds nothing a
// configuration change or another import needs, since it can take hours: only
// publishing takes the root's operation lease.
func (l *Library) StartImport(plan *acquire.Plan, backend string, log *slog.Logger) (string, error) {
	job := &Job{ID: "imp_" + root.NewID(), State: "running", Started: time.Now(), Plan: plan}
	ctx, cancel := context.WithTimeout(context.Background(), l.ImportLimits(context.Background(), plan))
	job.cancel = cancel
	l.jobs.mu.Lock()
	if l.jobs.byID == nil {
		l.jobs.byID = map[string]*Job{}
	}
	running := 0
	for _, j := range l.jobs.byID {
		if j.State != "running" {
			continue
		}
		running++
		if j.Plan.ID == plan.ID {
			l.jobs.mu.Unlock()
			cancel()
			return "", ErrImportRunning
		}
	}
	if running >= maxActiveImports {
		l.jobs.mu.Unlock()
		cancel()
		return "", ErrImportsBusy
	}
	l.jobs.byID[job.ID] = job
	l.jobs.trim()
	l.jobs.wg.Add(1)
	l.jobs.mu.Unlock()
	go func() {
		defer l.jobs.wg.Done()
		defer cancel()
		var model *models.Model
		var sel backends.Selection
		var fetched *acquire.Fetched
		err := l.PrepareImport(ctx, plan)
		if err == nil {
			fetched, err = acquire.Fetch(ctx, l.Root.Dir, plan, func(p acquire.Progress) {
				l.jobs.mu.Lock()
				if p.Note == "" {
					p.Note = job.Progress.Note // it's said once, and stays on the page
				}
				job.Progress = p
				l.jobs.mu.Unlock()
			})
		}
		if err == nil {
			var res *acquire.Result
			if res, sel, err = l.PublishImport(ctx, fetched, plan, backend); err == nil {
				model = &res.Model
			}
		}
		l.jobs.mu.Lock()
		cancelled := job.cancelled
		l.jobs.mu.Unlock()
		var leftBehind error
		if err != nil && cancelled {
			leftBehind = acquire.RemoveStaging(l.Root.Dir, plan)
		}
		l.jobs.mu.Lock()
		defer l.jobs.mu.Unlock()
		if err != nil && cancelled {
			// A person chose this, so it isn't a failure.
			job.State, job.ErrorCode, job.Detail = "cancelled", acquire.CodeCancelled, "The import was cancelled, and what it downloaded was removed."
			if leftBehind != nil {
				// PruneStaging clears it once nothing has touched it for a while.
				job.Detail = "The import was cancelled, but what it downloaded could not be removed yet. LocalBase will clear it later."
			}
			if log != nil {
				log.Info("Model import cancelled.", "event", "model.import-cancelled")
				if leftBehind != nil {
					log.Warn("A cancelled import's download could not be removed.", "event", "model.import-cleanup-failed", "error", obs.BoundedText(leftBehind.Error(), 1024))
				}
			}
			return
		}
		if err != nil {
			job.State, job.Error = "failed", obs.BoundedText(err.Error(), 1024)
			var refused *acquire.RefusedError
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				job.Error, job.ErrorCode = "The import took too long and was stopped. Try again to resume it. Whoever runs this LocalBase can allow longer.", acquire.CodeTimedOut
			case errors.As(err, &refused):
				job.ErrorCode = refused.Code
			}
			if log != nil {
				log.Warn("Model import failed.", "event", "model.import-failed", "error", job.Error)
			}
			return
		}
		job.State, job.Model, job.Selection = "done", model, &sel
		if log != nil {
			log.Info("Imported model "+model.ID+".", "event", "model.imported", "model_id", model.ID)
		}
	}()
	return job.ID, nil
}

// trim forgets the oldest finished jobs. Call it holding the lock.
func (j *jobs) trim() {
	for len(j.byID) > keptFinishedImports {
		var oldest *Job
		for _, job := range j.byID {
			if job.State != "running" && (oldest == nil || job.Started.Before(oldest.Started)) {
				oldest = job
			}
		}
		if oldest == nil {
			return
		}
		delete(j.byID, oldest.ID)
	}
}

// CancelImport stops a running import and discards what it downloaded. It reports
// whether the job exists; a finished job stays as it ended.
func (l *Library) CancelImport(id string) bool {
	l.jobs.mu.Lock()
	defer l.jobs.mu.Unlock()
	job := l.jobs.byID[id]
	if job == nil {
		return false
	}
	if job.State == "running" {
		job.cancelled = true
		job.cancel()
	}
	return true
}

// StopImports ends running imports for a shutdown, within grace. What they
// downloaded stays, so the same import resumes after a restart.
func (l *Library) StopImports(grace time.Duration) {
	l.jobs.mu.Lock()
	for _, job := range l.jobs.byID {
		if job.State == "running" {
			job.cancel()
		}
	}
	l.jobs.mu.Unlock()
	done := make(chan struct{})
	go func() { l.jobs.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// Job returns a snapshot of a job, or nil.
func (l *Library) Job(id string) *Job {
	l.jobs.mu.Lock()
	defer l.jobs.mu.Unlock()
	j := l.jobs.byID[id]
	if j == nil {
		return nil
	}
	c := *j
	return &c
}
