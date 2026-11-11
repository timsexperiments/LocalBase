package system

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

const (
	modulePath = "github.com/timsexperiments/LocalBase/go/internal"
	testKeyID  = "contract-test"
)

// upgradeKit is a throwaway key and real binaries built with it. Scratch only: nothing here
// touches a real service, ~/.local/bin or GitHub.
type upgradeKit struct {
	priv ed25519.PrivateKey
	pub  string
	dir  string
	bins sync.Map
}

var (
	kitOnce sync.Once
	kit     *upgradeKit
	kitErr  error
)

func theKit(t *testing.T) *upgradeKit {
	t.Helper()
	kitOnce.Do(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			kitErr = err
			return
		}
		dir, err := fsguard.ScratchTemp("lbgo-upgrade-")
		if err != nil {
			kitErr = err
			return
		}
		kit = &upgradeKit{priv: priv, pub: base64.StdEncoding.EncodeToString(pub), dir: dir}
	})
	if kitErr != nil {
		t.Fatal(kitErr)
	}
	return kit
}

// build compiles a stamped binary once per version and kind.
func (k *upgradeKit) build(t *testing.T, ver string, withKey bool) string {
	t.Helper()
	name := fmt.Sprintf("local-base-%s-%v", ver, withKey)
	if p, ok := k.bins.Load(name); ok {
		return p.(string)
	}
	out := filepath.Join(k.dir, name)
	flags := "-X " + modulePath + "/version.Version=" + ver + " -X " + modulePath + "/version.Commit=contracttest"
	if withKey {
		flags += " -X " + modulePath + "/upgrade.testKey=" + testKeyID + ":" + k.pub
	}
	cmd := exec.Command("go", "build", "-ldflags", flags, "-o", out, "github.com/timsexperiments/LocalBase/go/cmd/localbase")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	k.bins.Store(name, out)
	return out
}

func (k *upgradeKit) signature(id string, priv ed25519.PrivateKey, b []byte) []byte {
	return []byte(id + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, b)) + "\n")
}

// releaseFiles is what a release publishes for this platform.
func (k *upgradeKit) releaseFiles(t *testing.T, binary string) map[string][]byte {
	t.Helper()
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	name, archive := upgrade.AssetBinary(goruntime.GOOS, goruntime.GOARCH), upgrade.AssetArchive(goruntime.GOOS, goruntime.GOARCH)
	var b bytes.Buffer
	if strings.HasSuffix(archive, ".zip") {
		zw := zip.NewWriter(&b)
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write(body)
		tw.Close()
		gz.Close()
	}
	sum := sha256.Sum256(b.Bytes())
	checks := []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	return map[string][]byte{archive: b.Bytes(), "checksums.txt": checks, "checksums.txt.sig": k.signature(testKeyID, k.priv, checks)}
}

// fakeReleases answers the check, and serves files by tag.
type fakeReleases struct {
	latest string
	tags   map[string]map[string][]byte
	srv    *httptest.Server
}

func newFakeReleases(t *testing.T, latest string) *fakeReleases {
	t.Helper()
	f := &fakeReleases{latest: latest, tags: map[string]map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			json.NewEncoder(w).Encode(map[string]string{"tag_name": f.latest, "html_url": "https://example.test/" + f.latest})
			return
		}
		tag, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/files/"), "/")
		b, ok := f.tags[tag][name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReleases) env() []string {
	return []string{"LOCALBASE_RELEASE_URL=" + f.srv.URL + "/files", "LOCALBASE_RELEASE_API_URL=" + f.srv.URL + "/latest"}
}

// installBinary places a binary in a scratch "bin" directory, as an installer would.
func installBinary(t *testing.T, from string) string {
	t.Helper()
	dir := filepath.Join(fsguard.ScratchDir(t), "bin")
	if err := fsguard.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "local-base")
	if err := fsguard.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return to
}

