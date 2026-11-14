package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a fake release -------------------------------------------------------------------

type testKeyPair struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t *testing.T, id string) testKeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKeyPair{id, pub, priv}
}

func (k testKeyPair) listed() Key {
	return Key{ID: k.id, Public: base64.StdEncoding.EncodeToString(k.pub)}
}

func (k testKeyPair) sign(b []byte) string {
	return k.id + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, b)) + "\n"
}

// fakeBinary is a script standing in for local-base: it prints a version.
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo " + version + "; exit 0; fi\nexit 0\n")
}

func targz(t *testing.T, name string, body []byte, extra ...func(*tar.Writer)) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "./LICENSE", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("x\n"))
	for _, e := range extra {
		e(tw)
	}
	if name != "" {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func zipped(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(body)
	zw.Close()
	return b.Bytes()
}

// release builds what a release publishes, signed by key, for linux-x64.
type release struct {
	files map[string][]byte
}

func newRelease(t *testing.T, key testKeyPair, version string) *release {
	t.Helper()
	name := AssetBinary("linux", "amd64")
	archive := targz(t, name, fakeBinary(version))
	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + AssetArchive("linux", "amd64") + "\n")
	return &release{map[string][]byte{
		AssetArchive("linux", "amd64"): archive,
		"checksums.txt":                checksums,
		"checksums.txt.sig":            []byte(key.sign(checksums)),
	}}
}

func serve(t *testing.T, tag string, r *release) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked = append(asked, req.URL.Path)
		mu.Unlock()
		name := strings.TrimPrefix(req.URL.Path, "/"+tag+"/")
		b, ok := r.files[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func prepare(t *testing.T, srv *httptest.Server, keys []Key, tag string) (*Candidate, error) {
	t.Helper()
	dir := t.TempDir()
	return Prepare(context.Background(), Source{Files: srv.URL, Client: srv.Client()}, PrepareOptions{Tag: tag, GOOS: "linux", GOARCH: "amd64", Dir: dir, Keys: keys, Run: runScript})
}

// runScript runs the fake binary. Scripts, not real binaries, so this runs anywhere.
func runScript(path string) (string, error) { return runVersion(path) }

// --- signatures -----------------------------------------------------------------------

func TestSignatureGate(t *testing.T) {
	if len(ReleaseKeys) != 0 {
		t.Skip("a real key is listed: the gate is open by design")
	}
	if Ready(nil) {
		t.Error("no key in the list: nothing can be verified")
	}
	k := newKey(t, "k1")
	r := newRelease(t, k, "1.2.3")
	srv, asked := serve(t, "v1.2.3", r)
	_, err := prepare(t, srv, nil, "v1.2.3")
	var gate *GateError
	if !errors.As(err, &gate) {
		t.Fatalf("with no key: %v", err)
	}
	if !strings.Contains(err.Error(), "signature verification required") {
		t.Errorf("the message should say what is required: %v", err)
	}
	if len(*asked) != 0 {
		t.Errorf("it downloaded with no way to verify: %v", *asked)
	}
}

func TestKeyList(t *testing.T) {
	a, b, c := newKey(t, "a"), newKey(t, "b"), newKey(t, "c")
	body := []byte("the checksums\n")
	revokedA := a.listed()
	revokedA.Revoked = true
	for name, tc := range map[string]struct {
		keys []Key
		sig  string
		ok   bool
		used string
	}{
		"a listed key":                           {[]Key{a.listed()}, a.sign(body), true, "a"},
		"either of two":                          {[]Key{a.listed(), b.listed()}, b.sign(body), true, "b"},
		"two signatures, one known":              {[]Key{b.listed()}, a.sign(body) + b.sign(body), true, "b"},
		"revoked, though the signature is valid": {[]Key{revokedA}, a.sign(body), false, ""},
		"revoked and a good one":                 {[]Key{revokedA, b.listed()}, a.sign(body) + b.sign(body), true, "b"},
		"an unknown key id":                      {[]Key{a.listed()}, c.sign(body), false, ""},
		"a listed id, signed by another":         {[]Key{a.listed()}, "a " + strings.Fields(c.sign(body))[1] + "\n", false, ""},
		"no signature":                           {[]Key{a.listed()}, "", false, ""},
		"garbage":                                {[]Key{a.listed()}, "not a signature\n", false, ""},
		"bad base64":                             {[]Key{a.listed()}, "a !!!\n", false, ""},
	} {
		used, err := VerifySignature(body, []byte(tc.sig), tc.keys)
		if (err == nil) != tc.ok || used != tc.used {
			t.Errorf("%s: %q, %v", name, used, err)
		}
	}
	if _, err := VerifySignature(append(body, 'x'), []byte(a.sign(body)), []Key{a.listed()}); err == nil {
		t.Error("a changed file passed")
	}
}

// A release key must be well formed, or the binary is wrong at build time.
func TestListedKeysAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range ReleaseKeys {
		b, err := base64.StdEncoding.DecodeString(k.Public)
		if err != nil || len(b) != ed25519.PublicKeySize || k.ID == "" || seen[k.ID] {
			t.Errorf("key %q", k.ID)
		}
		seen[k.ID] = true
	}
}

