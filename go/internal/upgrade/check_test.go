package upgrade

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type fakeAPI struct {
	srv             *httptest.Server
	requests        atomic.Int32
	status          atomic.Int32
	tag             atomic.Pointer[string]
	lastIfNoneMatch atomic.Pointer[string]
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.status.Store(200)
	tag := "v9.9.9"
	f.tag.Store(&tag)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		etag := r.Header.Get("If-None-Match")
		f.lastIfNoneMatch.Store(&etag)
		if etag != "" && etag == `"e1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		if s := f.status.Load(); s != 200 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(int(s))
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.test/r"}`, *f.tag.Load())
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newChecker(t *testing.T, f *fakeAPI, logs *bytes.Buffer) (*Checker, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	c := NewChecker(st, Source{API: f.srv.URL, Client: NewClient()}, "0.2.0", true, slog.New(slog.NewJSONHandler(logs, nil)))
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	return c, st, &now
}

// Off by default: nothing is asked, however long it runs.
func TestUpdateCheckOffByDefault(t *testing.T) {
	f := newFakeAPI(t)
	c, _, _ := newChecker(t, f, nil)
	c.firstWait, c.tick = time.Millisecond, time.Millisecond
	if c.Enabled(context.Background()) {
		t.Fatal("on by default")
	}
	stop := c.Start()
	time.Sleep(30 * time.Millisecond)
	stop()
	if got := f.requests.Load(); got != 0 {
		t.Errorf("%d requests with the check off", got)
	}
	if s := c.Read(context.Background()); s.Enabled || s.Latest != nil || s.CheckedAt != nil || s.UpdateAvailable {
		t.Errorf("%+v", s)
	}
}

func TestUpdateCheckSchedule(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)
	c, st, now := newChecker(t, f, nil)
	if err := c.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Turning it on does not itself ask.
	if got := f.requests.Load(); got != 0 {
		t.Errorf("%d requests on turning it on", got)
	}
	if !c.CheckIfDue(ctx) || f.requests.Load() != 1 {
		t.Fatalf("the first check: %d", f.requests.Load())
	}
	s := c.Read(ctx)
	if !s.Enabled || s.Latest == nil || *s.Latest != "v9.9.9" || !s.UpdateAvailable || s.URL == nil {
		t.Errorf("%+v", s)
	}
	// Not again for 24 hours, including across a restart.
	*now = now.Add(23 * time.Hour)
	if c.CheckIfDue(ctx) || f.requests.Load() != 1 {
		t.Errorf("checked after 23 hours: %d", f.requests.Load())
	}
	again := NewChecker(st, c.src, "0.2.0", true, c.log)
	again.now = c.now
	if again.CheckIfDue(ctx) || f.requests.Load() != 1 {
		t.Errorf("a restart checked again: %d", f.requests.Load())
	}
	// A day later it asks, conditionally, and an unchanged answer stands.
	*now = now.Add(2 * time.Hour)
	if !c.CheckIfDue(ctx) || f.requests.Load() != 2 {
		t.Fatalf("a day later: %d", f.requests.Load())
	}
	if got := *f.lastIfNoneMatch.Load(); got != `"e1"` {
		t.Errorf("If-None-Match %q", got)
	}
	if s := c.Read(ctx); s.Latest == nil || *s.Latest != "v9.9.9" {
		t.Errorf("a 304 lost the answer: %+v", s)
	}
}

// A failure counts as the attempt, stays quiet, and blocks nothing.
func TestUpdateCheckFailureIsQuiet(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	f := newFakeAPI(t)
	c, st, now := newChecker(t, f, &logs)
	c.SetEnabled(ctx, true)
	c.CheckIfDue(ctx)
	st.SetSetting(ctx, etagKey, "") // so the next asks unconditionally
	*now = now.Add(25 * time.Hour)
	f.status.Store(403)
	c.CheckIfDue(ctx)
	if s := c.Read(ctx); s.Latest == nil || *s.Latest != "v9.9.9" {
		t.Errorf("a failure lost the cached answer: %+v", s)
	}
	if strings.Count(logs.String(), "update.check-failed") != 1 {
		t.Errorf("logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a rate limit is not an error worth raising: %s", logs.String())
	}
	before := f.requests.Load()
	*now = now.Add(time.Hour)
	if c.CheckIfDue(ctx) || f.requests.Load() != before {
		t.Errorf("a failure should wait a day like anything else: %d", f.requests.Load())
	}
	// A server that never answers cannot delay the caller past the timeout, and Start returns at once.
	c.firstWait = time.Hour
	begin := time.Now()
	stop := c.Start()
	stop()
	if d := time.Since(begin); d > time.Second {
		t.Errorf("Start or stop blocked for %v", d)
	}
}

func TestUpdateCheckIgnoresOtherTags(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)
	c, _, _ := newChecker(t, f, nil)
	c.SetEnabled(ctx, true)
	tag := "kokoro-v0.0.2"
	f.tag.Store(&tag)
	if !c.CheckIfDue(ctx) {
		t.Fatal("it should have asked")
	}
	if s := c.Read(ctx); s.Latest != nil || s.UpdateAvailable {
		t.Errorf("a runtime tag counted: %+v", s)
	}
}

func TestDevelopmentBuildNeverChecks(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)
	c, _, _ := newChecker(t, f, nil)
	c.release = false
	c.SetEnabled(ctx, true)
	if c.CheckIfDue(ctx) || f.requests.Load() != 0 {
		t.Errorf("%d requests", f.requests.Load())
	}
}

// Turned off again, the stored answer stops being offered.
func TestUpdateCheckTurnedOff(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)
	c, _, now := newChecker(t, f, nil)
	c.SetEnabled(ctx, true)
	c.CheckIfDue(ctx)
	c.SetEnabled(ctx, false)
	if c.Read(ctx).UpdateAvailable {
		t.Error("off, it should not say a version is available")
	}
	*now = now.Add(48 * time.Hour)
	before := f.requests.Load()
	if c.CheckIfDue(ctx) || f.requests.Load() != before {
		t.Errorf("off, it asked: %d", f.requests.Load())
	}
}
