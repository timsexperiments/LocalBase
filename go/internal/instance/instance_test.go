package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// fakeGateway is an HTTP server at the address a root's owner.json names, with this test's live
// PID. proven says whether it answers the identity check with that owner's id and root hash.
type fakeGateway struct {
	mu   sync.Mutex
	seen []*http.Request
	body map[string][]byte // what each call sent
}

func (g *fakeGateway) requests() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*http.Request(nil), g.seen...)
}

func ownedRoot(t *testing.T, proven bool, handler http.HandlerFunc) (*root.Root, *fakeGateway) {
	t.Helper()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{body: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_localbase/instance" && req.Method == http.MethodGet {
			id, hash := "someone-else", "not-this-root"
			if proven && req.Header.Get("X-Localbase-Instance-Token") == "the-token" {
				id, hash = "the-instance", "the-root-hash"
			}
			json.NewEncoder(w).Encode(map[string]string{"instanceId": id, "rootHash": hash})
			return
		}
		b, _ := io.ReadAll(req.Body)
		g.mu.Lock()
		g.seen = append(g.seen, req)
		g.body[req.URL.Path] = b
		g.mu.Unlock()
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	lock := filepath.Join(r.Dir, "runtime", "gateway.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"version": 2, "instanceId": "the-instance", "root": r.Dir, "rootHash": "the-root-hash",
		"pid": os.Getpid(), "startedAt": "2026-11-10T00:00:00Z", "host": host, "port": p, "instanceToken": "the-token"})
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), owner, 0o600); err != nil {
		t.Fatal(err)
	}
	return r, g
}

// A gateway that proves it is the root's gets a client, and every call carries the instance token.
func TestDialAProvenGateway(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`{"ok":true}`)) })
	c, state, _ := Dial(r)
	if c == nil || state != "active" {
		t.Fatalf("Dial on a proven gateway: client %v, state %q", c, state)
	}
	a, err := c.Get(context.Background(), "/_localbase/placement-state", nil)
	if err != nil || a.Status != 200 {
		t.Fatalf("Get: %v %+v", err, a)
	}
	var out struct{ OK bool }
	if err := a.Decode(&out); err != nil || !out.OK {
		t.Errorf("Decode: %v %+v", err, out)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].Header.Get("X-Localbase-Instance-Token") != "the-token" {
		t.Errorf("the call didn't carry the instance token: %v", seen)
	}
}

// A gateway that doesn't prove it is the root's gets no client and no call: only the identity
// check reaches it.
func TestDialRefusesAnUnprovenGateway(t *testing.T) {
	r, g := ownedRoot(t, false, func(w http.ResponseWriter, req *http.Request) {})
	c, state, detail := Dial(r)
	if c != nil || state == "active" || detail == "" {
		t.Fatalf("Dial on an unproven gateway: client %v, state %q, detail %q", c, state, detail)
	}
	if seen := g.requests(); len(seen) != 0 {
		t.Errorf("an unproven gateway was sent %d calls", len(seen))
	}
}

// With no owner record, there is no gateway to dial.
func TestDialWithNoGateway(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	if c, state, _ := Dial(r); c != nil || state != "missing" {
		t.Errorf("Dial with no owner: client %v, state %q", c, state)
	}
}

// A query value is encoded, so one with &, a space or a control byte can't change the URL.
func TestAQueryValueCannotChangeTheURL(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`{}`)) })
	c, _, _ := Dial(r)
	id := "a b&model=other\x01"
	if _, err := c.Get(context.Background(), "/_localbase/placement-state", map[string][]string{"model": {id}}); err != nil {
		t.Fatal(err)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].URL.Query().Get("model") != id || len(seen[0].URL.Query()["model"]) != 1 {
		t.Errorf("the query reached the gateway as %q", seen[0].URL.RawQuery)
	}
}

// Post sends its value as a JSON body.
func TestPostSendsJSON(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(202); w.Write([]byte(`{}`)) })
	c, _, _ := Dial(r)
	a, err := c.Post(context.Background(), "/_localbase/model-management", map[string]string{"modelId": "m", "action": "cancel"})
	if err != nil || a.Status != 202 {
		t.Fatalf("Post: %v %+v", err, a)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].Header.Get("Content-Type") != "application/json" ||
		!bytes.Equal(bytes.TrimSpace(g.body["/_localbase/model-management"]), []byte(`{"action":"cancel","modelId":"m"}`)) {
		t.Errorf("the body reached the gateway as %q", g.body["/_localbase/model-management"])
	}
}

// An answer larger than MaxAnswer is refused rather than read whole.
func TestAnAnswerPastTheLimitIsRefused(t *testing.T) {
	r, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write(bytes.Repeat([]byte("x"), MaxAnswer+1)) })
	c, _, _ := Dial(r)
	if _, err := c.Get(context.Background(), "/big", nil); !errors.Is(err, ErrAnswerTooLarge) {
		t.Errorf("an answer past the limit: %v", err)
	}
	r2, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write(bytes.Repeat([]byte("x"), MaxAnswer)) })
	c2, _, _ := Dial(r2)
	if a, err := c2.Get(context.Background(), "/big", nil); err != nil || len(a.Body) != MaxAnswer {
		t.Errorf("an answer at the limit: %v", err)
	}
}

// A call keeps to its context's deadline.
func TestACallKeepsToItsDeadline(t *testing.T) {
	r, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c, _, _ := Dial(r)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Get(ctx, "/slow", nil); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("a slow answer past the deadline: %v after %v", err, time.Since(start))
	}
}
