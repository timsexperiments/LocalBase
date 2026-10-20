// Package obs writes LocalBase's structured event log: one JSON object per
// line in <root>/logs/events.jsonl, the same schema (version 2) the TS
// gateway writes, so `local-base logs` from either binary reads both.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	rotateBytes = 10 << 20
	archives    = 5
)

// Sink is a rotating, private JSONL file. Safe for concurrent use.
type Sink struct {
	mu   sync.Mutex
	dir  string
	f    *os.File
	size int64
	// Also writes each line here when set (foreground serve's stderr).
	Mirror io.Writer
}

func OpenSink(dir string) (*Sink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Sink{dir: dir}
	return s, s.open()
}

func (s *Sink) path() string { return filepath.Join(s.dir, "events.jsonl") }

func (s *Sink) open() error {
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, _ := f.Stat()
	s.f, s.size = f, info.Size()
	return nil
}

func (s *Sink) rotate() error {
	s.f.Close()
	for i := archives - 1; i >= 1; i-- {
		os.Rename(filepath.Join(s.dir, fmt.Sprintf("events.%d.jsonl", i)), filepath.Join(s.dir, fmt.Sprintf("events.%d.jsonl", i+1)))
	}
	os.Rename(s.path(), filepath.Join(s.dir, "events.1.jsonl"))
	return s.open()
}

func (s *Sink) Write(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size+int64(len(line)) > rotateBytes {
		if err := s.rotate(); err != nil {
			return
		}
	}
	n, _ := s.f.Write(line)
	s.size += int64(n)
	if s.Mirror != nil {
		s.Mirror.Write(line)
	}
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// Handler is a slog.Handler emitting schema-2 events. Well-known attrs map to
// top-level fields: event, category, component, runtime, request_id, http
// (a group); everything else goes to `attributes`, redacted by key.
type Handler struct {
	sink  *Sink
	level slog.Level
	attrs []slog.Attr
}

func NewHandler(sink *Sink, level slog.Level) *Handler { return &Handler{sink: sink, level: level} }

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{sink: h.sink, level: h.level, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *Handler) WithGroup(string) slog.Handler { return h }

// Redact replaces values whose keys name credentials or request content.
func Redact(key string, v any) any {
	if _, val, ok := attribute(key, v); ok {
		return val
	}
	return redacted
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b)
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	ev := map[string]any{
		"schemaVersion": 2, "id": uuid(), "timestamp": r.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
		"severity": severity(r.Level), "eventName": "localbase.event", "category": "", "component": "gateway",
		"runtime": "gateway",
	}
	attributes := map[string]any{}
	addAttr := func(key string, v any) {
		if len(attributes) >= maxAttributes {
			return
		}
		if k, val, ok := attribute(key, v); ok {
			attributes[k] = val
		}
	}
	var visit func(a slog.Attr)
	visit = func(a slog.Attr) {
		v := a.Value.Resolve()
		switch a.Key {
		case "event":
			ev["eventName"] = eventName(v.String())
		case "category":
			if slices.Contains(categories, v.String()) {
				ev["category"] = v.String()
			}
		case "runtime":
			if slices.Contains(runtimes, v.String()) {
				ev["runtime"] = v.String()
			}
		case "component":
			ev["component"] = componentName(v.String())
		case "request_id":
			if id := safeRequestID(v.String()); id != "" {
				ev["requestId"] = id
			}
		case "error":
			ev["error"] = map[string]any{"type": "Error", "message": BoundedText(v.String(), maxMessage)}
		case "http":
			m := map[string]any{}
			for _, g := range v.Group() {
				m[g.Key] = g.Value.Resolve().Any()
			}
			if p, ok := m["path"].(string); ok {
				route, page := canonicalRoute(p)
				m["path"] = route
				if page != "" {
					addAttr("ui_route", page)
				}
			}
			ev["http"] = m
		default:
			if v.Kind() == slog.KindGroup {
				for _, g := range v.Group() {
					visit(g)
				}
				return
			}
			addAttr(a.Key, v.Any())
		}
	}
	for _, a := range h.attrs {
		visit(a)
	}
	r.Attrs(func(a slog.Attr) bool { visit(a); return true })
	if ev["category"] == "" {
		ev["category"] = categoryFor(ev["eventName"].(string))
	}
	msg := BoundedText(r.Message, maxMessage)
	if msg == "" {
		msg = "Unknown error"
	}
	ev["message"] = msg
	if len(attributes) > 0 {
		ev["attributes"] = attributes
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	h.sink.Write(append(line, '\n'))
	return nil
}
