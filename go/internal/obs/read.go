package obs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Filters select events (logs.ts matchesLogFilters).
type Filters struct {
	Since     time.Time
	Level     string
	Runtime   string
	RequestID string
}

const maxLineBytes = 16 * 1024

// Event is a decoded log event; Raw keeps its JSON for output.
type Event struct {
	Timestamp string `json:"timestamp"`
	Severity  string `json:"severity"`
	Runtime   string `json:"runtime"`
	RequestID string `json:"requestId"`
	Component string `json:"component"`
	Message   string `json:"message"`
	HTTP      *struct {
		Method     string  `json:"method"`
		Path       string  `json:"path"`
		Status     int     `json:"status"`
		DurationMs float64 `json:"durationMs"`
	} `json:"http"`
	Trace *struct {
		TraceID string `json:"traceId"`
		SpanID  string `json:"spanId"`
	} `json:"trace"`
	Raw json.RawMessage `json:"-"`
}

var eventNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// parseEvent accepts a line that looks like a schema-2 event.
func parseEvent(line []byte) (*Event, bool) {
	if len(line) == 0 || len(line) > maxLineBytes {
		return nil, false
	}
	var head struct {
		SchemaVersion int    `json:"schemaVersion"`
		ID            string `json:"id"`
		EventName     string `json:"eventName"`
		Category      string `json:"category"`
	}
	if json.Unmarshal(line, &head) != nil || head.SchemaVersion != 2 || head.ID == "" ||
		!eventNameRe.MatchString(head.EventName) || !slices.Contains(categories, head.Category) {
		return nil, false
	}
	var e Event
	if json.Unmarshal(line, &e) != nil || e.Message == "" || !slices.Contains(runtimes, e.Runtime) {
		return nil, false
	}
	e.Raw = append(json.RawMessage{}, line...)
	return &e, true
}

func (f Filters) match(e *Event) bool {
	if !f.Since.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil || t.Before(f.Since) {
			return false
		}
	}
	return (f.Level == "" || e.Severity == f.Level) && (f.Runtime == "" || e.Runtime == f.Runtime) &&
		(f.RequestID == "" || e.RequestID == f.RequestID)
}

var archiveRe = regexp.MustCompile(`^events\.(\d+)\.jsonl$`)

// paths lists the log files oldest first: archives 5..1, then the active file.
func paths(rootDir string) []string {
	dir := filepath.Join(rootDir, "logs")
	var out []string
	for i := archives; i >= 1; i-- {
		p := filepath.Join(dir, fmt.Sprintf("events.%d.jsonl", i))
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	if st, err := os.Lstat(filepath.Join(dir, "events.jsonl")); err == nil && st.Mode().IsRegular() {
		out = append(out, filepath.Join(dir, "events.jsonl"))
	}
	return out
}

// Snapshot returns up to limit matching events, oldest first, from the
// newest end of the log.
func Snapshot(rootDir string, f Filters, limit int) ([]*Event, error) {
	files := paths(rootDir)
	var out []*Event
	for i := len(files) - 1; i >= 0 && len(out) < limit; i-- {
		b, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for j := len(lines) - 1; j >= 0 && len(out) < limit; j-- {
			if e, ok := parseEvent([]byte(lines[j])); ok && f.match(e) {
				out = append(out, e)
			}
		}
	}
	slices.Reverse(out)
	return out, nil
}

type followState struct {
	f       *os.File
	offset  int64
	rest    []byte
	visible bool
}

func identity(f *os.File) string {
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	sys := st.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", sys.Dev, sys.Ino)
}

// Follow emits matching events from every log file and keeps polling for
// new ones until ctx ends, draining rotated files before releasing them.
func Follow(ctx context.Context, rootDir string, f Filters, emit func(*Event) error) error {
	states := map[string]*followState{}
	defer func() {
		for _, s := range states {
			s.f.Close()
		}
	}()
	for ctx.Err() == nil {
		for _, s := range states {
			s.visible = false
		}
		var order []string
		for _, p := range paths(rootDir) {
			fh, err := os.Open(p)
			if err != nil {
				continue
			}
			id := identity(fh)
			if s, ok := states[id]; ok {
				s.visible = true
				fh.Close()
			} else {
				states[id] = &followState{f: fh, visible: true}
			}
			order = append(order, id)
		}
		for id, s := range states {
			if !s.visible {
				order = append([]string{id}, order...)
			}
		}
		for _, id := range order {
			s := states[id]
			st, err := s.f.Stat()
			if err != nil {
				continue
			}
			if st.Size() < s.offset {
				s.offset, s.rest = 0, nil
			}
			if st.Size() > s.offset {
				s.f.Seek(s.offset, io.SeekStart)
				r := bufio.NewReader(io.LimitReader(s.f, st.Size()-s.offset))
				data, _ := io.ReadAll(r)
				s.offset += int64(len(data))
				data = append(s.rest, data...)
				last := strings.LastIndexByte(string(data), '\n')
				var complete []byte
				if last >= 0 {
					complete, s.rest = data[:last+1], append([]byte{}, data[last+1:]...)
				} else {
					s.rest = data
				}
				if len(s.rest) > maxLineBytes {
					s.rest = nil
				}
				for _, line := range strings.Split(string(complete), "\n") {
					if e, ok := parseEvent([]byte(line)); ok && f.match(e) {
						if err := emit(e); err != nil {
							return err
						}
					}
				}
			}
			if !s.visible {
				s.f.Close()
				delete(states, id)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// Human formats an event like logs.ts formatHumanLogEvent.
func Human(e *Event) string {
	trace, req := "", ""
	if e.Trace != nil {
		trace = " trace=" + e.Trace.TraceID + "/" + e.Trace.SpanID
	}
	if e.HTTP != nil {
		req = fmt.Sprintf(" %s %s %d %.1fms", e.HTTP.Method, e.HTTP.Path, e.HTTP.Status, e.HTTP.DurationMs)
	}
	return e.Timestamp + " " + strings.ToUpper(e.Severity) + " " + e.Component + " " + e.Message + trace + req
}