// The signature comes before anything in checksums.txt is trusted.
func TestSignatureBeforeChecksums(t *testing.T) {
	k := newKey(t, "k1")
	r := newRelease(t, k, "1.2.3")
	// A file that would parse, and points at a tampered archive, with no valid signature.
	bad := targz(t, AssetBinary("linux", "amd64"), fakeBinary("6.6.6"))
	sum := sha256.Sum256(bad)
	r.files[AssetArchive("linux", "amd64")] = bad
	r.files["checksums.txt"] = []byte(hex.EncodeToString(sum[:]) + "  " + AssetArchive("linux", "amd64") + "\n")
	srv, asked := serve(t, "v1.2.3", r)
	_, err := prepare(t, srv, []Key{k.listed()}, "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a tampered checksums.txt: %v", err)
	}
	if slices.Contains(*asked, "/v1.2.3/"+AssetArchive("linux", "amd64")) {
		t.Error("it fetched the archive before the signature passed")
	}
}

func TestParseChecksums(t *testing.T) {
	good := strings.Repeat("a", 64)
	got, err := FindChecksum([]byte(good+"  one.tar.gz\n"+strings.Repeat("b", 64)+" *two.zip\n"), "two.zip")
	if err != nil || got != strings.Repeat("b", 64) {
		t.Errorf("%q %v", got, err)
	}
	for _, bad := range []string{"", good + "  other\n", "short  one.tar.gz\n", strings.Repeat("z", 64) + "  one.tar.gz\n"} {
		if _, err := FindChecksum([]byte(bad), "one.tar.gz"); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

// --- preparing ------------------------------------------------------------------------

func TestPrepare(t *testing.T) {
	k := newKey(t, "k1")
	r := newRelease(t, k, "1.2.3")
	srv, _ := serve(t, "v1.2.3", r)
	c, err := prepare(t, srv, []Key{k.listed()}, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "1.2.3" || c.KeyID != "k1" || c.SHA256 == "" {
		t.Errorf("%+v", c)
	}
	info, err := os.Stat(c.Path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("binary: %v %v", info, err)
	}
	d, err := os.Stat(filepath.Dir(c.Path))
	if err != nil || d.Mode().Perm() != 0o700 {
		t.Errorf("the working directory is private: %v %v", d, err)
	}
}

func TestPrepareRefuses(t *testing.T) {
	k, other := newKey(t, "k1"), newKey(t, "other")
	binary := AssetBinary("linux", "amd64")
	archive := AssetArchive("linux", "amd64")
	resign := func(r *release, arch []byte) {
		sum := sha256.Sum256(arch)
		r.files[archive] = arch
		r.files["checksums.txt"] = []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
		r.files["checksums.txt.sig"] = []byte(k.sign(r.files["checksums.txt"]))
	}
	for name, tc := range map[string]struct {
		change func(*release)
		want   string
	}{
		"a wrong hash":            {func(r *release) { r.files[archive] = append(r.files[archive], 0) }, "checksum"},
		"no signature":            {func(r *release) { delete(r.files, "checksums.txt.sig") }, "signature"},
		"another key's signature": {func(r *release) { r.files["checksums.txt.sig"] = []byte(other.sign(r.files["checksums.txt"])) }, "signature"},
		"no checksum for this platform": {func(r *release) {
			r.files["checksums.txt"] = []byte(strings.Repeat("a", 64) + "  other.tar.gz\n")
			r.files["checksums.txt.sig"] = []byte(k.sign(r.files["checksums.txt"]))
		}, "checksum"},
		"the wrong version inside": {func(r *release) { resign(r, targz(t, binary, fakeBinary("9.9.9"))) }, "reports"},
		"no binary inside":         {func(r *release) { resign(r, targz(t, "", nil)) }, "does not hold"},
		"a symlink as the binary": {func(r *release) {
			resign(r, targz(t, "", nil, func(tw *tar.Writer) {
				tw.WriteHeader(&tar.Header{Name: "./" + binary, Typeflag: tar.TypeSymlink, Linkname: "/bin/sh", Mode: 0o755})
			}))
		}, "regular"},
		"a path trying to escape": {func(r *release) {
			resign(r, targz(t, "", nil, func(tw *tar.Writer) {
				tw.WriteHeader(&tar.Header{Name: "../../" + binary, Typeflag: tar.TypeReg, Mode: 0o755, Size: 2})
				tw.Write([]byte("x\n"))
			}))
		}, "does not hold"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, k, "1.2.3")
			tc.change(r)
			srv, _ := serve(t, "v1.2.3", r)
			dir := t.TempDir()
			_, err := Prepare(context.Background(), Source{Files: srv.URL, Client: srv.Client()},
				PrepareOptions{Tag: "v1.2.3", GOOS: "linux", GOARCH: "amd64", Dir: dir, Keys: []Key{k.listed()}, Run: runScript})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
			// It leaves nothing behind.
			left, _ := os.ReadDir(dir)
			if len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

func TestPrepareRefusesAnOversizedBinary(t *testing.T) {
	k := newKey(t, "k1")
	archive := AssetArchive("linux", "amd64")
	body := targz(t, AssetBinary("linux", "amd64"), bytes.Repeat([]byte("x"), 2048))
	sum := sha256.Sum256(body)
	checks := []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	srv, _ := serve(t, "v1.2.3", &release{map[string][]byte{archive: body, "checksums.txt": checks, "checksums.txt.sig": []byte(k.sign(checks))}})
	dir := t.TempDir()
	_, err := Prepare(context.Background(), Source{Files: srv.URL, Client: srv.Client()},
		PrepareOptions{Tag: "v1.2.3", GOOS: "linux", GOARCH: "amd64", Dir: dir, Keys: []Key{k.listed()}, Run: runScript, maxBinary: 1024})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("%v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestPrepareZip(t *testing.T) {
	k := newKey(t, "k1")
	binary, archive := AssetBinary("darwin", "arm64"), AssetArchive("darwin", "arm64")
	if !strings.HasSuffix(archive, ".zip") {
		t.Fatalf("macOS archives are zip: %s", archive)
	}
	body := zipped(t, binary, fakeBinary("1.2.3"))
	sum := sha256.Sum256(body)
	checks := []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	r := &release{map[string][]byte{archive: body, "checksums.txt": checks, "checksums.txt.sig": []byte(k.sign(checks))}}
	srv, _ := serve(t, "v1.2.3", r)
	c, err := Prepare(context.Background(), Source{Files: srv.URL, Client: srv.Client()},
		PrepareOptions{Tag: "v1.2.3", GOOS: "darwin", GOARCH: "arm64", Dir: t.TempDir(), Keys: []Key{k.listed()}, Run: runScript})
	if err != nil || c.Version != "1.2.3" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestDownloadLimitsAndRedirects(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 2048)) }))
	defer big.Close()
	if _, err := (Source{Files: big.URL, Client: big.Client()}).fetchBytes(context.Background(), big.URL+"/x", 1024); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("over the limit: %v", err)
	}
	// A public address must be HTTPS. Plain HTTP is for loopback, for tests, only.
	for raw, ok := range map[string]bool{"https://github.com/x": true, "http://127.0.0.1:9/x": true, "http://localhost:9/x": true,
		"http://github.com/x": false, "http://192.168.1.5/x": false, "ftp://github.com/x": false, "": false} {
		if got := allowedURL(raw); got != ok {
			t.Errorf("%q: %v", raw, got)
		}
	}
	// A redirect to plain HTTP elsewhere is refused.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://downgrade.example/file", http.StatusFound)
	}))
	defer redirect.Close()
	client := NewClient()
	if _, err := (Source{Files: redirect.URL, Client: client}).fetchBytes(context.Background(), redirect.URL+"/x", 1024); err == nil {
		t.Error("a redirect to plain HTTP was followed")
	}
}

