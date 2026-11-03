package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// Characterization, inventory item 5: downloads, against a fake server. It pins
// today's behaviour; nothing touches the network.

type artifactServer struct {
	*httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	ranges []string // the Range header of each request ("" for none)
	auth   []string
	handle func(w http.ResponseWriter, r *http.Request, data []byte) bool // true: answered
}

func newArtifactServer(t *testing.T, files map[string][]byte) *artifactServer {
	t.Helper()
	s := &artifactServer{files: files}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.ranges = append(s.ranges, r.Header.Get("Range"))
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		data, ok := s.files[filepath.Base(r.URL.Path)]
		handle := s.handle
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		if handle != nil && handle(w, r, data) {
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(data)))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *artifactServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ranges)
}

func artifactOf(name string, data []byte) catalog.Artifact {
	sum := sha256.Sum256(data)
	return catalog.Artifact{SourcePath: name, Filename: name, ExpectedSizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Role: "primary"}
}

func specWith(url string, artifacts ...catalog.Artifact) *catalog.Spec {
	return &catalog.Spec{ModelID: "char-model", Kind: "stt", Source: url + "/org/repo", RepositoryRevision: "abc", Artifacts: artifacts}
}

func plenty(string) (int64, error) { return 1 << 40, nil }

func installTo(t *testing.T, dir string, s *catalog.Spec, report func(Event)) error {
	t.Helper()
	_, err := Model(context.Background(), s, dir, Options{Available: plenty, Report: report})
	return err
}

var modelBytes = []byte(strings.Repeat("model bytes ", 1000))

func TestDownloadInstallsAndVerifies(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	dir := filepath.Join(t.TempDir(), "models")
	var kinds []string
	report := func(e Event) {
		if len(kinds) == 0 || kinds[len(kinds)-1] != e.Kind {
			kinds = append(kinds, e.Kind)
		}
	}
	s := specWith(srv.URL, artifactOf("m.bin", modelBytes))
	if err := installTo(t, dir, s, report); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "m.bin"))
	if err != nil || string(got) != string(modelBytes) {
		t.Fatalf("installed: %d bytes, %v", len(got), err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "m.bin")); info.Mode().Perm() != 0o600 {
		t.Errorf("mode: %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "m.bin.partial")); err == nil {
		t.Error("the partial file stayed")
	}
	if want := "started,download-progress,verification-started,verification-completed,completed"; strings.Join(kinds, ",") != want {
		t.Errorf("events: %v", kinds)
	}
	// Reinstalling downloads nothing. The first check hashes the file again, because
	// the verified identity includes ctime and publishing the partial changed it;
	// after that the identity is trusted.
	before := srv.requests()
	how := ""
	report = func(e Event) {
		if e.Kind == "verification-completed" {
			how = e.Verification
		}
	}
	for _, want := range []string{"sha256", "cached-identity", "cached-identity"} {
		if err := installTo(t, dir, s, report); err != nil {
			t.Fatal(err)
		}
		if srv.requests() != before || how != want {
			t.Errorf("reinstalling: %d requests, %q, want %q", srv.requests()-before, how, want)
		}
	}
	// A change to the file, same size, is noticed.
	changed := append([]byte{}, modelBytes...)
	changed[0] ^= 1
	os.WriteFile(filepath.Join(dir, "m.bin"), changed, 0o600)
	if err := installTo(t, dir, s, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "m.bin")); string(got) != string(modelBytes) {
		t.Error("a changed file was kept")
	}
	if srv.requests() == before {
		t.Error("a changed file was not downloaded again")
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	bad := make([]byte, len(modelBytes))
	srv := newArtifactServer(t, map[string][]byte{"m.bin": bad})
	dir := filepath.Join(t.TempDir(), "models")
	err := installTo(t, dir, specWith(srv.URL, artifactOf("m.bin", modelBytes)), nil)
	if err == nil || !strings.Contains(err.Error(), "Checksum mismatch") {
		t.Fatalf("%v", err)
	}
	for _, name := range []string{"m.bin", "m.bin.partial"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s stayed after a bad checksum", name)
		}
	}
}