func runBinary(t *testing.T, bin string, env []string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(cleanEnv(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return cliResult{code: code, stdout: out.String(), stderr: errb.String()}
}

func versionOf(t *testing.T, bin string) string {
	t.Helper()
	res := runBinary(t, bin, nil, "--version")
	if res.code != 0 {
		t.Fatalf("%s --version: %d %s", bin, res.code, res.stderr)
	}
	return strings.TrimSpace(res.stdout)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// From a real old binary to a real new one, and back, with no gateway on the root.
func TestUpgradeEndToEnd(t *testing.T) {
	k := theKit(t)
	old, next := k.build(t, "0.2.0", true), k.build(t, "9.9.9", true)
	rel := newFakeReleases(t, "v9.9.9")
	rel.tags["v9.9.9"] = k.releaseFiles(t, next)
	bin := installBinary(t, old)
	rootDir := filepath.Join(fsguard.ScratchDir(t), "root")
	before, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}

	res := runBinary(t, bin, rel.env(), "--root", rootDir, "--json", "upgrade", "--yes")
	if res.code != 0 {
		t.Fatalf("upgrade: %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, `"signedBy":"`+testKeyID+`"`) {
		t.Errorf("result: %s", res.stdout)
	}
	if got := versionOf(t, bin); got != "9.9.9" {
		t.Errorf("after upgrade: %s", got)
	}
	previous := bin + upgrade.PreviousSuffix
	if got := versionOf(t, previous); got != "0.2.0" {
		t.Errorf("the previous binary: %s", got)
	}
	// Never written in place.
	now, _ := os.Stat(bin)
	kept, _ := os.Stat(previous)
	if os.SameFile(before, now) || !os.SameFile(before, kept) {
		t.Error("the binary was written in place")
	}
	if got := dirNames(t, filepath.Dir(bin)); len(got) != 2 {
		t.Errorf("left behind: %v", got)
	}
	human := runBinary(t, bin, rel.env(), "--root", rootDir, "upgrade", "--yes")
	if human.code != 0 || !strings.Contains(human.stdout, "up to date") {
		t.Errorf("already there: %d %s %s", human.code, human.stdout, human.stderr)
	}

	// One line back, and it can be undone.
	res = runBinary(t, bin, rel.env(), "--root", rootDir, "upgrade", "rollback")
	if res.code != 0 {
		t.Fatalf("rollback: %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if got := versionOf(t, bin); got != "0.2.0" {
		t.Errorf("after rollback: %s", got)
	}
	if got := versionOf(t, previous); got != "9.9.9" {
		t.Errorf("what was replaced: %s", got)
	}
}

func TestUpgradeRollbackWithNothingToGoBackTo(t *testing.T) {
	k := theKit(t)
	bin := installBinary(t, k.build(t, "0.2.0", true))
	res := runBinary(t, bin, nil, "--root", filepath.Join(fsguard.ScratchDir(t), "root"), "upgrade", "rollback")
	if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "no previous version") {
		t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
	}
}

// --check enables nothing and writes nothing: not even the root.
func TestUpgradeCheck(t *testing.T) {
	k := theKit(t)
	rel := newFakeReleases(t, "v9.9.9")
	bin := installBinary(t, k.build(t, "0.2.0", true))
	rootDir := filepath.Join(fsguard.ScratchDir(t), "root-never-made")
	res := runBinary(t, bin, rel.env(), "--root", rootDir, "--json", "upgrade", "--check")
	if res.code != 0 || !strings.Contains(res.stdout, `"newer":true`) || !strings.Contains(res.stdout, `"latest":"v9.9.9"`) {
		t.Errorf("newer: %d %s %s", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Errorf("--check made the root: %v", err)
	}
	if got := dirNames(t, filepath.Dir(bin)); len(got) != 1 {
		t.Errorf("--check changed the install: %v", got)
	}
	rel.latest = "v0.2.0"
	if res := runBinary(t, bin, rel.env(), "upgrade", "--check"); res.code != 0 || !strings.Contains(res.stdout, "up to date") {
		t.Errorf("the same: %d %s", res.code, res.stdout)
	}
	// A runtime package's tag is no information, and not an update.
	rel.latest = "kokoro-v0.0.2"
	if res := runBinary(t, bin, rel.env(), "--json", "upgrade", "--check"); res.code != 0 || !strings.Contains(res.stdout, `"known":false`) || !strings.Contains(res.stdout, `"newer":false`) {
		t.Errorf("a runtime tag: %d %s", res.code, res.stdout)
	}
	// A failure says so.
	broken := []string{"LOCALBASE_RELEASE_API_URL=http://127.0.0.1:1/latest"}
	if res := runBinary(t, bin, broken, "upgrade", "--check"); res.code != 1 || !strings.Contains(res.stderr+res.stdout, "could not reach") {
		t.Errorf("no network: %d %s %s", res.code, res.stdout, res.stderr)
	}
}

// The gate: with no key, a build will not install anything, and says why. --check still works.
func TestSignatureGateRealBinary(t *testing.T) {
	k := theKit(t)
	rel := newFakeReleases(t, "v9.9.9")
	rel.tags["v9.9.9"] = k.releaseFiles(t, k.build(t, "9.9.9", true))
	bin := installBinary(t, k.build(t, "0.2.0", false))
	root := filepath.Join(fsguard.ScratchDir(t), "root")
	res := runBinary(t, bin, rel.env(), "--root", root, "upgrade", "--yes")
	if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "signature verification required") {
		t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
	}
	if got := versionOf(t, bin); got != "0.2.0" {
		t.Errorf("it changed: %s", got)
	}
	if got := dirNames(t, filepath.Dir(bin)); len(got) != 1 {
		t.Errorf("left behind: %v", got)
	}
	if res := runBinary(t, bin, rel.env(), "upgrade", "--check"); res.code != 0 || !strings.Contains(res.stdout, "newer version") {
		t.Errorf("--check should still work: %d %s %s", res.code, res.stdout, res.stderr)
	}
}

func TestUpgradeRefusesBadReleasesRealBinary(t *testing.T) {
	k := theKit(t)
	old, next := k.build(t, "0.2.0", true), k.build(t, "9.9.9", true)
	archive := upgrade.AssetArchive(goruntime.GOOS, goruntime.GOARCH)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string][]byte){
		"no signature": func(f map[string][]byte) { delete(f, "checksums.txt.sig") },
		"signed by another key": func(f map[string][]byte) {
			f["checksums.txt.sig"] = k.signature(testKeyID, otherPriv, f["checksums.txt"])
		},
		"tampered checksums": func(f map[string][]byte) { f["checksums.txt"] = append(f["checksums.txt"], '\n') },
		"tampered archive":   func(f map[string][]byte) { f[archive] = append(f[archive], 0) },
	} {
		t.Run(name, func(t *testing.T) {
			rel := newFakeReleases(t, "v9.9.9")
			files := k.releaseFiles(t, next)
			change(files)
			rel.tags["v9.9.9"] = files
			bin := installBinary(t, old)
			res := runBinary(t, bin, rel.env(), "--root", filepath.Join(fsguard.ScratchDir(t), "root"), "upgrade", "--yes")
			if res.code != 1 {
				t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
			}
			if got := versionOf(t, bin); got != "0.2.0" {
				t.Errorf("it changed: %s", got)
			}
			if got := dirNames(t, filepath.Dir(bin)); len(got) != 1 {
				t.Errorf("left behind: %v", got)
			}
		})
	}
	// A release whose binary lies about its version.
	rel := newFakeReleases(t, "v9.9.9")
	rel.tags["v9.9.9"] = k.releaseFiles(t, k.build(t, "8.8.8", true))
	bin := installBinary(t, old)
	res := runBinary(t, bin, rel.env(), "--root", filepath.Join(fsguard.ScratchDir(t), "root"), "upgrade", "--yes")
	if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "reports") || versionOf(t, bin) != "0.2.0" {
		t.Errorf("the wrong version: %d %s %s", res.code, res.stdout, res.stderr)
	}
}

