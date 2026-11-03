// Package upgrade finds, verifies and installs a release of this binary (DESIGN §33). The
// check sends nothing about this computer. A release is only installed if a compiled-in key
// signed its checksums, and nothing is written over the running binary: it is replaced by a
// rename, and the old file is kept for rollback.
package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAPI is the one request the check makes.
	DefaultAPI = "https://api.github.com/repos/timsexperiments/LocalBase/releases/latest"
	// DefaultFiles is where a release's files are, under the tag.
	DefaultFiles = "https://github.com/timsexperiments/LocalBase/releases/download"

	// PreviousSuffix names the kept binary, beside the installed one.
	PreviousSuffix = ".previous"

	maxChecksumBytes  = 64 << 10
	maxSignatureBytes = 8 << 10
	maxArchiveBytes   = 512 << 20
	checkTimeout      = 10 * time.Second
)

// maxBinaryBytes bounds what is taken out of an archive. A variable so a test need not build
// half a gigabyte to reach it.
var maxBinaryBytes int64 = 512 << 20

// --- the keys --------------------------------------------------------------------------

// Key is a public key that can vouch for a release. A revoked key stays listed, so it is
// plain that it no longer counts.
type Key struct {
	ID      string
	Public  string // base64 Ed25519
	Revoked bool
}

// ReleaseKeys is the gate: "signature verification required". It is empty until the owner
// creates a release key, and until then this build refuses to install anything (DESIGN §33.2).
var ReleaseKeys = []Key{}

// testKey is set only with -ldflags in test builds, as "id:base64". The release build sets no
// such thing, and scripts/release/build.sh is checked for that.
var testKey string

// TrustedKeys is what this build will accept.
func TrustedKeys() []Key {
	keys := append([]Key{}, ReleaseKeys...)
	if id, pub, ok := strings.Cut(testKey, ":"); ok {
		keys = append(keys, Key{ID: id, Public: pub})
	}
	return keys
}

// Ready says whether any listed key can verify a release.
func Ready(keys []Key) bool {
	for _, k := range keys {
		if !k.Revoked {
			return true
		}
	}
	return false
}

// GateError says this build cannot verify a release, so it will not install one.
type GateError struct{}

func (*GateError) Error() string {
	return "signature verification required: this build has no release signing key, so it cannot verify a release and will not install one. `local-base upgrade --check` still works. To update now, use the installer: scripts/install.sh"
}

// VerifySignature checks a signature file over body. It has one "<key id> <base64>" line per
// signing key, and any listed, non-revoked key is enough. It returns the key that verified.
func VerifySignature(body, sig []byte, keys []Key) (string, error) {
	for _, line := range strings.Split(string(sig), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil || len(raw) != ed25519.SignatureSize {
			continue
		}
		for _, k := range keys {
			if k.ID != fields[0] || k.Revoked {
				continue
			}
			pub, err := base64.StdEncoding.DecodeString(k.Public)
			if err != nil || len(pub) != ed25519.PublicKeySize {
				continue
			}
			if ed25519.Verify(ed25519.PublicKey(pub), body, raw) {
				return k.ID, nil
			}
		}
	}
	return "", errors.New("the release's signature did not verify against any trusted key")
}

