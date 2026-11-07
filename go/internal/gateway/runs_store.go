package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// --- checkpoints ------------------------------------------------------------------

func (m *runManager) path(id string) string { return filepath.Join(m.dir, id+".json") }

// checkpoint writes the run's state, output included, for a restart to
// report (DESIGN §21.6). Called with r.mu held.
func (m *runManager) checkpoint(r *run) {
	if m.dir == "" {
		return
	}
	rec := r.runRecord
	out := r.output
	rec.Output = &out
	if !r.restored {
		rec.Events = len(r.events)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if os.MkdirAll(m.dir, 0o700) != nil {
		return
	}
	obs.WarnRefusedWrite(m.opts.log(), obs.WritePrivateAtomic(m.path(r.ID), b))
	r.saved = m.now()
}

func (m *runManager) remove(id string) {
	if m.dir != "" {
		os.Remove(m.path(id))
	}
}

// sweepStart reads the checkpoints a previous gateway left: unfinished runs
// become interrupted (gateway_restart), expired and unreadable files go.
func (m *runManager) sweepStart() {
	if m.dir == "" {
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	now := m.now()
	for _, e := range entries {
		p := filepath.Join(m.dir, e.Name())
		id, ok := strings.CutSuffix(e.Name(), ".json")
		b, err := obs.ReadOwnedRegular(p)
		var rec runRecord
		if !ok || !strings.HasPrefix(id, "run_") || err != nil || json.Unmarshal(b, &rec) != nil || rec.ID != id || rec.Owner == "" {
			os.Remove(p)
			continue
		}
		r := &run{m: m, runRecord: rec, restored: true, notify: make(chan struct{}), done: make(chan struct{})}
		close(r.done)
		if rec.Output != nil {
			r.output = *rec.Output
		}
		r.Output = nil
		if r.terminal() && (r.ExpiresAt == nil || !now.Before(*r.ExpiresAt)) {
			os.Remove(p)
			continue
		}
		r.held = int64(len(b)) // Its output is back in memory.
		m.held.Add(r.held)
		if !r.terminal() || (r.Target != chatTarget && r.Status == runCompleted) {
			// Unfinished, or a media result that lived in memory only and is
			// gone: either way the run was interrupted by the restart.
			r.Status, r.Reason = runInterrupted, "gateway_restart"
			r.EndedAt = &now
			exp := now.Add(m.retention)
			r.ExpiresAt = &exp
			m.checkpoint(r)
			m.log(r, "run.interrupted")
		}
		if r.ExpiresAt == nil || !now.Before(*r.ExpiresAt) {
			os.Remove(p)
			continue
		}
		m.runs[id] = r
	}
}

// sweepExpired deletes finished runs past their retention.
func (m *runManager) sweepExpired() {
	now := m.now()
	m.mu.Lock()
	var gone []*run
	for id, r := range m.runs {
		r.mu.Lock()
		if r.terminal() && r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
			delete(m.runs, id)
			gone = append(gone, r)
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	for _, r := range gone {
		m.release(r)
		m.remove(r.ID)
		m.log(r, "run.expired")
	}
}

func (m *runManager) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.sweepExpired()
		case <-m.quit:
			return
		}
	}
}