// A short body keeps what arrived, and the next install resumes from there.
func TestDownloadResumesAShortBody(t *testing.T) {
	half := len(modelBytes) / 2
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	first := true
	srv.handle = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		if !first {
			return false
		}
		first = false
		w.(http.Flusher).Flush() // no Content-Length, so the short body ends cleanly
		w.Write(data[:half])
		return true
	}
	dir := filepath.Join(t.TempDir(), "models")
	s := specWith(srv.URL, artifactOf("m.bin", modelBytes))
	err := installTo(t, dir, s, nil)
	if err == nil || !strings.Contains(err.Error(), "download ended at") {
		t.Fatalf("%v", err)
	}
	if n, ok := size(filepath.Join(dir, "m.bin.partial")); !ok || n != int64(half) {
		t.Fatalf("partial: %d %v", n, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.bin")); err == nil {
		t.Fatal("a short download was published")
	}
	if err := installTo(t, dir, s, nil); err != nil {
		t.Fatal(err)
	}
	if got := srv.ranges[len(srv.ranges)-1]; got != fmt.Sprintf("bytes=%d-", half) {
		t.Errorf("Range: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "m.bin")); string(got) != string(modelBytes) {
		t.Error("the resumed file is wrong")
	}
}

// A resume is only appended to when the server really resumed from that byte.
func TestDownloadRefusesABadResume(t *testing.T) {
	half := int64(len(modelBytes) / 2)
	total := int64(len(modelBytes))
	for name, c := range map[string]struct {
		answer func(w http.ResponseWriter, data []byte)
		want   string
	}{
		"ignores Range, sends all": {func(w http.ResponseWriter, data []byte) { w.Write(data) }, "Expected an HTTP 206"},
		"206 with no Content-Range": {func(w http.ResponseWriter, data []byte) {
			w.WriteHeader(206)
			w.Write(data[half:])
		}, "Expected an HTTP 206"},
		"another start": {func(w http.ResponseWriter, data []byte) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", total-1, total))
			w.WriteHeader(206)
			w.Write(data)
		}, "does not describe"},
		"another total": {func(w http.ResponseWriter, data []byte) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, total-1, total+1))
			w.WriteHeader(206)
			w.Write(data[half:])
		}, "does not describe"},
		"garbled": {func(w http.ResponseWriter, data []byte) {
			w.Header().Set("Content-Range", "bytes nonsense")
			w.WriteHeader(206)
		}, "Invalid Content-Range"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
			srv.handle = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
				c.answer(w, data)
				return true
			}
			dir := t.TempDir()
			partial := filepath.Join(dir, "m.bin.partial")
			os.WriteFile(partial, modelBytes[:half], 0o600)
			err := installTo(t, dir, specWith(srv.URL, artifactOf("m.bin", modelBytes)), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
			if got, _ := os.ReadFile(partial); string(got) != string(modelBytes[:half]) {
				t.Errorf("the partial changed: %d bytes", len(got))
			}
		})
	}
}

// An error page answered with 200 never becomes the model.
func TestDownloadHTMLErrorPage(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	srv.handle = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>please sign in</html>"))
		return true
	}
	dir := t.TempDir()
	err := installTo(t, dir, specWith(srv.URL, artifactOf("m.bin", modelBytes)), nil)
	if err == nil || !strings.Contains(err.Error(), "Content-Length") {
		t.Fatalf("%v", err)
	}
	for _, name := range []string{"m.bin", "m.bin.partial"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s exists", name)
		}
	}
}

// What is already on disk decides what is fetched.
func TestDownloadUsesWhatIsThere(t *testing.T) {
	half := len(modelBytes) / 2
	for name, c := range map[string]struct {
		final, partial []byte
		requests       int
		lastRange      string
	}{
		"a complete partial":           {nil, modelBytes, 0, ""},
		"a corrupt complete partial":   {nil, make([]byte, len(modelBytes)), 1, ""},
		"an oversized partial":         {nil, append(append([]byte{}, modelBytes...), 'x'), 1, ""},
		"a truncated final file":       {modelBytes[:half], nil, 1, fmt.Sprintf("bytes=%d-", half)},
		"a verified final file":        {modelBytes, nil, 0, ""},
		"an oversized final file":      {append(append([]byte{}, modelBytes...), 'x'), nil, 1, ""},
		"a truncated final, a partial": {modelBytes[:half/2], modelBytes[:half], 1, fmt.Sprintf("bytes=%d-", half)},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
			dir := t.TempDir()
			if c.final != nil {
				os.WriteFile(filepath.Join(dir, "m.bin"), c.final, 0o600)
			}
			if c.partial != nil {
				os.WriteFile(filepath.Join(dir, "m.bin.partial"), c.partial, 0o600)
			}
			if err := installTo(t, dir, specWith(srv.URL, artifactOf("m.bin", modelBytes)), nil); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "m.bin")); string(got) != string(modelBytes) {
				t.Errorf("installed %d bytes", len(got))
			}
			if _, err := os.Stat(filepath.Join(dir, "m.bin.partial")); err == nil {
				t.Error("the partial stayed")
			}
			if srv.requests() != c.requests {
				t.Errorf("requests: %d, ranges %v", srv.requests(), srv.ranges)
			}
			if c.requests > 0 && srv.ranges[len(srv.ranges)-1] != c.lastRange {
				t.Errorf("Range: %q", srv.ranges[len(srv.ranges)-1])
			}
		})
	}
}

// A failure on a later file keeps the earlier ones, so a retry does not refetch them.
func TestDownloadManyArtifacts(t *testing.T) {
	second := []byte("the second file's bytes")
	srv := newArtifactServer(t, map[string][]byte{"a.bin": modelBytes})
	dir := t.TempDir()
	a, b := artifactOf("a.bin", modelBytes), artifactOf("b.bin", second)
	b.Role = "supplementary"
	s := specWith(srv.URL, a, b)
	if err := installTo(t, dir, s, nil); err == nil {
		t.Fatal("the missing file should fail")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.bin")); string(got) != string(modelBytes) {
		t.Error("the first file was lost")
	}
	before := srv.requests()
	srv.mu.Lock()
	srv.files["b.bin"] = second
	srv.mu.Unlock()
	if err := installTo(t, dir, s, nil); err != nil {
		t.Fatal(err)
	}
	if srv.requests()-before != 1 {
		t.Errorf("the retry made %d requests", srv.requests()-before)
	}
}