// FindChecksum reads one file's hash from a checksums.txt that has already been verified.
func FindChecksum(checksums []byte, name string) (string, error) {
	hexHash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && hexHash.MatchString(fields[0]) {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no checksum for %s", name)
}

// --- the assets ------------------------------------------------------------------------

func assetTarget(goos, goarch string) string {
	osName := map[string]string{"darwin": "macos", "linux": "linux"}[goos]
	archName := map[string]string{"arm64": "arm64", "amd64": "x64"}[goarch]
	if osName == "" || archName == "" {
		return ""
	}
	return osName + "-" + archName
}

// AssetBinary is the binary's name inside a release archive, or "" if there is no such target.
func AssetBinary(goos, goarch string) string {
	if t := assetTarget(goos, goarch); t != "" {
		return "local-base-" + t
	}
	return ""
}

// AssetArchive is the archive's name: zip on macOS, because notarization needs it.
func AssetArchive(goos, goarch string) string {
	b := AssetBinary(goos, goarch)
	switch {
	case b == "":
		return ""
	case goos == "darwin":
		return b + ".zip"
	}
	return b + ".tar.gz"
}

// --- reaching a release ----------------------------------------------------------------

// Source says where releases are. The defaults are GitHub's; the installer's
// LOCALBASE_RELEASE_URL also works, for tests and mirrors, and signatures still apply.
type Source struct {
	API, Files string
	Client     *http.Client
}

// DefaultSource reads the environment's overrides.
func DefaultSource() Source {
	s := Source{API: DefaultAPI, Files: DefaultFiles, Client: NewClient()}
	if v := os.Getenv("LOCALBASE_RELEASE_URL"); v != "" {
		s.Files = strings.TrimSuffix(v, "/")
	}
	if v := os.Getenv("LOCALBASE_RELEASE_API_URL"); v != "" {
		s.API = v
	}
	return s
}

// allowedURL: HTTPS, or plain HTTP only to this computer.
func allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// NewClient refuses to be redirected anywhere plain, and asks for no compression, so the
// check's headers are exactly what the contract says.
func NewClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !allowedURL(req.URL.String()) {
			return fmt.Errorf("refused a redirect to %s", req.URL.Scheme+"://"+req.URL.Host)
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

// Latest is what the check found. NotModified: the stored answer still stands.
type Latest struct {
	Tag, URL, ETag string
	NotModified    bool
}

// RateLimited is GitHub saying not now.
type RateLimited struct{ Reset time.Time }

func (e *RateLimited) Error() string {
	if e.Reset.IsZero() {
		return "GitHub's rate limit was reached. Try again later"
	}
	return "GitHub's rate limit was reached. It resets at " + e.Reset.Local().Format(time.RFC1123)
}

// Latest makes the one request the check makes. It sends Accept and User-Agent, and the
// stored ETag when there is one: no version, query, cookie or token.
func (s Source) Latest(ctx context.Context, etag string) (*Latest, error) {
	if !allowedURL(s.API) {
		return nil, fmt.Errorf("refused to check %q: it must be HTTPS", s.API)
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.API, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "local-base")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return &Latest{NotModified: true, ETag: etag}, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		limited := &RateLimited{}
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			limited.Reset = time.Unix(n, 0)
		}
		return nil, limited
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, errors.New("GitHub's answer could not be read")
	}
	return &Latest{Tag: body.Tag, URL: body.URL, ETag: resp.Header.Get("ETag")}, nil
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return NewClient()
}

