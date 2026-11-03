package library

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// Job is an import running in the gateway. Its fields never contain header
// values, tokens or signed URLs.
type Job struct {
	ID        string              `json:"id"`
	State     string              `json:"state"` // running | done | failed
	Progress  acquire.Progress    `json:"progress"`
	Error     string              `json:"error,omitempty"`
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

// StartImport runs an import in the background. The download holds nothing a
// configuration change or another import needs, since it can take hours: only
// publishing takes the root's operation lease.
func (l *Library) StartImport(plan *acquire.Plan, backend string, log *slog.Logger) (string, error) {
	job := &Job{ID: "imp_" + root.NewID(), State: "running", Started: time.Now(), Plan: plan}
	ctx, cancel := context.WithTimeout(context.Background(), acquire.ImportDeadline)
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
		fetched, err := acquire.Fetch(ctx, l.Root.Dir, plan, func(p acquire.Progress) {
			l.jobs.mu.Lock()
			job.Progress = p
			l.jobs.mu.Unlock()
		})
		if err == nil {
			err = root.WithOperation(l.Root.Dir, "install model", func() error {
				res, err := fetched.Publish()
				if err != nil {
					return err
				}
				if sel, err = l.SaveImport(context.WithoutCancel(ctx), res, backend); err != nil {
					return err
				}
				model = &res.Model
				return nil
			})
		}
		l.jobs.mu.Lock()
		cancelled := job.cancelled
		l.jobs.mu.Unlock()
		if err != nil && cancelled {
			acquire.RemoveStaging(l.Root.Dir, plan)
		}
		l.jobs.mu.Lock()
		defer l.jobs.mu.Unlock()
		if err != nil {
			job.State, job.Error = "failed", obs.BoundedText(err.Error(), 1024)
			var refused *acquire.RefusedError
			switch {
			case cancelled:
				job.Error, job.ErrorCode = "The import was cancelled.", acquire.CodeCancelled
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				job.Error, job.ErrorCode = "The import took too long and was stopped. Try again to resume it.", acquire.CodeTimedOut
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
