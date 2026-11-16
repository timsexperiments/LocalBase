package lab

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// Event is one line of the gateway's event log. A foreground gateway mirrors
// every event to stderr, so serve.log has them all, never rotated away.
type Event struct {
	Name       string         `json:"eventName"`
	Severity   string         `json:"severity"`
	Time       time.Time      `json:"timestamp"`
	Runtime    string         `json:"runtime"`
	RequestID  string         `json:"requestId"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"attributes"`
	HTTP       struct {
		Status int `json:"status"`
	} `json:"http"`
}

// Attr is a string attribute, or "".
func (e Event) Attr(name string) string {
	s, _ := e.Attributes[name].(string)
	return s
}

// Num is a numeric attribute and whether it is one.
func (e Event) Num(name string) (float64, bool) {
	n, ok := e.Attributes[name].(float64)
	return n, ok
}

// LogTail reads a gateway's serve.log incrementally. Keep decides which events
// it holds on to; every event is passed to Seen, and every line that is not an
// event to Raw (a panic or a runtime fatal prints those).
type LogTail struct {
	Path string
	Keep func(Event) bool
	Seen func(Event)
	Raw  func(string)
	// Line sees every line, event or not.
	Line func(string)

	mu     sync.Mutex
	off    int64
	events []Event
}

// Read consumes what has been written since the last call. A partial last line
// waits for the next call.
func (t *LogTail) Read() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(t.off, 0); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil // a partial line stays for the next read
		}
		t.off += int64(len(line))
		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		if t.Line != nil {
			t.Line(line)
		}
		var e Event
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &e) != nil || e.Name == "" {
			if t.Raw != nil {
				t.Raw(line)
			}
			continue
		}
		if t.Seen != nil {
			t.Seen(e)
		}
		if t.Keep == nil || t.Keep(e) {
			t.events = append(t.events, e)
		}
	}
}

// Events are the kept events so far, in log order.
func (t *LogTail) Events() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Event(nil), t.events...)
}