type rewriteTransport struct {
	to   string
	seen []*http.Request
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = "http", r.to
	return http.DefaultTransport.RoundTrip(out)
}

// The token goes only to https://huggingface.co, and never into an error.
func TestDownloadTokenOnlyToHuggingFace(t *testing.T) {
	const token = "hf_secretTokenValue"
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	t.Setenv("HF_TOKEN", "")
	host := strings.TrimPrefix(srv.URL, "http://")
	run := func(source string) string {
		t.Helper()
		tr := &rewriteTransport{to: host}
		s := specWith(source, artifactOf("m.bin", modelBytes))
		s.Source = source
		if _, err := Model(context.Background(), s, t.TempDir(), Options{HFToken: token, Available: plenty, Client: &http.Client{Transport: tr}}); err != nil {
			t.Fatal(err)
		}
		return tr.seen[0].Header.Get("Authorization")
	}
	if got := run("https://huggingface.co/org/repo"); got != "Bearer "+token {
		t.Errorf("Hugging Face: %q", got)
	}
	for _, other := range []string{"https://example.com/org/repo", "http://huggingface.co/org/repo", "https://huggingface.co.evil.example/org/repo"} {
		if got := run(other); got != "" {
			t.Errorf("%s got the token: %q", other, got)
		}
	}
	srv.handle = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		w.WriteHeader(403)
		return true
	}
	tr := &rewriteTransport{to: host}
	_, err := Model(context.Background(), specWith("https://huggingface.co/org/repo", artifactOf("m.bin", modelBytes)), t.TempDir(),
		Options{HFToken: token, Available: plenty, Client: &http.Client{Transport: tr}})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the error: %v", err)
	}
}

func TestDownloadURL(t *testing.T) {
	a := catalog.Artifact{SourcePath: "/dir/my file.bin"}
	for source, want := range map[string]string{
		"https://huggingface.co/org/repo/": "https://huggingface.co/org/repo/resolve/rev/dir/my file.bin",
		"https://github.com/org/repo":      "https://raw.githubusercontent.com/org/repo/rev/dir/my%20file.bin",
	} {
		if got := DownloadURL(&catalog.Spec{Source: source, RepositoryRevision: "rev"}, a); got != want {
			t.Errorf("%s: %s", source, got)
		}
	}
	a.Source = []byte(`{"repositoryUrl":"https://huggingface.co/other/repo","revision":"pinned"}`)
	if got := DownloadURL(&catalog.Spec{Source: "https://huggingface.co/org/repo", RepositoryRevision: "rev"}, a); got != "https://huggingface.co/other/repo/resolve/pinned/dir/my file.bin" {
		t.Errorf("an artifact's own source: %s", got)
	}
}

func TestRequiredBytes(t *testing.T) {
	dir := t.TempDir()
	a, b := artifactOf("a.bin", modelBytes), artifactOf("b.bin", modelBytes)
	s := specWith("http://x", a, b)
	total := int64(len(modelBytes))
	if got := RequiredBytes(s, dir); got != 2*total {
		t.Errorf("nothing there: %d", got)
	}
	os.WriteFile(filepath.Join(dir, "a.bin"), modelBytes, 0o600)
	if got := RequiredBytes(s, dir); got != total {
		t.Errorf("one complete: %d", got)
	}
	os.WriteFile(filepath.Join(dir, "b.bin.partial"), modelBytes[:100], 0o600)
	if got := RequiredBytes(s, dir); got != total-100 {
		t.Errorf("a partial: %d", got)
	}
}

func TestInstallRefusesBeforeDownloading(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	noHash := artifactOf("m.bin", modelBytes)
	noHash.SHA256 = ""
	if err := installTo(t, t.TempDir(), specWith(srv.URL, noHash), nil); err == nil || !strings.Contains(err.Error(), "installation is disabled") {
		t.Errorf("no hash: %v", err)
	}
	noSize := artifactOf("m.bin", modelBytes)
	noSize.ExpectedSizeBytes = 0
	if err := installTo(t, t.TempDir(), specWith(srv.URL, noSize), nil); err == nil || !strings.Contains(err.Error(), "installation is disabled") {
		t.Errorf("no size: %v", err)
	}
	_, err := Model(context.Background(), specWith(srv.URL, artifactOf("m.bin", modelBytes)), t.TempDir(), Options{Available: func(string) (int64, error) { return 1, nil }})
	if err == nil || !strings.Contains(err.Error(), "Insufficient disk space") {
		t.Errorf("no space: %v", err)
	}
	if srv.requests() != 0 {
		t.Errorf("%d requests", srv.requests())
	}
}
