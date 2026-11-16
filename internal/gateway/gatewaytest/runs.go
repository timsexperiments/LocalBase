package gatewaytest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// SyncBuffer is a log sink safe for the gateway's goroutines.
type SyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// WaitFor polls cond for up to d.
func WaitFor(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// RunEvent is one event of a run's event stream: its id, its name and its data.
type RunEvent struct {
	ID         int
	Name, Data string
}

// RunClient talks to one gateway as one caller (key "" under --no-auth).
type RunClient struct {
	T    *testing.T
	Base string
	Key  string
}

// Do sends a request as the caller, with the headers given as name, value pairs, and returns the
// status and the JSON answer.
func (c RunClient) Do(method, path, body string, hdr ...string) (int, map[string]any) {
	c.T.Helper()
	req, _ := http.NewRequest(method, c.Base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.T.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// ChatBody is a chat request for ChatModel with one user message, content.
func ChatBody(content string) string {
	return `{"model":"` + ChatModel + `","messages":[{"role":"user","content":"` + content + `"}]}`
}

// Start creates a chat run and returns its id.
func (c RunClient) Start(content string) string {
	c.T.Helper()
	code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", ChatBody(content))
	if code != 202 || m["object"] != "localbase.run" {
		c.T.Fatalf("create: %d %v", code, m)
	}
	return m["id"].(string)
}

// Get returns the run, failing the test unless it's found.
func (c RunClient) Get(id string) map[string]any {
	c.T.Helper()
	code, m := c.Do("GET", "/_localbase/runs/"+id, "")
	if code != 200 {
		c.T.Fatalf("get %s: %d %v", id, code, m)
	}
	return m
}

// WaitStatus polls until the run reaches one of the statuses.
func (c RunClient) WaitStatus(id string, statuses ...string) map[string]any {
	c.T.Helper()
	var m map[string]any
	if !WaitFor(10*time.Second, func() bool {
		m = c.Get(id)
		for _, s := range statuses {
			if m["status"] == s {
				return true
			}
		}
		// A run that has ended won't become what is wanted, and what it says is why.
		switch m["status"] {
		case "completed", "failed", "cancelled", "interrupted":
			return true
		}
		return false
	}) {
		c.T.Fatalf("run %s: status %v, want %v", id, m["status"], statuses)
	}
	if !slices.Contains(statuses, fmt.Sprint(m["status"])) {
		c.T.Fatalf("run %s ended %v, want %v: reason %v, error %v", id, m["status"], statuses, m["reason"], m["error"])
	}
	return m
}

// Content is the text of a run's output.
func Content(m map[string]any) string {
	out, _ := m["output"].(map[string]any)
	s, _ := out["content"].(string)
	return s
}

// Events reads a run's event stream until stop returns true or the stream
// ends; it returns the events, comments seen, and the response headers.
func (c RunClient) Events(id string, hdr []string, query string, stop func([]RunEvent) bool) ([]RunEvent, int, http.Header) {
	c.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/_localbase/runs/"+id+"/events"+query, nil)
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.T.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.T.Fatalf("events %s: %d", id, resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	var evs []RunEvent
	comments := 0
	cur := RunEvent{}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return evs, comments, resp.Header
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.Name != "" {
				evs = append(evs, cur)
				if stop != nil && stop(evs) {
					return evs, comments, resp.Header
				}
			}
			cur = RunEvent{}
		case strings.HasPrefix(line, ":"):
			comments++
			if stop != nil && stop(nil) {
				return evs, comments, resp.Header
			}
		case strings.HasPrefix(line, "id: "):
			cur.ID, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// Words is what the fake backend streams for "fake:slow n" and "fake:burst n": "w1 w2 ... wn ".
func Words(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "w%d ", i+1)
	}
	return b.String()
}
