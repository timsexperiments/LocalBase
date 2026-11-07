// Package obs writes LocalBase's structured event log: one JSON object per
// line in <root>/logs/events.jsonl, the same schema (version 2) the TS
// gateway writes, so `local-base logs` from either binary reads both.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

const (
	rotateBytes = 10 << 20
	archives    = 5
	// What an audit log may do rather than lose events when it can't rotate: go this far past its
	// limit, then put this many bytes of events on stderr, per outage. Past both they are lost, loudly.
	auditOverrun = 4 << 20
)

// Sink is a rotating, private JSONL file. Safe for concurrent use.
type Sink struct {
	lastErr, auditOpenErr error // see AuditErr
	mu                    sync.Mutex
	dir                   string
	name                  string // the file is <name>.jsonl and its archives <name>.N.jsonl
	f                     *os.File
	size                  int64
	// Also writes each line here when set (foreground serve's stderr).
	Mirror io.Writer
	// audit holds audit.* events too, in a file nothing else writes, so a flood of other
	// events can't push them out. Opened when the first one arrives.
	audit *Sink

	write  func([]byte) (int, error)
	closed bool
	torn   bool // the file ends in part of a line
	// A log that can't be written has only this to say so: stderr, once per outage.
	complaints io.Writer
	outage     *LogOutage
	overrun    int64 // how far past the limit this log may go; 0 is not at all
	spilled    int64 // bytes of events put on stderr in this outage; the limit is per outage, not in total
}