func (s Source) fetchBytes(ctx context.Context, raw string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.fetch(ctx, raw, &buf, limit); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s Source) fetch(ctx context.Context, raw string, out io.Writer, limit int64) error {
	if !allowedURL(raw) {
		return fmt.Errorf("refused to download from %q: it must be HTTPS", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "local-base")
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("could not download %s: %w", path.Base(raw), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not download %s: HTTP %d", path.Base(raw), resp.StatusCode)
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("could not download %s: %w", path.Base(raw), err)
	}
	if n > limit {
		return fmt.Errorf("%s is over the size limit", path.Base(raw))
	}
	return nil
}

// --- comparing versions ----------------------------------------------------------------

var (
	tagRe     = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
)

func parts(re *regexp.Regexp, s string) ([3]int, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Comparison is what a tag means for the running version. Known is false for anything that is
// not vX.Y.Z (a runtime package's tag, for one), or a current version that is not a release.
type Comparison struct{ Newer, Known bool }

// Compare looks at the tag on this computer. Neither value is sent anywhere.
func Compare(current, tag string) Comparison {
	cur, ok := parts(versionRe, current)
	got, ok2 := parts(tagRe, tag)
	if !ok || !ok2 {
		return Comparison{}
	}
	return Comparison{Newer: less(cur, got), Known: true}
}

// DecideVersion says whether to go from current to target. A downgrade needs asking for by
// name, so a replayed old release cannot walk someone back, even a properly signed one.
func DecideVersion(current, target string, allowDowngrade bool) (proceed bool, kind string) {
	cur, ok := parts(versionRe, current)
	got, ok2 := parts(versionRe, target)
	switch {
	case !ok || !ok2:
		return false, "unknown"
	case cur == got:
		return false, "same"
	case less(got, cur) && !allowDowngrade:
		return false, "downgrade"
	}
	return true, ""
}

// --- preparing a candidate ---------------------------------------------------------------

// Candidate is a verified binary, beside the installed one, ready to swap in.
type Candidate struct {
	Path, Dir, Version, SHA256, KeyID string
}

// Remove deletes the working directory.
func (c *Candidate) Remove() { os.RemoveAll(c.Dir) }

// PrepareOptions says what to prepare, and where. Dir is the installed binary's directory,
// so the swap is a rename on one filesystem.
type PrepareOptions struct {
	Tag, GOOS, GOARCH, Dir string
	Keys                   []Key
	// Run runs a file with --version and returns what it says.
	Run func(path string) (string, error)
}

// RunVersion runs a file with --version, and returns what it says.
func RunVersion(path string) (string, error) { return runVersion(path) }

func runVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Prepare downloads and verifies a release, and leaves nothing behind if it refuses. The order
// matters: the signature over checksums.txt is verified before anything in it is read, and
// the archive against that hash before it is opened.
func Prepare(ctx context.Context, s Source, o PrepareOptions) (*Candidate, error) {
	if !Ready(o.Keys) {
		return nil, &GateError{}
	}
	archive, binary := AssetArchive(o.GOOS, o.GOARCH), AssetBinary(o.GOOS, o.GOARCH)
	if archive == "" {
		return nil, fmt.Errorf("there is no release for %s/%s", o.GOOS, o.GOARCH)
	}
	want, ok := parts(tagRe, o.Tag)
	if !ok {
		return nil, fmt.Errorf("%q is not a release version", o.Tag)
	}
	base := s.Files + "/" + o.Tag + "/"

	checksums, err := s.fetchBytes(ctx, base+"checksums.txt", maxChecksumBytes)
	if err != nil {
		return nil, err
	}
	sig, err := s.fetchBytes(ctx, base+"checksums.txt.sig", maxSignatureBytes)
	if err != nil {
		return nil, fmt.Errorf("the release has no signature to verify: %w", err)
	}
	keyID, err := VerifySignature(checksums, sig, o.Keys)
	if err != nil {
		return nil, err
	}
	wantSum, err := FindChecksum(checksums, archive)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(o.Dir, ".local-base-upgrade-")
	if err != nil {
		return nil, fmt.Errorf("could not write beside the binary: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	c, err := func() (*Candidate, error) {
		archivePath := filepath.Join(dir, archive)
		f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		err = s.fetch(ctx, base+archive, io.MultiWriter(f, h), maxArchiveBytes)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
			return nil, fmt.Errorf("%s does not match its checksum", archive)
		}
		out := filepath.Join(dir, "local-base.new")
		if strings.HasSuffix(archive, ".zip") {
			err = extractZip(archivePath, binary, out)
		} else {
			err = extractTarGz(archivePath, binary, out)
		}
		if err != nil {
			return nil, err
		}
		sum, err := fileSHA256(out)
		if err != nil {
			return nil, err
		}
		reported, err := o.Run(out)
		if err != nil {
			return nil, fmt.Errorf("the downloaded binary did not run: %w", err)
		}
		got, ok := parts(versionRe, strings.TrimSpace(reported))
		if !ok || got != want {
			return nil, fmt.Errorf("the downloaded binary reports %q, not %s", reported, o.Tag)
		}
		os.Remove(archivePath)
		return &Candidate{Path: out, Dir: dir, Version: strings.TrimPrefix(o.Tag, "v"), SHA256: sum, KeyID: keyID}, nil
	}()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return c, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveName is an entry's plain name, or "" for anything with a directory in it, so
// a path cannot reach out of the working directory: only one exact name is ever taken.
func archiveName(name string) string {
	clean := strings.TrimPrefix(name, "./")
	if clean == "" || strings.ContainsAny(clean, `/\`) {
		return ""
	}
	return clean
}

func writeBinary(out string, r io.Reader) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBinaryBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxBinaryBytes {
		return errors.New("the binary is over the size limit")
	}
	return os.Chmod(out, 0o755)
}

func extractTarGz(archive, name, out string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the archive could not be read: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive does not hold %s", name)
		}
		if err != nil {
			return fmt.Errorf("the archive could not be read: %w", err)
		}
		if archiveName(h.Name) != name {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s in the archive is not a regular file", name)
		}
		return writeBinary(out, tr)
	}
}

func extractZip(archive, name, out string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("the archive could not be read: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if archiveName(f.Name) != name {
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("%s in the archive is not a regular file", name)
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		return writeBinary(out, r)
	}
	return fmt.Errorf("the archive does not hold %s", name)
}

// --- applying ---------------------------------------------------------------------------

// Ops are what happens to the gateway around a swap. They are empty for a root with no
// gateway: then only the binary changes.
type Ops struct {
	Stop  func() error // stops the gateway cleanly, and confirms it and its backends are gone
	Start func() error // restarts through the binary at the installed path, and waits for readiness
	Smoke func() error
}

func call(f func() error) error {
	if f == nil {
		return nil
	}
	return f()
}

// Result says how an Apply ended, as well as its error.
type Result struct{ RolledBack bool }

// SmokeFailed: the new version is running, and smoke did not pass. It can fail for reasons of
// its own, so this does not roll back.
type SmokeFailed struct{ Err error }

func (e *SmokeFailed) Error() string {
	return "the new version is running, but smoke failed: " + e.Err.Error()
}
func (e *SmokeFailed) Unwrap() error { return e.Err }

// keepPrevious keeps the installed file as <binary>.previous: a hard link and a rename, so
// the old inode is never touched. A copy if the filesystem has no links.
func keepPrevious(binary string) error {
	previous := binary + PreviousSuffix
	tmp := previous + ".tmp"
	os.Remove(tmp)
	if err := os.Link(binary, tmp); err != nil {
		if err := copyFile(binary, tmp); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, previous); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(to)
	}
	return err
}

// swapBack puts <binary>.previous in place, and keeps what was there as .previous, so it can
// be undone. Both steps are renames.
func swapBack(binary string) error {
	previous := binary + PreviousSuffix
	tmp := binary + ".rollback.tmp"
	os.Remove(tmp)
	if err := os.Link(binary, tmp); err != nil {
		if err := copyFile(binary, tmp); err != nil {
			return err
		}
	}
	if err := os.Rename(previous, binary); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, previous)
}

// Apply replaces the installed binary. Anything before the swap that fails brings the old
// version back. A new version that does not come up is rolled back automatically.
func Apply(c *Candidate, binary string, ops Ops) (Result, error) {
	defer c.Remove()
	if err := call(ops.Stop); err != nil {
		// It may be partly down: bring the old version back, and say both.
		if serr := call(ops.Start); serr != nil {
			return Result{}, fmt.Errorf("could not stop the gateway: %w. The old version did not come back either: %v", err, serr)
		}
		return Result{}, fmt.Errorf("could not stop the gateway, so nothing was changed: %w", err)
	}
	restore := func(err error) (Result, error) {
		if serr := call(ops.Start); serr != nil {
			return Result{}, fmt.Errorf("%w. The old version did not come back either: %v", err, serr)
		}
		return Result{}, err
	}
	// What is swapped in is what was verified.
	sum, err := fileSHA256(c.Path)
	if err != nil {
		return restore(fmt.Errorf("could not read the new binary: %w", err))
	}
	if sum != c.SHA256 {
		return restore(errors.New("the new binary changed after it was verified, so nothing was changed"))
	}
	if err := keepPrevious(binary); err != nil {
		return restore(fmt.Errorf("could not keep the previous binary: %w", err))
	}
	if err := os.Rename(c.Path, binary); err != nil {
		return restore(fmt.Errorf("could not replace the binary: %w", err))
	}
	if err := call(ops.Start); err != nil {
		res := Result{RolledBack: true}
		if rerr := swapBack(binary); rerr != nil {
			return res, fmt.Errorf("the new version did not start (%v), and rolling back failed: %w", err, rerr)
		}
		if serr := call(ops.Start); serr != nil {
			return res, fmt.Errorf("the new version did not start (%w), so it was rolled back, but the old version did not come back either: %v", err, serr)
		}
		return res, fmt.Errorf("the new version did not start, so it was rolled back: %w", err)
	}
	if err := call(ops.Smoke); err != nil {
		return Result{}, &SmokeFailed{err}
	}
	return Result{}, nil
}

// Rollback swaps <binary>.previous back. It can be undone the same way. If the older version
// will not come up (a database newer than it knows, say), the newer one goes back.
func Rollback(binary string, ops Ops) error {
	if _, err := os.Stat(binary + PreviousSuffix); err != nil {
		return errors.New("there is no previous version to go back to")
	}
	if err := call(ops.Stop); err != nil {
		if serr := call(ops.Start); serr != nil {
			return fmt.Errorf("could not stop the gateway: %w. It did not come back either: %v", err, serr)
		}
		return fmt.Errorf("could not stop the gateway, so nothing was changed: %w", err)
	}
	if err := swapBack(binary); err != nil {
		return err
	}
	if err := call(ops.Start); err != nil {
		if rerr := swapBack(binary); rerr != nil {
			return fmt.Errorf("the previous version did not start (%v), and going forward again failed: %w", err, rerr)
		}
		if serr := call(ops.Start); serr != nil {
			return fmt.Errorf("the previous version did not start (%w), and neither did this one: %v", err, serr)
		}
		return fmt.Errorf("the previous version did not start, so this version is back: %w", err)
	}
	return call(ops.Smoke)
}
