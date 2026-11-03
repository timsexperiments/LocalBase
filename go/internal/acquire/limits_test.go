package acquire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// limit sets one of the package's limits for a test.
func limit[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// endless sends data with no length, until the client goes away.
func endless(t *testing.T) *httptest.Server {
	t.Helper()
	chunk := make([]byte, 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Far above any limit these tests set, so a broken limit fails a test quickly
		// instead of filling the disk.
		for sent := 0; sent < 64<<20; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if r.Header.Get("Range") == "bytes=0-0" {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func plenty(t *testing.T) {
	t.Helper()
	limit(t, &availableBytes, func(string) (int64, error) { return 1 << 50, nil })
}

func refusedCode(err error) string {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return refused.Code
	}
	return ""
}

func resolveFrom(t *testing.T, root, source string) *Plan {
	t.Helper()
	p, err := Resolve(context.Background(), root, Request{Source: source, AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: "m", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A source that does not say how big a file is cannot send for ever.
func TestUnknownSizeHasABudget(t *testing.T) {
	plenty(t)
	limit(t, &maxUnknownSizeBytes, 1<<20)
	root := t.TempDir()
	p := resolveFrom(t, root, endless(t).URL+"/m.gguf")
	if p.SizeKnown {
		t.Fatal("the size should be unknown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Fetch(ctx, root, p, nil)
	if refusedCode(err) != CodeTooLarge {
		t.Fatalf("%v, want %s", err, CodeTooLarge)
	}
	matches, _ := filepath.Glob(filepath.Join(stagingDir(root, p), "*"))
	if len(matches) != 0 {
		t.Errorf("left behind: %v", matches)
	}
}

// A running download stops before it uses the space the database and logs need,
// whatever the source claimed, and it frees what it used.
func TestDownloadLeavesTheReserve(t *testing.T) {
	limit(t, &freeSpaceReserve, 1<<20)
	limit(t, &freeSpaceEvery, 128<<10)
	calls := 0
	limit(t, &availableBytes, func(string) (int64, error) {
		calls++
		if calls > 3 { // something else filled the disk meanwhile
			return 1 << 10, nil
		}
		return 1 << 40, nil
	})
	root := t.TempDir()
	p := resolveFrom(t, root, endless(t).URL+"/m.gguf")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Fetch(ctx, root, p, nil)
	if refusedCode(err) != CodeNoSpace {
		t.Fatalf("%v, want %s", err, CodeNoSpace)
	}
	matches, _ := filepath.Glob(filepath.Join(stagingDir(root, p), "*"))
	if len(matches) != 0 {
		t.Errorf("left behind: %v", matches)
	}
}

// What a source says it will send is checked before anything is written.
func TestClaimedSizeMustFit(t *testing.T) {
	limit(t, &freeSpaceReserve, 1<<30)
	limit(t, &availableBytes, func(string) (int64, error) { return 2 << 30, nil })
	root := t.TempDir()
	const huge = 1_000_000_000_000_000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", int64(huge)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("G"))
			return
		}
		t.Error("the download started")
	}))
	defer srv.Close()
	p := resolveFrom(t, root, srv.URL+"/m.gguf")
	err := CheckSpace(root, p)
	if refusedCode(err) != CodeNoSpace || !strings.Contains(err.Error(), "1.0 GiB") {
		t.Errorf("%v", err)
	}
	if _, err := Fetch(context.Background(), root, p, nil); refusedCode(err) != CodeNoSpace {
		t.Errorf("fetch: %v", err)
	}
	if _, err := os.Stat(stagingDir(root, p)); err == nil {
		t.Error("a staging directory was made for it")
	}
}

// What is already staged counts: resuming needs only the rest.
func TestResumeNeedsOnlyTheRest(t *testing.T) {
	limit(t, &freeSpaceReserve, 0)
	limit(t, &availableBytes, func(string) (int64, error) { return 100, nil })
	root := t.TempDir()
	p := &Plan{ID: "m", Kind: "url", Display: "x", Files: []PlannedFile{{Filename: "m.gguf", SizeBytes: 150}}}
	if err := CheckSpace(root, p); refusedCode(err) != CodeNoSpace {
		t.Fatalf("nothing staged: %v", err)
	}
	st := stagingDir(root, p)
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st, "m.gguf.partial"), make([]byte, 60), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpace(root, p); err != nil {
		t.Errorf("60 of 150 staged, 100 free: %v", err)
	}
}

// A disk that cannot be read does not stop an import: the running check still applies.
func TestUnreadableFreeSpaceDoesNotRefuse(t *testing.T) {
	limit(t, &availableBytes, func(string) (int64, error) { return 0, errors.New("no") })
	p := &Plan{ID: "m", Kind: "url", Display: "x", Files: []PlannedFile{{Filename: "m.gguf", SizeBytes: 1 << 40}}}
	if err := CheckSpace(t.TempDir(), p); err != nil {
		t.Error(err)
	}
}

// Nothing is a model until it is published.
func TestFetchPublishesNothing(t *testing.T) {
	plenty(t)
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, ggufBytes(t, "qwen3", 2048)) }))
	defer srv.Close()
	f, err := Fetch(context.Background(), root, resolveFrom(t, root, srv.URL+"/m.gguf"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ImportedDir(root, "m")); err == nil {
		t.Fatal("published by Fetch")
	}
	res, err := f.Publish()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "m.gguf")); err != nil {
		t.Error(err)
	}
	// Something published under that id meanwhile is not replaced.
	f2 := &Fetched{staging: t.TempDir(), dest: res.Dir, model: f.model, progress: func(Progress) {}}
	if _, err := f2.Publish(); err == nil || !strings.Contains(err.Error(), "already imported") {
		t.Errorf("published over an existing model: %v", err)
	}
}

