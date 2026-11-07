package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// --- telemetry ---------------------------------------------------------------------

// log records a lifecycle event: ids and outcomes, never content.
func (m *Manager) log(r *run, event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	attrs := []any{"event", event, "category", "run", "run_id", r.ID, "target", r.Target, "model_id", r.Model,
		"owner", r.Owner, "status", r.Status}
	if r.RequestID != "" {
		attrs = append(attrs, "request_id", r.RequestID)
	}
	if r.Reason != "" {
		attrs = append(attrs, "reason", r.Reason)
	}
	if code, ok := r.Error["code"].(string); ok && code != "" {
		attrs = append(attrs, "error_code", code)
	}
	if r.EndedAt != nil {
		attrs = append(attrs, "duration_ms", Round2(Milliseconds(r.EndedAt.Sub(r.CreatedAt))), "output_bytes", r.bytes+len(r.result), "events", len(r.events))
	}
	level := slog.LevelInfo
	switch event {
	case "run.failed":
		level = slog.LevelWarn
	case "run.interrupted":
		level = slog.LevelWarn
	}
	msg := map[string]string{"run.started": "Run started.", "run.completed": "Run completed.", "run.failed": "Run failed.",
		"run.cancelled": "Run stopped.", "run.interrupted": "Run interrupted.", "run.expired": "Run expired.",
		"run.dismissed": "Run dismissed."}[event]
	m.opts.Log().Log(context.Background(), level, msg, attrs...)
}

// --- events ------------------------------------------------------------------------

// emit appends an event and wakes the readers. Called with r.mu held.
func (r *run) emit(name string, data []byte) {
	r.events = append(r.events, runEvent{id: len(r.events) + 1, name: name, data: data})
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *run) setStatus(status, reason string) {
	r.Status, r.Reason = status, reason
	b, _ := json.Marshal(map[string]any{"status": status, "reason": reason})
	if reason == "" {
		b, _ = json.Marshal(map[string]any{"status": status})
	}
	r.emit("status", b)
}

// view is the public run object. Called with r.mu held.
func (r *run) view(withOutput bool) map[string]any {
	v := map[string]any{"id": r.ID, "object": "localbase.run", "target": r.Target, "label": r.Label, "model": r.Model,
		"status": r.Status, "error": r.Error, "createdAt": r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"startedAt": stamp(r.StartedAt), "endedAt": stamp(r.EndedAt), "expiresAt": stamp(r.ExpiresAt),
		"lastEventId": r.lastEventID(), "resumable": false}
	if r.Reason != "" {
		v["reason"] = r.Reason
	}
	if r.RequestID != "" {
		v["requestId"] = r.RequestID
	}
	if r.Status == runInterrupted {
		v["continuation"] = map[string]any{"by": "client", "message": "LocalBase does not keep the request, so it cannot " +
			"resume this run. To continue, send the conversation again with the partial reply."}
	}
	if withOutput && r.Target == chatTarget {
		v["output"] = r.output.snapshot()
	}
	if r.result != nil {
		res := map[string]any{"contentType": r.resultType, "bytes": len(r.result), "url": "/_localbase/runs/" + r.ID + "/result"}
		if len(r.notes) > 0 {
			res["notes"] = r.notes
		}
		v["result"] = res
	}
	return v
}

func (r *run) lastEventID() int {
	if r.restored {
		return r.Events + 2 // snapshot, end
	}
	return len(r.events)
}

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
