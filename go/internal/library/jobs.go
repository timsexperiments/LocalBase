package library

import (
	"context"
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
	Model     *models.Model       `json:"model,omitempty"`
	Selection *backends.Selection `json:"selection,omitempty"`
	Started   time.Time           `json:"started"`
	Plan      *acquire.Plan       `json:"plan"`
}

type jobs struct {
	mu   sync.Mutex
	byID map[string]*Job
}

// StartImport runs an import in the background, holding the root operation
// lease, and returns its job id.
func (l *Library) StartImport(plan *acquire.Plan, backend string, log *slog.Logger) string {
	job := &Job{ID: "imp_" + root.NewID(), State: "running", Started: time.Now(), Plan: plan}
	l.jobs.mu.Lock()
	if l.jobs.byID == nil {
		l.jobs.byID = map[string]*Job{}
	}
	l.jobs.byID[job.ID] = job
	l.jobs.mu.Unlock()
	go func() {
		ctx := context.Background()
		var model *models.Model
		var sel backends.Selection
		err := root.WithOperation(l.Root.Dir, "install model", func() error {
			res, err := acquire.Import(ctx, l.Root.Dir, plan, func(p acquire.Progress) {
				l.jobs.mu.Lock()
				job.Progress = p
				l.jobs.mu.Unlock()
			})
			if err != nil {
				return err
			}
			if sel, err = l.SaveImport(ctx, res, backend); err != nil {
				return err
			}
			model = &res.Model
			return nil
		})
		l.jobs.mu.Lock()
		defer l.jobs.mu.Unlock()
		if err != nil {
			job.State, job.Error = "failed", obs.BoundedText(err.Error(), 1024)
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
	return job.ID
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