func TestPlanHasAFileLimit(t *testing.T) {
	plenty(t)
	limit(t, &maxPlanFiles, 2)
	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("big-%05d-of-00003.gguf", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Resolve(context.Background(), t.TempDir(), Request{Source: filepath.Join(dir, "big-00001-of-00003.gguf"), AllowLocal: true, Task: "chat"})
	if refusedCode(err) != CodeTooMany {
		t.Errorf("%v", err)
	}
}

// An abandoned download is kept for resuming, but not for ever.
func TestStagingIsPruned(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "models", ".staging")
	make := func(name string, age time.Duration) string {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "m.gguf.partial")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		os.Chtimes(f, at, at)
		os.Chtimes(d, at, at)
		return d
	}
	old := make("old", stagingMaxAge+time.Hour)
	fresh := make("fresh", time.Hour)
	kept := make("kept", stagingMaxAge+time.Hour)
	// A file written recently keeps an old directory.
	active := make("active", stagingMaxAge+time.Hour)
	now := time.Now()
	os.Chtimes(filepath.Join(active, "m.gguf.partial"), now, now)
	os.Chtimes(active, now.Add(-stagingMaxAge-time.Hour), now.Add(-stagingMaxAge-time.Hour))
	PruneStaging(root, kept)
	for path, want := range map[string]bool{old: false, fresh: true, kept: true, active: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
}

// A root's own limit replaces the default, and the refusal says how to raise it in words
// that suit who is reading: the command on the host, a plain sentence elsewhere.
func TestUnknownSizeLimitComesFromThePlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		onHost bool
		want   string
		not    string
	}{
		{"through the API", false, "Whoever runs this LocalBase can raise the limit", "local-base configure"},
		{"on the host", true, "local-base configure --import-max-unknown-gib <GiB>", "Whoever runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{MaxUnknownBytes: 1 << 20, onHost: tc.onHost}
			b := p.budget(t.TempDir(), 0)
			if err := b.check(1 << 20); err != nil {
				t.Errorf("at the limit: %v", err)
			}
			err := b.check(1<<20 + 1)
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Code != CodeTooLarge {
				t.Fatalf("past the limit: %v", err)
			}
			if !strings.Contains(refused.Message, tc.want) || strings.Contains(refused.Message, tc.not) {
				t.Errorf("message: %s", refused.Message)
			}
		})
	}
	// No limit set on the plan: the default, and a stated size isn't limited at all.
	limit(t, &maxUnknownSizeBytes, 1<<10)
	if err := (&Plan{}).budget(t.TempDir(), 0).check(1<<10 + 1); err == nil {
		t.Error("the default limit didn't apply")
	}
	limit(t, &availableBytes, func(string) (int64, error) { return 1 << 60, nil })
	if err := (&Plan{}).budget(t.TempDir(), 1<<30).check(1 << 29); err != nil {
		t.Errorf("a file of stated size: %v", err)
	}
}

// Through a real download, a plan's own limit is what stops a source with no stated size.
func TestDownloadKeepsToThePlansLimit(t *testing.T) {
	root := t.TempDir()
	srv := endless(t)
	p, err := Resolve(context.Background(), root, Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: "limited", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	p.MaxUnknownBytes = 256 << 10
	_, err = Fetch(context.Background(), root, p, nil)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Code != CodeTooLarge {
		t.Fatalf("%v", err)
	}
}
