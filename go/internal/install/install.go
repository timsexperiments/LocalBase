// Package install downloads catalog model artifacts: resumable, size- and
// sha256-checked against the catalog, published by rename. It shares the
// `.checksums.json` continuity cache and its SQLite lock with the TS binary.
package install

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"

	_ "modernc.org/sqlite"
)

// Event reports install progress (TS ModelInstallEvent).
type Event struct {
	Kind          string // started | download-progress | verification-started | verification-completed | completed | failed
	ModelID       string
	ArtifactCount int
	Filename      string
	Index         int
	Downloaded    int64
	Total         int64
	Percent       int
	Verification  string // cached-identity | sha256
	Phase         string // preparing | downloading | verifying | publishing (failed)
	Err           error
}

// Options configure an install.
type Options struct {
	HFToken string
	Report  func(Event)
	Client  *http.Client
	// Available overrides the free-disk probe (tests).
	Available func(dir string) (int64, error)
}

// DownloadURL is where an artifact downloads from.
func DownloadURL(s *catalog.Spec, a catalog.Artifact) string {
	repo, rev := s.Source, s.RepositoryRevision
	if len(a.Source) > 0 {
		var src struct{ RepositoryURL, Revision string }
		if json.Unmarshal(a.Source, &src) == nil && src.RepositoryURL != "" {
			repo, rev = src.RepositoryURL, src.Revision
		}
	}
	base := strings.TrimSuffix(repo, "/")
	path := strings.TrimLeft(a.SourcePath, "/")
	if u, err := url.Parse(base); err == nil && u.Host == "github.com" {
		parts := strings.Split(path, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		return "https://raw.githubusercontent.com" + u.Path + "/" + rev + "/" + strings.Join(parts, "/")
	}
	return base + "/resolve/" + rev + "/" + path
}

// RequiredBytes is the disk an install still needs, crediting complete files
// and partial downloads.
func RequiredBytes(s *catalog.Spec, dir string) int64 {
	var total int64
	for _, a := range s.Artifacts {
		final := filepath.Join(dir, a.Filename)
		if st, err := os.Stat(final); err == nil && st.Size() == a.ExpectedSizeBytes {
			continue
		}
		remaining := a.ExpectedSizeBytes
		if st, err := os.Stat(final + ".partial"); err == nil {
			remaining = max(0, remaining-st.Size())
		}
		if st, err := os.Stat(final); err == nil && st.Size() < a.ExpectedSizeBytes {
			remaining = min(remaining, a.ExpectedSizeBytes-st.Size())
		}
		total += remaining
	}
	return total
}

// FreeBytes is the space available to an unprivileged user at the deepest
// existing ancestor of dir.
func FreeBytes(dir string) (int64, error) {
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

const gib = 1 << 30

// Model installs every artifact of s into dir and returns the primary path.
func Model(ctx context.Context, s *catalog.Spec, dir string, o Options) (string, error) {
	report := o.Report
	if report == nil {
		report = func(Event) {}
	}
	phase := "preparing"
	fail := func(err error) (string, error) {
		report(Event{Kind: "failed", ModelID: s.ModelID, Phase: phase, Err: err})
		return "", err
	}
	for _, a := range s.Artifacts {
		if a.ExpectedSizeBytes <= 0 || a.SHA256 == "" {
			return fail(fmt.Errorf("Managed model %s is missing authoritative size and SHA-256 metadata; installation is disabled until the catalog is repaired.", s.ModelID))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	avail := o.Available
	if avail == nil {
		avail = FreeBytes
	}
	if free, err := avail(dir); err == nil {
		if need := RequiredBytes(s, dir); free < need {
			return fail(fmt.Errorf("Insufficient disk space to install %q: requires %.2f GiB, %.2f GiB available at %s.",
				s.ModelID, float64(need)/gib, float64(free)/gib, dir))
		}
	}
	report(Event{Kind: "started", ModelID: s.ModelID, ArtifactCount: len(s.Artifacts)})
	for i, a := range s.Artifacts {
		if err := artifact(ctx, s, a, dir, i+1, o, report, &phase); err != nil {
			return fail(err)
		}
	}
	report(Event{Kind: "completed", ModelID: s.ModelID})
	return filepath.Join(dir, s.Primary().Filename), nil
}

func size(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

func artifact(ctx context.Context, s *catalog.Spec, a catalog.Artifact, dir string, index int, o Options,
	report func(Event), phase *string) error {
	out := filepath.Join(dir, a.Filename)
	partial := out + ".partial"
	ev := Event{ModelID: s.ModelID, Filename: a.Filename, Index: index, ArtifactCount: len(s.Artifacts)}
	verify := func(path string) error {
		*phase = "verifying"
		e := ev
		e.Kind = "verification-started"
		report(e)
		how, err := Verify(path, a, dir)
		if err != nil {
			return err
		}
		e.Kind, e.Verification = "verification-completed", how
		report(e)
		return nil
	}
	*phase = "preparing"
	if existing, ok := size(out); ok {
		if existing < a.ExpectedSizeBytes {
			// A truncated final file becomes the partial download to resume.
			if p, ok := size(partial); !ok {
				os.Rename(out, partial)
			} else if p > a.ExpectedSizeBytes || existing > p {
				os.Remove(partial)
				os.Rename(out, partial)
			} else {
				os.Remove(out)
			}
		} else {
			if verify(out) == nil {
				return nil
			}
			*phase = "preparing"
			os.Remove(out)
			os.Remove(partial)
		}
	}
	if p, ok := size(partial); ok {
		switch {
		case p > a.ExpectedSizeBytes:
			os.Remove(partial)
		case p == a.ExpectedSizeBytes:
			if verify(partial) == nil {
				*phase = "publishing"
				return os.Rename(partial, out)
			}
			*phase = "preparing"
			os.Remove(partial)
		}
	}
	*phase = "downloading"
	src := DownloadURL(s, a)
	token := o.HFToken
	if token == "" {
		token = os.Getenv("HF_TOKEN")
	}
	if err := download(ctx, src, partial, a.ExpectedSizeBytes, token, o.Client, func(n int64) {
		e := ev
		e.Kind, e.Downloaded, e.Total, e.Percent = "download-progress", n, a.ExpectedSizeBytes, int(n*100/a.ExpectedSizeBytes)
		report(e)
	}); err != nil {
		return downloadError(src, token, err)
	}
	if err := verify(partial); err != nil {
		os.Remove(partial)
		return err
	}
	*phase = "publishing"
	return os.Rename(partial, out)
}

func downloadError(src, token string, err error) error {
	low := strings.ToLower(src)
	if token == "" && (strings.Contains(low, "gemma") || strings.Contains(low, "llama")) {
		return fmt.Errorf("Failed to download model from %s.\n\nThis model is gated on Hugging Face. To download it:\n"+
			"1. Accept the model terms on Hugging Face (e.g., https://huggingface.co/google/gemma-3-27b-it)\n"+
			"2. Save your access token in the database: local-base configure --hf-token \"your_token\" (or set the HF_TOKEN environment variable)\n"+
			"3. Re-run the command.", src)
	}
	return fmt.Errorf("Failed to download model from %s: %w", src, err)
}

var contentRangeRe = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)

// download appends to partial from its current size, checking the response
// framing (200 for a new file, 206 with a matching Content-Range to resume).
func download(ctx context.Context, src, partial string, expected int64, token string, client *http.Client, progress func(int64)) error {
	if client == nil {
		client = http.DefaultClient
	}
	offset, _ := size(partial)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	if u, _ := url.Parse(src); token != "" && u != nil && u.Scheme == "https" && u.Host == "huggingface.co" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := client.Do(req)
	if err != nil {
		removeIfEmpty(partial)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		removeIfEmpty(partial)
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "))
	}
	if err := checkFraming(resp, offset, expected); err != nil {
		removeIfEmpty(partial)
		return err
	}
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n := offset
	buf := make([]byte, 1<<20)
	last := time.Time{}
	progress(n)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			n += int64(k)
			if n > expected {
				return fmt.Errorf("downloaded size exceeds authoritative size %d", expected)
			}
			if _, err := f.Write(buf[:k]); err != nil {
				return err
			}
			if time.Since(last) > 250*time.Millisecond || n == expected {
				progress(n)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if n != expected {
		return fmt.Errorf("download ended at %d bytes; expected %d", n, expected)
	}
	return nil
}

func checkFraming(resp *http.Response, offset, expected int64) error {
	var length int64 = -1
	if h := resp.Header.Get("Content-Length"); h != "" {
		v, err := strconv.ParseInt(h, 10, 64)
		if err != nil || v < 0 {
			return fmt.Errorf("Invalid Content-Length header %q", h)
		}
		length = v
	}
	cr := resp.Header.Get("Content-Range")
	if offset == 0 {
		if resp.StatusCode != 200 || cr != "" {
			return fmt.Errorf("Expected an HTTP 200 response without Content-Range for a new download, received %d", resp.StatusCode)
		}
		if length >= 0 && length != expected {
			return fmt.Errorf("Content-Length %d does not match authoritative size %d", length, expected)
		}
		return nil
	}
	if resp.StatusCode != 206 || cr == "" {
		return fmt.Errorf("Expected an HTTP 206 response with Content-Range for a resumed download, received %d", resp.StatusCode)
	}
	m := contentRangeRe.FindStringSubmatch(cr)
	if m == nil {
		return fmt.Errorf("Invalid Content-Range header %q", cr)
	}
	start, _ := strconv.ParseInt(m[1], 10, 64)
	end, _ := strconv.ParseInt(m[2], 10, 64)
	total, _ := strconv.ParseInt(m[3], 10, 64)
	if start != offset || end < start || end >= total || total != expected {
		return fmt.Errorf("Content-Range %s does not describe bytes %d-%d/%d", cr, offset, expected-1, expected)
	}
	if length >= 0 && length != end-start+1 {
		return fmt.Errorf("Content-Length %d does not match Content-Range size %d", length, end-start+1)
	}
	return nil
}

func removeIfEmpty(p string) {
	if n, ok := size(p); ok && n == 0 {
		os.Remove(p)
	}
}

// --- verification and the continuity cache ---------------------------------------

type identity struct {
	Size    int64   `json:"size"`
	MtimeMs float64 `json:"mtimeMs"`
	CtimeMs float64 `json:"ctimeMs"`
	Dev     uint64  `json:"dev"`
	Ino     uint64  `json:"ino"`
}

type cacheEntry struct {
	AuthoritativeSha256 string   `json:"authoritativeSha256"`
	ExpectedSizeBytes   int64    `json:"expectedSizeBytes"`
	File                identity `json:"file"`
}

type cacheFile struct {
	Version int                   `json:"version"`
	Entries map[string]cacheEntry `json:"entries"`
}

func fileIdentity(path string) (identity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return identity{}, err
	}
	m, c := statTimes(&st)
	return identity{Size: st.Size, MtimeMs: m, CtimeMs: c, Dev: uint64(st.Dev), Ino: st.Ino}, nil
}

func readCache(dir string) cacheFile {
	empty := cacheFile{Version: 1, Entries: map[string]cacheEntry{}}
	b, err := os.ReadFile(filepath.Join(dir, ".checksums.json"))
	if err != nil {
		return empty
	}
	var c cacheFile
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Entries == nil {
		return empty
	}
	return c
}

// Verify checks a file's size and sha256 against the catalog. A file whose
// identity (size, times, device, inode) matches a previous verification is
// trusted without rehashing ("cached-identity").
func Verify(path string, a catalog.Artifact, dir string) (string, error) {
	id, err := fileIdentity(path)
	if err != nil {
		return "", err
	}
	if id.Size != a.ExpectedSizeBytes {
		return "", fmt.Errorf("Size mismatch for %s: expected %d bytes, got %d bytes.", a.Filename, a.ExpectedSizeBytes, id.Size)
	}
	digest := strings.ToLower(a.SHA256)
	if e, ok := readCache(dir).Entries[a.Filename]; ok && strings.ToLower(e.AuthoritativeSha256) == digest &&
		e.ExpectedSizeBytes == a.ExpectedSizeBytes && e.File == id {
		return "cached-identity", nil
	}
	actual, err := SHA256File(path)
	if err != nil {
		return "", err
	}
	if actual != digest {
		return "", fmt.Errorf("Checksum mismatch for %s!\n  Expected: %s\n  Got:      %s\n  File may be corrupted or tampered with. Delete it and retry.",
			a.Filename, digest, actual)
	}
	err = withCacheLock(dir, func() error {
		c := readCache(dir)
		c.Entries[a.Filename] = cacheEntry{AuthoritativeSha256: digest, ExpectedSizeBytes: a.ExpectedSizeBytes, File: id}
		b, _ := json.MarshalIndent(c, "", "  ")
		tmp := filepath.Join(dir, fmt.Sprintf(".checksums.json.%d.%s.tmp", os.Getpid(), store.UUID()))
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, ".checksums.json")); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	})
	return "sha256", err
}

// SHA256File streams a file through sha256.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// withCacheLock holds the same exclusive SQLite lock TS uses on
// `.checksums.json.lock.db`; the kernel releases it if the holder dies.
func withCacheLock(dir string, fn func() error) error {
	path := filepath.Join(dir, ".checksums.json.lock.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	os.Chmod(path, 0o600)
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE")
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") && !strings.Contains(err.Error(), "database is locked") {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("Timed out waiting for the checksum store lock at " + path + "; another LocalBase process is holding it.")
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	return fn()
}
