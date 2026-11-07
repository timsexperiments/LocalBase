package web

// Tests for the web review's lab findings (03-M1, M5, M6), each failing on main.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

const mib = 1 << 20

// 03-M1: the store is bounded by bytes, in all and per person; a person over their share loses only
// their own oldest results; expired results go when something new comes in.
func TestResultStoreBudgets(t *testing.T) {
	var s resultStore
	other := s.put("b", "audio/wav", make([]byte, 50*mib))
	first := s.put("a", "audio/wav", make([]byte, 50*mib))
	s.put("a", "audio/wav", make([]byte, 50*mib))
	s.put("a", "audio/wav", make([]byte, 50*mib)) // a now holds 150 MiB: over its 128 MiB share
	if s.get("a", first) != nil {
		t.Error("a person over their share kept their oldest result")
	}
	if s.get("b", other) == nil {
		t.Error("one person's results pushed out someone else's")
	}
	if s.each["a"] > resultBytesEach || s.bytes > resultBytes {
		t.Errorf("over budget: a %d, all %d", s.each["a"], s.bytes)
	}
	// Over the whole budget, the oldest goes, whoever's it is.
	s.put("c", "audio/wav", make([]byte, 100*mib))
	if s.bytes > resultBytes {
		t.Errorf("over the whole budget: %d", s.bytes)
	}
	// Expired results go when something new comes in, not only when they're asked for.
	var e resultStore
	old := e.put("a", "image/png", make([]byte, mib))
	e.items[old].at = time.Now().Add(-resultTTL - time.Minute)
	e.put("a", "image/png", []byte("x"))
	if _, kept := e.items[old]; kept || e.bytes != 1 {
		t.Errorf("an expired result stayed in memory: kept %v, %d bytes", kept, e.bytes)
	}
	// The count cap still holds.
	var c resultStore
	for range resultLimit + 5 {
		c.put("a", "image/png", []byte("x"))
	}
	if len(c.items) != resultLimit || len(c.order) != resultLimit {
		t.Errorf("count: %d items, %d in order", len(c.items), len(c.order))
	}
}

// 03-M1: notes are capped by count, the oldest going first.
func TestLabNotesAreCapped(t *testing.T) {
	var n labNotes
	for i := range noteLimit + 40 {
		n.put("a", fmt.Sprint("run", i), "words")
	}
	if len(n.items) > noteLimit {
		t.Errorf("%d notes kept", len(n.items))
	}
	if n.get("a", fmt.Sprint("run", noteLimit+39)) == nil {
		t.Error("the newest note was dropped")
	}
}

// 03-M5: a note's data is set and read from several requests at once without a race (run with -race).
func TestLabNoteConcurrentReadsAndWrites(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("png"))
	u := New(Deps{NoAuth: true, Call: func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
		return APIResult{Status: 200, Body: []byte(`{"data":[{"b64_json":"` + img + `"}]}`)}
	}})
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u.principal(r)))
	owner := u.caller(r).OwnerID()
	u.notes.put(owner, "run1", "a boat")
	run := labRun{ID: "run1", Target: "/v1/images/generations", Status: "completed"}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); u.notes.setImage(owner, "run1", imageRequest{Prompt: "a boat", N: 1 + i%2}) }()
		go func() { defer wg.Done(); u.labItem(r, run) }()
	}
	wg.Wait()
	if v := u.labItem(r, run); len(v.Pictures) != 1 {
		t.Errorf("collected %d pictures, want 1 (collected once)", len(v.Pictures))
	}
}

// 03-M5: one person's slow result read doesn't hold up another person's tray.
func TestSlowCollectDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	img := base64.StdEncoding.EncodeToString([]byte("png"))
	u := New(Deps{NoAuth: true, Call: func(_ context.Context, _ *auth.Principal, _, path, _ string, _ []byte) APIResult {
		if strings.Contains(path, "/slow/") {
			<-release
		}
		return APIResult{Status: 200, Body: []byte(`{"data":[{"b64_json":"` + img + `"}]}`)}
	}})
	as := func(id string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		return r.WithContext(context.WithValue(r.Context(), ctxKey{}, &auth.Principal{Kind: "browser-session", ID: id}))
	}
	slow, fast := as("a"), as("b")
	u.notes.put("a", "slow", "x")
	u.notes.put("b", "fast", "y")
	go u.labItem(slow, labRun{ID: "slow", Target: "/v1/images/generations", Status: "completed"})
	time.Sleep(50 * time.Millisecond)
	done := make(chan labItemView, 1)
	go func() {
		done <- u.labItem(fast, labRun{ID: "fast", Target: "/v1/images/generations", Status: "completed"})
	}()
	select {
	case v := <-done:
		if len(v.Pictures) != 1 {
			t.Errorf("the other person's tray: %d pictures", len(v.Pictures))
		}
	case <-time.After(2 * time.Second):
		t.Error("another person's tray waited on someone else's slow result read")
	}
	close(release)
}

// 03-M6: a player's Range requests fetch the video from the API once, not once each.
func TestVideoRangeRequestsFetchOnce(t *testing.T) {
	video := bytes.Repeat([]byte("0123456789"), 1000)
	calls := 0
	u := New(Deps{NoAuth: true, Call: func(_ context.Context, _ *auth.Principal, _, path, _ string, _ []byte) APIResult {
		if strings.HasSuffix(path, "/content") {
			calls++
			return APIResult{Status: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: video}
		}
		return APIResult{Status: 404}
	}})
	for i := range 5 {
		req := httptest.NewRequest("GET", "/app/lab/video/job1/content", nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", i*100, i*100+99))
		w := httptest.NewRecorder()
		u.ServeHTTP(w, req)
		got, _ := io.ReadAll(w.Body)
		if w.Code != http.StatusPartialContent || !bytes.Equal(got, video[i*100:i*100+100]) {
			t.Errorf("range %d: %d, %q", i, w.Code, got[:min(len(got), 20)])
		}
	}
	if calls != 1 {
		t.Errorf("the video was fetched %d times for 5 range requests", calls)
	}
}
