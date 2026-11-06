package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// A pinned file already on disk with the right size and the wrong bytes is not the pinned file.
func TestFetchPinnedChecksWhatIsAlreadyThere(t *testing.T) {
	right := []byte(strings.Repeat("the pinned bytes ", 100))
	sum := sha256.Sum256(right)
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		serveRange(w, r, right)
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	art := models.Artifact{Filename: "m.gguf", SizeBytes: int64(len(right)), SHA256: hex.EncodeToString(sum[:]),
		Source: models.Source{Kind: "huggingface", Repo: "org/repo", Revision: strings.Repeat("a", 40), Path: "m.gguf"}}
	dir := t.TempDir()
	final := filepath.Join(dir, "m.gguf")
	wrong := append([]byte(nil), right...)
	wrong[3] ^= 0xff
	if err := os.WriteFile(final, wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	err := FetchPinned(context.Background(), dir, []models.Artifact{art}, "", nil)
	got, _ := os.ReadFile(final)
	var mismatch *HashMismatchError
	if err != nil && !errors.As(err, &mismatch) {
		t.Fatalf("%v", err)
	}
	if err == nil && string(got) != string(right) {
		t.Error("FetchPinned kept a file of the right size with the wrong bytes")
	}
	// What is right stays, with nothing fetched.
	if err := os.WriteFile(final, right, 0o600); err != nil {
		t.Fatal(err)
	}
	served.Store(0)
	if err := FetchPinned(context.Background(), dir, []models.Artifact{art}, "", nil); err != nil || served.Load() != 0 {
		t.Errorf("a correct file: %v, %d requests", err, served.Load())
	}
}

// resumingServer serves one object at a time, and can change it between requests, as a URL that
// isn't pinned may. It honors If-Range as a real server does.
type resumingServer struct {
	body atomic.Value // []byte
	etag atomic.Value // string
	cut  atomic.Int64 // when set, the first response stops after this many bytes
	// deaf ignores If-Range, as some servers and proxies do.
	deaf atomic.Bool
	// ifRanges counts the requests that carried If-Range.
	ifRanges atomic.Int64
}

func (s *resumingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := s.body.Load().([]byte)
	etag := s.etag.Load().(string)
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	start := 0
	if rng := r.Header.Get("Range"); rng != "" {
		ifr := r.Header.Get("If-Range")
		if ifr != "" {
			s.ifRanges.Add(1)
		}
		if ifr == "" || ifr == etag || s.deaf.Load() {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
	if start > 0 {
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
		w.WriteHeader(206)
	}
	out := body[start:]
	if n := s.cut.Swap(0); n > 0 {
		w.Write(out[:n])
		panic(http.ErrAbortHandler)
	}
	w.Write(out)
}

// An unpinned URL whose object changes mid-download must not produce a file that is part of each,
// and is then published with a hash that only says which bytes were received.
func TestResumeAcrossAChangedObject(t *testing.T) {
	const size = 1 << 16
	old, changed := []byte(strings.Repeat("o", size)), []byte(strings.Repeat("n", size))
	for name, tags := range map[string][2]string{"the server gives an ETag": {`"v1"`, `"v2"`}, "the server gives none": {"", ""}} {
		t.Run(name, func(t *testing.T) {
			s := &resumingServer{}
			s.body.Store(old)
			s.etag.Store(tags[0])
			s.cut.Store(size / 2)
			srv := httptest.NewServer(s)
			defer srv.Close()
			partial := filepath.Join(t.TempDir(), "m.gguf.partial")
			notices := 0
			try := func() error {
				return download(context.Background(), srv.Client(), srv.URL, partial, 0, nil, newBudget(filepath.Dir(partial), 0), resumeRule{cannotResume: func() { notices++ }}, func(int64) {})
			}
			if err := try(); err == nil {
				t.Fatal("the cut download succeeded")
			}
			s.body.Store(changed)
			s.etag.Store(tags[1])
			if err := try(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(partial)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(changed) {
				t.Errorf("the file mixes two objects: %d bytes, starts %q, ends %q", len(got), got[:1], got[len(got)-1:])
			}
			if _, err := os.Stat(validatorPath(partial)); err == nil {
				t.Error("the validator stayed after the download finished")
			}
			if want := map[string]int{"the server gives an ETag": 0, "the server gives none": 1}[name]; notices != want {
				t.Errorf("told the person %d times, want %d", notices, want)
			}
			if tags[0] != "" && s.ifRanges.Load() != 1 {
				t.Errorf("%d requests carried If-Range, want 1", s.ifRanges.Load())
			}
		})
	}
}

// A server that ignores If-Range and resumes anyway says which object it sent, and that is checked.
func TestResumeFromAServerThatIgnoresIfRange(t *testing.T) {
	const size = 1 << 16
	changed := []byte(strings.Repeat("n", size))
	s := &resumingServer{}
	s.body.Store([]byte(strings.Repeat("o", size)))
	s.etag.Store(`"v1"`)
	s.cut.Store(size / 2)
	s.deaf.Store(true)
	srv := httptest.NewServer(s)
	defer srv.Close()
	partial := filepath.Join(t.TempDir(), "m.gguf.partial")
	try := func() error {
		return download(context.Background(), srv.Client(), srv.URL, partial, 0, nil, newBudget(filepath.Dir(partial), 0), resumeRule{}, func(int64) {})
	}
	if err := try(); err == nil {
		t.Fatal("the cut download succeeded")
	}
	s.body.Store(changed)
	s.etag.Store(`"v2"`)
	if err := try(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(partial); string(got) != string(changed) {
		t.Errorf("the file mixes two objects: %d bytes", len(got))
	}
}

// A pin makes resuming safe whatever the server says, because the hash decides: it still resumes.
func TestPinnedDownloadResumes(t *testing.T) {
	const size = 1 << 16
	body := []byte(strings.Repeat("x", size))
	s := &resumingServer{}
	s.body.Store(body)
	s.etag.Store("")
	s.cut.Store(size / 2)
	var ranged atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		s.ServeHTTP(w, r)
	}))
	defer srv.Close()
	partial := filepath.Join(t.TempDir(), "m.gguf.partial")
	try := func() error {
		return download(context.Background(), srv.Client(), srv.URL, partial, size, nil, newBudget(filepath.Dir(partial), size), resumeRule{pinned: true}, func(int64) {})
	}
	if err := try(); err == nil {
		t.Fatal("the cut download succeeded")
	}
	if err := try(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(partial); string(got) != string(body) || ranged.Load() != 1 {
		t.Errorf("%d bytes, %d ranged requests", len(got), ranged.Load())
	}
}

// Told once, not per retry: a source with no validator that keeps failing.
func TestCannotResumeIsSaidOnce(t *testing.T) {
	const size = 1 << 16
	s := &resumingServer{}
	s.body.Store([]byte(strings.Repeat("x", size)))
	s.etag.Store("")
	srv := httptest.NewServer(s)
	defer srv.Close()
	partial := filepath.Join(t.TempDir(), "m.gguf.partial")
	notices := 0
	for range 4 {
		s.cut.Store(size / 2)
		_ = download(context.Background(), srv.Client(), srv.URL, partial, 0, nil, newBudget(filepath.Dir(partial), 0),
			resumeRule{cannotResume: func() { notices++ }}, func(int64) {})
	}
	if notices != 1 {
		t.Errorf("told the person %d times, want once", notices)
	}
}

// A second mismatch, after fetching again, is an error naming the file. The note says what happened.
func TestFetchPinnedSaysWhenItFetchesAgain(t *testing.T) {
	right := []byte(strings.Repeat("the pinned bytes ", 100))
	sum := sha256.Sum256(right)
	wrong := append([]byte(nil), right...)
	wrong[3] ^= 0xff
	source := wrong // the pinned source itself is wrong
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, source) }))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	art := models.Artifact{Filename: "m.gguf", SizeBytes: int64(len(right)), SHA256: hex.EncodeToString(sum[:]),
		Source: models.Source{Kind: "huggingface", Repo: "org/repo", Revision: strings.Repeat("a", 40), Path: "m.gguf"}}
	dir := t.TempDir()
	final := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(final, wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	var notes []string
	progress := func(p Progress) {
		if p.Note != "" {
			notes = append(notes, p.Note)
		}
	}
	err := FetchPinned(context.Background(), dir, []models.Artifact{art}, "", progress)
	var mismatch *HashMismatchError
	if !errors.As(err, &mismatch) || mismatch.File != "m.gguf" {
		t.Errorf("a pinned source that is wrong: %v", err)
	}
	if len(notes) != 1 || notes[0] != noteFetchedAgain {
		t.Errorf("notes: %q", notes)
	}
	if _, err := os.Stat(final); err == nil {
		t.Error("the wrong file is still in place")
	}
	source = right
	notes = nil
	if err := FetchPinned(context.Background(), dir, []models.Artifact{art}, "", progress); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(final); string(got) != string(right) || len(notes) != 0 {
		t.Errorf("after the source was right: %d bytes, notes %q", len(got), notes)
	}
}