func TestUpgradeDowngrade(t *testing.T) {
	k := theKit(t)
	old, next := k.build(t, "0.2.0", true), k.build(t, "9.9.9", true)
	rel := newFakeReleases(t, "v0.2.0")
	rel.tags["v0.2.0"] = k.releaseFiles(t, old)
	bin := installBinary(t, next)
	root := filepath.Join(fsguard.ScratchDir(t), "root")
	for _, args := range [][]string{{"upgrade", "--yes"}, {"upgrade", "--yes", "--version", "v0.2.0"}} {
		res := runBinary(t, bin, rel.env(), append([]string{"--root", root}, args...)...)
		if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "--allow-downgrade") {
			t.Errorf("%v: %d %s %s", args, res.code, res.stdout, res.stderr)
		}
		if got := versionOf(t, bin); got != "9.9.9" {
			t.Errorf("%v: it changed: %s", args, got)
		}
	}
	res := runBinary(t, bin, rel.env(), "--root", root, "upgrade", "--yes", "--allow-downgrade", "--version", "v0.2.0")
	if res.code != 0 || versionOf(t, bin) != "0.2.0" {
		t.Errorf("asked for by name: %d %s %s", res.code, res.stdout, res.stderr)
	}
}

func TestUpgradeInputs(t *testing.T) {
	k := theKit(t)
	rel := newFakeReleases(t, "v9.9.9")
	bin := installBinary(t, k.build(t, "0.2.0", true))
	root := filepath.Join(fsguard.ScratchDir(t), "root")
	for name, args := range map[string][]string{
		"not a version":   {"upgrade", "--yes", "--version", "latest"},
		"too long a wait": {"upgrade", "--yes", "--wait", "601"},
		"a negative wait": {"upgrade", "--yes", "--wait", "-1"},
		"wait and force":  {"upgrade", "--yes", "--wait", "5", "--force"},
	} {
		if res := runBinary(t, bin, rel.env(), append([]string{"--root", root}, args...)...); res.code != 2 {
			t.Errorf("%s: %d %s %s", name, res.code, res.stdout, res.stderr)
		}
	}
	// Without --yes, and not at a terminal, it does not guess.
	rel.tags["v9.9.9"] = k.releaseFiles(t, k.build(t, "9.9.9", true))
	res := runBinary(t, bin, rel.env(), "--root", root, "upgrade")
	if res.code != 2 || !strings.Contains(res.stderr, "--yes") || versionOf(t, bin) != "0.2.0" {
		t.Errorf("no --yes: %d %s %s", res.code, res.stdout, res.stderr)
	}
	if got := dirNames(t, filepath.Dir(bin)); len(got) != 1 {
		t.Errorf("left behind: %v", got)
	}
}