// --- the check ------------------------------------------------------------------------

func TestUpgradeCheckSendsNothing(t *testing.T) {
	t.Setenv("HF_TOKEN", "planted-hf")
	t.Setenv("GITHUB_TOKEN", "planted-gh")
	t.Setenv("GH_TOKEN", "planted-gh2")
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, `{"tag_name":"v1.2.3","html_url":"https://example.test/v1.2.3"}`)
	}))
	defer srv.Close()
	s := Source{API: srv.URL + "/latest", Client: NewClient()}
	if _, err := s.Latest(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got.Method != "GET" || got.URL.Path != "/latest" || got.URL.RawQuery != "" {
		t.Errorf("%s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	var names []string
	for k := range got.Header {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Accept", "User-Agent"}) {
		t.Errorf("headers %v: exactly Accept and User-Agent, nothing else", got.Header)
	}
	if ua := got.Header.Get("User-Agent"); ua != "local-base" {
		t.Errorf("User-Agent %q: no version", ua)
	}
	if strings.Contains(fmt.Sprint(got.Header), "planted") {
		t.Errorf("a token was sent: %v", got.Header)
	}
}

func TestDefaultCheckURL(t *testing.T) {
	if DefaultAPI != "https://api.github.com/repos/timsexperiments/LocalBase/releases/latest" {
		t.Errorf("the contract names this URL: %s", DefaultAPI)
	}
}

func TestUpgradeCheckCompares(t *testing.T) {
	for _, tc := range []struct {
		current, tag string
		newer, known bool
	}{
		{"0.2.0", "v0.3.0", true, true},
		{"0.2.0", "v0.2.1", true, true},
		{"0.2.0", "v1.0.0", true, true},
		{"0.10.0", "v0.9.0", false, true},
		{"0.2.0", "v0.2.0", false, true},
		{"0.3.0", "v0.2.0", false, true},
		{"0.2.0", "kokoro-v0.0.2", false, false}, // a runtime package's tag
		{"0.2.0", "v1.2", false, false},
		{"0.2.0", "v1.2.3-rc1", false, false},
		{"0.2.0", "", false, false},
		{"dev", "v1.0.0", false, false},
	} {
		got := Compare(tc.current, tc.tag)
		if got.Newer != tc.newer || got.Known != tc.known {
			t.Errorf("%s against %s: %+v", tc.current, tc.tag, got)
		}
	}
}

func TestLatestAnswers(t *testing.T) {
	status, body := 200, `{"tag_name":"v1.2.3","html_url":"https://example.test/r"}`
	headers := map[string]string{"ETag": `"abc"`}
	var sentETag string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentETag = r.Header.Get("If-None-Match")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	s := Source{API: srv.URL, Client: NewClient()}
	got, err := s.Latest(context.Background(), "")
	if err != nil || got.Tag != "v1.2.3" || got.ETag != `"abc"` || got.URL != "https://example.test/r" || sentETag != "" {
		t.Errorf("%+v %v, etag %q", got, err, sentETag)
	}
	status, body = 304, ""
	got, err = s.Latest(context.Background(), `"abc"`)
	if err != nil || !got.NotModified || sentETag != `"abc"` {
		t.Errorf("304: %+v %v", got, err)
	}
	// A rate limit says when it resets, and is a plain error.
	status, body = 403, `{"message":"API rate limit exceeded"}`
	reset := time.Now().Add(time.Hour).Unix()
	headers["X-RateLimit-Remaining"] = "0"
	headers["X-RateLimit-Reset"] = fmt.Sprint(reset)
	_, err = s.Latest(context.Background(), "")
	var limited *RateLimited
	if !errors.As(err, &limited) || limited.Reset.Unix() != reset {
		t.Errorf("a rate limit: %v", err)
	}
	status, body = 429, ""
	if _, err = s.Latest(context.Background(), ""); !errors.As(err, &limited) {
		t.Errorf("429: %v", err)
	}
	delete(headers, "X-RateLimit-Remaining")
	delete(headers, "X-RateLimit-Reset")
	status, body = 500, "boom"
	if _, err = s.Latest(context.Background(), ""); err == nil || errors.As(err, &limited) {
		t.Errorf("500: %v", err)
	}
	status, body = 200, "not json"
	if _, err = s.Latest(context.Background(), ""); err == nil {
		t.Error("not JSON was accepted")
	}
}

// --- applying --------------------------------------------------------------------------

type script struct {
	calls               []string
	failStop, failStart error
	failSmoke           error
	startFailures       int
}

func (s *script) ops() Ops {
	return Ops{
		Stop: func() error { s.calls = append(s.calls, "stop"); return s.failStop },
		Start: func() error {
			s.calls = append(s.calls, "start")
			if s.startFailures > 0 {
				s.startFailures--
				return s.failStart
			}
			return nil
		},
		Smoke: func() error { s.calls = append(s.calls, "smoke"); return s.failSmoke },
	}
}

func installed(t *testing.T, content string) (binary string) {
	t.Helper()
	binary = filepath.Join(t.TempDir(), "local-base")
	if err := os.WriteFile(binary, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

func candidateFor(t *testing.T, binary, content string) *Candidate {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Dir(binary), ".local-base-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "new")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return &Candidate{Path: path, Dir: dir, Version: "1.2.3", SHA256: hex.EncodeToString(sum[:])}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplySwapOrder(t *testing.T) {
	binary := installed(t, "old")
	before, _ := os.Stat(binary)
	s := &script{}
	c := candidateFor(t, binary, "new")
	res, err := Apply(c, binary, s.ops())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.calls, []string{"stop", "start", "smoke"}) {
		t.Errorf("order %v", s.calls)
	}
	if res.RolledBack {
		t.Error("rolled back")
	}
	if read(t, binary) != "new" || read(t, binary+PreviousSuffix) != "old" {
		t.Errorf("binary %q, previous %q", read(t, binary), read(t, binary+PreviousSuffix))
	}
	// Never written in place: the path is a new inode, and the old inode is the previous binary.
	now, _ := os.Stat(binary)
	prev, _ := os.Stat(binary + PreviousSuffix)
	if os.SameFile(before, now) {
		t.Error("the binary was written in place")
	}
	if !os.SameFile(before, prev) {
		t.Error("the previous binary is not the old file")
	}
	if _, err := os.Stat(c.Dir); !os.IsNotExist(err) {
		t.Errorf("the working directory is left behind: %v", err)
	}
}

func TestApplyStopFailure(t *testing.T) {
	binary := installed(t, "old")
	s := &script{failStop: errors.New("a backend survived")}
	c := candidateFor(t, binary, "new")
	_, err := Apply(c, binary, s.ops())
	if err == nil || !strings.Contains(err.Error(), "survived") {
		t.Fatalf("%v", err)
	}
	if read(t, binary) != "old" {
		t.Error("it swapped after a failed stop")
	}
	if _, err := os.Stat(binary + PreviousSuffix); !os.IsNotExist(err) {
		t.Error("it kept a previous binary without swapping")
	}
	if !slices.Equal(s.calls, []string{"stop", "start"}) {
		t.Errorf("the old version should come back: %v", s.calls)
	}
}

func TestApplyRollsBackOnFailedStart(t *testing.T) {
	binary := installed(t, "old")
	s := &script{failStart: errors.New("not ready"), startFailures: 1}
	c := candidateFor(t, binary, "new")
	res, err := Apply(c, binary, s.ops())
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
	if !res.RolledBack {
		t.Error("should say it rolled back")
	}
	if read(t, binary) != "old" {
		t.Errorf("binary %q", read(t, binary))
	}
	if !slices.Equal(s.calls, []string{"stop", "start", "start"}) {
		t.Errorf("calls %v: no smoke, and the old version started", s.calls)
	}
}

func TestApplyRollbackThatAlsoFailsToStart(t *testing.T) {
	binary := installed(t, "old")
	s := &script{failStart: errors.New("not ready"), startFailures: 2}
	res, err := Apply(candidateFor(t, binary, "new"), binary, s.ops())
	if err == nil || !res.RolledBack || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, binary) != "old" {
		t.Errorf("binary %q", read(t, binary))
	}
}

func TestApplySmokeFailureKeepsNewVersion(t *testing.T) {
	binary := installed(t, "old")
	s := &script{failSmoke: errors.New("no model installed")}
	res, err := Apply(candidateFor(t, binary, "new"), binary, s.ops())
	if err == nil || res.RolledBack {
		t.Fatalf("%+v %v", res, err)
	}
	var smoke *SmokeFailed
	if !errors.As(err, &smoke) {
		t.Errorf("%v", err)
	}
	if read(t, binary) != "new" {
		t.Error("a failed smoke rolled back")
	}
}

// Changed between verifying and swapping: the checksum is checked again at the last moment.
func TestApplyRefusesAChangedCandidate(t *testing.T) {
	binary := installed(t, "old")
	c := candidateFor(t, binary, "new")
	if err := os.WriteFile(c.Path, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &script{}
	_, err := Apply(c, binary, s.ops())
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("%v", err)
	}
	if read(t, binary) != "old" {
		t.Error("it swapped in a changed binary")
	}
	if !slices.Equal(s.calls, []string{"stop", "start"}) {
		t.Errorf("the old version should come back: %v", s.calls)
	}
}

func TestRollback(t *testing.T) {
	binary := installed(t, "old")
	s := &script{}
	if _, err := Apply(candidateFor(t, binary, "new"), binary, s.ops()); err != nil {
		t.Fatal(err)
	}
	s = &script{}
	if err := Rollback(binary, s.ops()); err != nil {
		t.Fatal(err)
	}
	if read(t, binary) != "old" || read(t, binary+PreviousSuffix) != "new" {
		t.Errorf("binary %q, previous %q", read(t, binary), read(t, binary+PreviousSuffix))
	}
	if !slices.Equal(s.calls, []string{"stop", "start", "smoke"}) {
		t.Errorf("calls %v", s.calls)
	}
	// It can be undone the same way.
	s = &script{}
	if err := Rollback(binary, s.ops()); err != nil {
		t.Fatal(err)
	}
	if read(t, binary) != "new" {
		t.Errorf("after undoing: %q", read(t, binary))
	}
	none := installed(t, "only")
	s = &script{}
	err := Rollback(none, s.ops())
	if err == nil || !strings.Contains(err.Error(), "no previous") || len(s.calls) != 0 {
		t.Errorf("%v, calls %v", err, s.calls)
	}
	// If the older version won't come up, the newer one goes back, and it says so.
	binary = installed(t, "old")
	s = &script{}
	if _, err := Apply(candidateFor(t, binary, "new"), binary, s.ops()); err != nil {
		t.Fatal(err)
	}
	s = &script{failStart: errors.New("refuses this database"), startFailures: 1}
	err = Rollback(binary, s.ops())
	if err == nil || !strings.Contains(err.Error(), "refuses this database") || read(t, binary) != "new" {
		t.Errorf("%v, binary %q", err, read(t, binary))
	}
}

func TestDowngradeDecision(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		allow, proceed  bool
		kind            string
	}{
		{"0.2.0", "0.3.0", false, true, ""},
		{"0.2.0", "0.2.0", false, false, "same"},
		{"0.3.0", "0.2.0", false, false, "downgrade"},
		{"0.3.0", "0.2.0", true, true, ""},
		{"0.2.0", "0.2.0", true, false, "same"},
	} {
		proceed, kind := DecideVersion(tc.current, tc.target, tc.allow)
		if proceed != tc.proceed || kind != tc.kind {
			t.Errorf("%+v: %v %q", tc, proceed, kind)
		}
	}
}

func TestAssets(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, binary, archive string }{
		{"darwin", "arm64", "local-base-macos-arm64", "local-base-macos-arm64.zip"},
		{"darwin", "amd64", "local-base-macos-x64", "local-base-macos-x64.zip"},
		{"linux", "amd64", "local-base-linux-x64", "local-base-linux-x64.tar.gz"},
		{"linux", "arm64", "local-base-linux-arm64", "local-base-linux-arm64.tar.gz"},
	} {
		if got := AssetBinary(tc.goos, tc.goarch); got != tc.binary {
			t.Errorf("binary %s", got)
		}
		if got := AssetArchive(tc.goos, tc.goarch); got != tc.archive {
			t.Errorf("archive %s", got)
		}
	}
	if AssetBinary("windows", "amd64") != "" {
		t.Error("windows is not a release target")
	}
}