// LogOutage is a log that can't rotate or can't be written, for doctor and status: its writer is
// the only one who knows, and the file's directory may be why it can't record that anywhere.
type LogOutage struct {
	Log       string `json:"log"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	Reason    string `json:"reason"`
	Since     string `json:"since"`
	Dropped   int    `json:"dropped"`
	OnStderr  int    `json:"onStderr,omitempty"`
}

// Describe is one line for a person.
func (o LogOutage) Describe(now time.Time) string {
	line := fmt.Sprintf("the %s log at %s (%d bytes) can't rotate or be written: %s, since %s", o.Log, o.Path, o.SizeBytes, o.Reason, o.Since)
	if since, err := time.Parse(time.RFC3339, o.Since); err == nil {
		line += fmt.Sprintf(" (%s ago)", now.Sub(since).Round(time.Second))
	}
	line += fmt.Sprintf(". %d events lost", o.Dropped)
	if o.OnStderr > 0 {
		line += fmt.Sprintf(", %d kept only in the service's stderr", o.OnStderr)
		if goruntime.GOOS == "darwin" {
			line += ", of which only the newest 1 MiB survives the next start"
		}
	}
	return line + "."
}

const auditPrefix = "audit."

func OpenSink(dir string) (*Sink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Sink{dir: dir, name: "events", complaints: os.Stderr}
	return s, s.open()
}

func (s *Sink) path() string { return filepath.Join(s.dir, s.name+".jsonl") }

func (s *Sink) archive(i int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s.%d.jsonl", s.name, i))
}

// rotatingPath is the name the active file passes through, so that a file that can't be moved fails
// before any archive has. Nothing reads it, and the next rotation moves along one a crash left behind.
func (s *Sink) rotatingPath() string { return filepath.Join(s.dir, s.name+".rotating.jsonl") }

func (s *Sink) open() error {
	f, err := safefile.OpenOwnedRegular(s.path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.f, s.size, s.write = f, info.Size(), f.Write
	return nil
}

// rotate makes the active file archive 1. A file that can't be moved stays as it was with the
// archives untouched, and the error tells the caller not to add to it, which keeps it under the
// limit however long that lasts.
func (s *Sink) rotate() error {
	rotating := s.rotatingPath()
	// A crash between the renames left one behind: it is the newest archive, so it goes first.
	if info, err := os.Lstat(rotating); err == nil && info.Mode().IsRegular() {
		s.pushArchives()
	}
	moved := true
	if err := os.Rename(s.path(), rotating); errors.Is(err, fs.ErrNotExist) {
		moved = false // Someone removed it: there is nothing to keep.
	} else if err != nil {
		return err
	}
	_ = s.f.Close() // Only ever written to, and each write was checked.
	s.f = nil
	if moved {
		s.pushArchives()
	}
	s.torn = false
	return s.open()
}

// pushArchives moves the archives along and makes the rotating file archive 1.
func (s *Sink) pushArchives() {
	for i := archives - 1; i >= 1; i-- {
		if err := os.Rename(s.archive(i), s.archive(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(s.complaints, "local-base: could not keep an old %s log: %v\n", s.name, reason(err))
		}
	}
	if err := os.Rename(s.rotatingPath(), s.archive(1)); err != nil {
		fmt.Fprintf(s.complaints, "local-base: could not keep the last %s log, the next rotation tries again and may replace it: %v\n", s.name, reason(err))
	}
}

// rotateOrFollow rotates, unless another process already has: then this file is its archive 1, and
// rotating again would push that along with almost nothing new.
func (s *Sink) rotateOrFollow() error {
	mine, errMine := s.f.Stat()
	active, errActive := os.Stat(s.path())
	if errMine != nil || errActive != nil || os.SameFile(mine, active) {
		return s.rotate()
	}
	_ = s.f.Close() // Only ever written to, and each write was checked.
	s.f, s.torn = nil, false
	return s.open()
}

// add puts one line in the file, and says whether it did and what is wrong: a log allowed an overrun
// can keep a line and still be unable to rotate.
func (s *Sink) add(line []byte) (kept bool, problem error) {
	if s.f == nil {
		if err := s.open(); err != nil {
			return false, err
		}
	}
	// The command line appends to this file too (its audit events), and may rotate it.
	if info, err := s.f.Stat(); err == nil && info.Size() > s.size {
		s.size = info.Size()
	}
	if s.size+int64(len(line)) > rotateBytes {
		if err := s.rotateOrFollow(); err != nil {
			if s.f == nil || s.size+int64(len(line)) > rotateBytes+s.overrun {
				return false, err
			}
			problem = err
		}
	}
	if s.torn {
		line = append([]byte{'\n'}, line...)
	}
	n, err := s.write(line)
	s.size += int64(n)
	if err == nil && n < len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.torn = s.torn || n > 0
		return false, err
	}
	s.torn = false
	return true, problem
}

// reason is what went wrong without the path, which holds a home directory and gets pasted into reports.
func reason(err error) error {
	var link *os.LinkError
	if errors.As(err, &link) {
		return link.Err
	}
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err
	}
	var unsafe *safefile.UnsafeFileError
	if errors.As(err, &unsafe) {
		return errors.New(unsafe.Problem)
	}
	return err
}

func (s *Sink) Write(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		kept, problem := s.add(line)
		// AuditErr is for whoever just wrote: the line is in this file or it isn't. A rotation that
		// failed while the line still went in isn't that line's failure.
		if kept {
			s.lastErr = nil
		} else {
			s.lastErr = problem
		}
		s.note(line, kept, problem)
	}
	if s.Mirror != nil {
		s.Mirror.Write(line)
	}
}

// note says when the log stops being written and when it starts again, not at every line. The main
// log loses the line. An audit log has already gone past its limit if it could, and puts the line on
// stderr next, because what someone with authority did must not just disappear.
func (s *Sink) note(line []byte, kept bool, problem error) {
	if problem == nil {
		if s.outage != nil {
			fmt.Fprintf(s.complaints, "local-base: the %s log is being written again, %d events were lost and %d are only on stderr\n", s.name, s.outage.Dropped, s.outage.OnStderr)
			s.outage, s.spilled = nil, 0
		}
		return
	}
	cause := reason(problem)
	first := s.outage == nil
	if first {
		s.outage = &LogOutage{Log: s.name, Path: s.path(), Since: time.Now().UTC().Format(time.RFC3339)}
	}
	s.outage.Reason = cause.Error()
	switch {
	case kept:
		if first {
			fmt.Fprintf(s.complaints, "local-base: cannot rotate the %s log, writing past its limit until it can: %v\n", s.name, cause)
		}
	case s.overrun == 0:
		if first {
			fmt.Fprintf(s.complaints, "local-base: cannot write the %s log, dropping events until it can: %v\n", s.name, cause)
		}
		s.outage.Dropped++
	case s.spilled+int64(len(line)) <= s.overrun:
		if s.outage.OnStderr == 0 {
			fmt.Fprintf(s.complaints, "local-base: cannot write the %s log, its events follow here until it can: %v\n", s.name, cause)
		}
		_, _ = s.complaints.Write(line) // Nowhere left to say it failed.
		s.spilled += int64(len(line))
		s.outage.OnStderr++
	default:
		if s.outage.Dropped == 0 {
			fmt.Fprintf(s.complaints, "local-base: the %s log still cannot be written and events are now being LOST: %v\n", s.name, cause)
		}
		s.outage.Dropped++
	}
}

// Outages lists the logs that can't rotate or be written right now.
func (s *Sink) Outages() []LogOutage {
	s.mu.Lock()
	out := s.current(nil)
	a := s.audit
	s.mu.Unlock()
	if a != nil {
		a.mu.Lock()
		out = a.current(out)
		a.mu.Unlock()
	}
	return out
}

func (s *Sink) current(out []LogOutage) []LogOutage {
	if s.outage == nil {
		return out
	}
	o := *s.outage
	o.SizeBytes = s.size
	return append(out, o)
}

// writeAudit keeps a line in the audit file as well. A file that can't be opened is an outage like
// any other, and each line tries again.
func (s *Sink) writeAudit(line []byte) {
	s.mu.Lock()
	if s.audit == nil {
		s.audit = &Sink{dir: s.dir, name: "audit", complaints: s.complaints, overrun: auditOverrun}
	}
	a := s.audit
	s.mu.Unlock()
	a.Write(line)
}

// AuditErr is the last failure to put a line in the audit file, for a short-lived writer that has
// one record to answer for. A failure of the main log alone isn't one: the record was written.
func (s *Sink) AuditErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auditOpenErr != nil || s.audit == nil {
		return s.auditOpenErr
	}
	return s.audit.lastWriteErr()
}

func (s *Sink) lastWriteErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audit != nil {
		if err := s.audit.Close(); err != nil {
			_ = s.closeFile() // The audit file's error is the one to report.
			return err
		}
	}
	return s.closeFile()
}

func (s *Sink) closeFile() error {
	if s.closed || s.f == nil {
		s.closed = true
		return nil
	}
	s.closed = true
	return s.f.Close()
}

// Handler is a slog.Handler emitting schema-2 events. Well-known attrs map to
// top-level fields: event, category, component, runtime, request_id, http
// (a group); everything else goes to `attributes`, redacted by key.
type Handler struct {
	sink  *Sink
	level slog.Level
	attrs []slog.Attr
	hooks *Hooks
}

// Hooks connect the handler to a trace exporter: Correlate names the
// recorded span active in a context, Export receives every written event
// except local diagnostics (TS logger.localDiagnostic).
type Hooks struct {
	Correlate func(ctx context.Context) (traceID, spanID string, ok bool)
	Export    func(line []byte)
}

// LocalOnly is an attribute key: an event carrying it is written to the
// local log but never exported (exporter diagnostics).
const LocalOnly = "localbase.local_only"

func NewHandler(sink *Sink, level slog.Level) *Handler {
	return &Handler{sink: sink, level: level, hooks: &Hooks{}}
}

// Hooks returns the hooks shared by this handler and those derived from it;
// set them before logging concurrently.
func (h *Handler) Hooks() *Hooks { return h.hooks }

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{sink: h.sink, level: h.level, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...), hooks: h.hooks}
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

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	localOnly := false
	ev := map[string]any{
		"schemaVersion": 2, "id": uuid(), "timestamp": r.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
		"severity": severity(r.Level), "eventName": "localbase.event", "category": "", "component": "gateway",
		"runtime": "gateway",
	}
	attributes := map[string]any{}
	errorType := "Error"
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
		case LocalOnly:
			localOnly = true
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
			ev["error"] = map[string]any{"type": errorType, "message": BoundedText(v.String(), maxMessage)}
		case "error_type":
			errorType = v.String()
			if e, ok := ev["error"].(map[string]any); ok {
				e["type"] = errorType
			}
		case "http":
			m := map[string]any{}
			for _, g := range v.Group() {
				m[g.Key] = g.Value.Resolve().Any()
			}
			if p, ok := m["path"].(string); ok {
				route, page := canonicalRoute(p)
				m["path"] = route
				switch {
				case strings.HasPrefix(page, "/_localbase/"):
					addAttr("api_route", page)
				case page != "":
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
	if h.hooks != nil && h.hooks.Correlate != nil && ctx != nil {
		if _, has := ev["trace"]; !has {
			if traceID, spanID, ok := h.hooks.Correlate(ctx); ok {
				ev["trace"] = map[string]any{"traceId": traceID, "spanId": spanID}
			}
		}
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	h.sink.Write(append(line, '\n'))
	if name, _ := ev["eventName"].(string); strings.HasPrefix(name, auditPrefix) {
		h.sink.writeAudit(append(line, '\n'))
	}
	if !localOnly && h.hooks != nil && h.hooks.Export != nil {
		h.hooks.Export(line)
	}
	return nil
}