// A development build has nothing to upgrade from.
func TestUpgradeDevelopmentBuild(t *testing.T) {
	bin := installBinary(t, goCLI(t))
	res := runBinary(t, bin, nil, "--root", filepath.Join(fsguard.ScratchDir(t), "root"), "upgrade", "--yes")
	if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "development build") {
		t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
	}
	if res := runBinary(t, bin, nil, "--json", "upgrade", "--check"); res.code != 0 || !strings.Contains(res.stdout, `"release":false`) {
		t.Errorf("--check: %d %s %s", res.code, res.stdout, res.stderr)
	}
}

// A gateway in the foreground: this cannot restart what it did not start.
func TestUpgradeRefusesForegroundGateway(t *testing.T) {
	var target *harness.Target
	for _, c := range harness.Targets(t) {
		if c.Name == "go" {
			target = c
		}
	}
	if target == nil || target.Root == "" {
		t.Skip("no Go gateway launched")
	}
	k := theKit(t)
	rel := newFakeReleases(t, "v9.9.9")
	rel.tags["v9.9.9"] = k.releaseFiles(t, k.build(t, "9.9.9", true))
	bin := installBinary(t, k.build(t, "0.2.0", true))
	res := runBinary(t, bin, rel.env(), "--root", target.Root, "upgrade", "--yes")
	if res.code != 1 || !strings.Contains(res.stderr+res.stdout, "foreground") {
		t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
	}
	if got := versionOf(t, bin); got != "0.2.0" {
		t.Errorf("it changed: %s", got)
	}
	if resp, err := http.Get(target.BaseURL + "/health"); err != nil {
		t.Errorf("the gateway should still be there: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// The daily check's switch goes through a running gateway, or to the database with none.
func TestUpgradeCheckUpdatesToggle(t *testing.T) {
	rootDir := newUsageRoot(t)
	res := runCLI(t, "go", rootDir, "--json", "upgrade", "check-updates", "on")
	if res.code != 0 || !strings.Contains(res.stdout, `"via":"database"`) {
		t.Errorf("no gateway: %d %s %s", res.code, res.stdout, res.stderr)
	}
	if res := runCLI(t, "go", rootDir, "upgrade", "check-updates", "maybe"); res.code != 2 {
		t.Errorf("not on or off: %d", res.code)
	}
	var target *harness.Target
	for _, c := range harness.Targets(t) {
		if c.Name == "go" {
			target = c
		}
	}
	if target == nil || target.Root == "" {
		t.Skip("no Go gateway launched")
	}
	res = runCLI(t, "go", target.Root, "--json", "upgrade", "check-updates", "on")
	if res.code != 0 || !strings.Contains(res.stdout, `"via":"gateway"`) {
		t.Fatalf("through the gateway: %d %s %s", res.code, res.stdout, res.stderr)
	}
	req, _ := http.NewRequest("GET", target.BaseURL+"/_localbase/update", nil)
	req.Header.Set("Authorization", "Bearer "+target.Admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"enabled":true`) {
		t.Errorf("as the gateway sees it: %s", body)
	}
	// Leave the shared gateway as it was.
	if res := runCLI(t, "go", target.Root, "--json", "upgrade", "check-updates", "off"); res.code != 0 {
		t.Errorf("off: %d %s", res.code, res.stderr)
	}
}

// An upgrade and a rollback each leave a record: which versions, the file's short checksum, how it
// ended. A token in a download URL, or the path, stays out of every log.
func TestUpgradeIsAudited(t *testing.T) {
	k := theKit(t)
	old, next := k.build(t, "0.2.0", true), k.build(t, "9.9.9", true)
	rel := newFakeReleases(t, "v9.9.9")
	rel.tags["v9.9.9"] = k.releaseFiles(t, next)
	bin := installBinary(t, old)
	rootDir := filepath.Join(fsguard.ScratchDir(t), "root")
	const token = "PLANTED_download_token_4e71a0"
	host := strings.TrimPrefix(rel.srv.URL, "http://")
	env := []string{"LOCALBASE_RELEASE_URL=http://mirror:" + token + "@" + host + "/files",
		"LOCALBASE_RELEASE_API_URL=" + rel.srv.URL + "/latest?token=" + token}
	if res := runBinary(t, bin, env, "--root", rootDir, "upgrade", "--yes"); res.code != 0 {
		t.Fatalf("upgrade: %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if res := runBinary(t, bin, env, "--root", rootDir, "upgrade", "rollback"); res.code != 0 {
		t.Fatalf("rollback: %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	audit, err := os.ReadFile(filepath.Join(rootDir, "logs", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		var ev struct {
			EventName  string         `json:"eventName"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if strings.HasPrefix(ev.EventName, "audit.upgrade-") {
			// Only these: the sink redacts a URL's secrets, but that's a second line of defence.
			for k := range ev.Attributes {
				if !slices.Contains([]string{"action", "actor_kind", "actor_id", "from_version", "to_version", "build", "outcome"}, k) {
					t.Errorf("an upgrade line has %q: %s", k, line)
				}
			}
			ev.Attributes["event"] = ev.EventName
			seen = append(seen, ev.Attributes)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("%d upgrade lines: %v", len(seen), seen)
	}
	up, back := seen[0], seen[1]
	if up["event"] != "audit.upgrade-applied" || up["from_version"] != "0.2.0" || up["to_version"] != "9.9.9" || up["outcome"] != "applied" || up["actor_kind"] != "cli" {
		t.Errorf("%v", up)
	}
	if b, _ := up["build"].(string); len(b) != 12 {
		t.Errorf("build %q", b)
	}
	if back["event"] != "audit.upgrade-rolled-back" || back["to_version"] != "0.2.0" || back["from_version"] != "9.9.9" || back["outcome"] != "rolled-back" {
		t.Errorf("%v", back)
	}
	logs, _ := filepath.Glob(filepath.Join(rootDir, "logs", "*"))
	for _, m := range logs {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), token) {
			t.Errorf("%s holds the download token", m)
		}
	}
	if strings.Contains(string(audit), filepath.Dir(bin)) {
		t.Error("the audit file holds the install path")
	}
}