// A rollback lets go of the newer version's records only once the previous version is in place and
// running. If it can't get there, they stay, since the newer version is still the one in use.
func TestRollbackRunsOnPreviousOnlyOnceThePreviousVersionIsIn(t *testing.T) {
	setUp := func() (string, *script) {
		binary := installed(t, "old")
		if _, err := Apply(candidateFor(t, binary, "new"), binary, (&script{}).ops()); err != nil {
			t.Fatal(err)
		}
		return binary, &script{}
	}
	withPrevious := func(s *script) Ops {
		ops := s.ops()
		ops.OnPrevious = func() error { s.calls = append(s.calls, "previous"); return nil }
		return ops
	}
	binary, s := setUp()
	if err := Rollback(binary, withPrevious(s)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.calls, []string{"stop", "start", "previous", "smoke"}) {
		t.Errorf("calls %v", s.calls)
	}
	binary, s = setUp()
	s.failStop = errors.New("will not stop")
	if err := Rollback(binary, withPrevious(s)); err == nil || slices.Contains(s.calls, "previous") || read(t, binary) != "new" {
		t.Errorf("a gateway that wouldn't stop: %v, calls %v", err, s.calls)
	}
	binary, s = setUp()
	s.failStart, s.startFailures = errors.New("refuses this database"), 1
	if err := Rollback(binary, withPrevious(s)); err == nil || slices.Contains(s.calls, "previous") || read(t, binary) != "new" {
		t.Errorf("a previous version that didn't start: %v, calls %v", err, s.calls)
	}
}
