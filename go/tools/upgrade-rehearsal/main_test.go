package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

func TestRefusals(t *testing.T) {
	live := "/Users/someone/.local/share/local-base"
	base := identity{work: "/private/tmp/lb-upgrade-rehearsal", root: "/private/tmp/lb-upgrade-rehearsal/root", label: "scratch",
		liveRoots: []string{live}, liveLabels: []string{"live"}, home: "/Users/someone"}
	if got := refusals(base); len(got) != 0 {
		t.Fatalf("a scratch identity is refused: %v", got)
	}
	for _, tc := range []struct {
		name string
		edit func(*identity)
		want string
	}{
		{"the root is the live root", func(i *identity) { i.root = live }, "overlaps the live root"},
		{"the work directory is the live root", func(i *identity) { i.work = live }, "overlaps the live root"},
		{"the root is inside the live root", func(i *identity) { i.work, i.root = live+"/x", live+"/x/root" }, "overlaps the live root"},
		{"the work directory holds the live root", func(i *identity) { i.work = "/Users/someone/.local" }, "overlaps the live root"},
		{"the label is the live label", func(i *identity) { i.label = "live" }, "is the live one"},
		{"the filesystem root", func(i *identity) { i.work = "/" }, "not a directory this can own"},
		{"a relative path", func(i *identity) { i.work = "tmp" }, "not a directory this can own"},
		{"no path", func(i *identity) { i.work = "" }, "not a directory this can own"},
		{"the home directory", func(i *identity) { i.work = "/Users/someone" }, "holds the home directory"},
	} {
		id := base
		id.liveRoots, id.liveLabels = []string{live}, []string{"live"}
		tc.edit(&id)
		got := strings.Join(refusals(id), "; ")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The live identity comes from the same code the binary uses, so this cannot drift from it.
func TestIdentifyRefusesTheDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALBASE_ROOT", "")
	id, _, err := identify(filepath.Join(home, ".local", "share", "local-base", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(refusals(id), "; "); !strings.Contains(got, "overlaps the live root") {
		t.Errorf("under the default root: %q", got)
	}

	other := filepath.Join(home, "elsewhere")
	t.Setenv("LOCALBASE_ROOT", other)
	id, _, err = identify(other)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(refusals(id), "; "); !strings.Contains(got, "overlaps the live root") {
		t.Errorf("under LOCALBASE_ROOT: %q", got)
	}

	id, _, err = identify(filepath.Join(home, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if got := refusals(id); len(got) != 0 {
		t.Errorf("a scratch directory: %v", got)
	}
}

func TestUsableWorkKeepsOtherPeoplesDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := usableWork(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("absent: %v", err)
	}
	if err := usableWork(dir); err == nil || !strings.Contains(err.Error(), "not made by this tool") {
		t.Errorf("someone else's: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := usableWork(dir); err == nil || !strings.Contains(err.Error(), "--cleanup") {
		t.Errorf("left over: %v", err)
	}
}

func TestPreflightCleanupOnlyTakesItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	id := identity{work: dir, root: filepath.Join(dir, "root"), label: "scratch"}
	if got := strings.Join(preflight(dir, id, serviceMeta(dir), true), "; "); !strings.Contains(got, "not made by this tool") {
		t.Errorf("cleanup of someone else's directory: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := preflight(dir, id, serviceMeta(dir), true); len(got) != 0 {
		t.Errorf("cleanup of its own directory: %v", got)
	}
}

func TestScratchConfigSetsEveryPort(t *testing.T) {
	doc := `version = 1

[gateway]
host = "0.0.0.0"
port = 2273

[runtime]
host = "0.0.0.0"
port = 18000
ctxSize = 131072
ctxMode = "auto"
parallel = "auto"
sttHost = "0.0.0.0"
sttPort = 18080

[models]
selectedLlmModels = ["a"]
selectedSttModels = ["b"]
selectedTtsModels = []
selectedImageModels = ["c"]
selectedVideoModels = []
activeLlmModel = "a"
activeSttModel = "b"
activeTtsModel = ""
activeImageModel = "c"
activeVideoModel = ""

[memory]

[memory.systemReserve]
percent = 15
minimumGb = 8

[memory.acceleratorReserve]
percent = 10
minimumGb = 2
`
	got, err := scratchConfig(doc, rehearsalPorts)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"2273", "18000", "18080", "0.0.0.0", `["a"]`, `["b"]`, `["c"]`} {
		if strings.Contains(got, gone) {
			t.Errorf("%s is still there:\n%s", gone, got)
		}
	}
	for _, want := range []string{"port = 29500", "port = 29501", "sttPort = 29502", `selectedImageModels = []`, `activeImageModel = ""`, `selectedLlmModels = ["rehearsal-tiny"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s is missing:\n%s", want, got)
		}
	}
	// A key it expects and does not find stops the run: a missing port falls back to the live one.
	if _, err := scratchConfig(strings.Replace(doc, "sttPort = 18080\n", "", 1), rehearsalPorts); err == nil || !strings.Contains(err.Error(), "runtime.sttPort") {
		t.Errorf("a missing key: %v", err)
	}
}

func TestPortsAvoidEveryBlock(t *testing.T) {
	blocks := [][2]int{{24173, 24173}, {24600, 24999}, {25000, 25499}, {26000, 26499}, {27000, 27499}, {27500, 27999}, {28000, 28499}, {28500, 28999}, {29000, 29499}}
	defaults := []int{2273, 18000, 18080, 8090, 8091, 8092}
	for _, p := range ports {
		for _, b := range blocks {
			if p >= b[0] && p <= b[1] {
				t.Errorf("port %d is in the block %d-%d", p, b[0], b[1])
			}
		}
		for _, d := range defaults {
			if p == d {
				t.Errorf("port %d is a default", p)
			}
		}
	}
}

func TestSameLive(t *testing.T) {
	a := map[string]liveState{"live": {Definition: "d", Binary: "b", ProcessID: "1"}}
	if got := sameLive(a, map[string]liveState{"live": {Definition: "d", Binary: "b", ProcessID: "1"}}); got != "" {
		t.Errorf("same: %q", got)
	}
	for name, now := range map[string]liveState{
		"a restart":    {Definition: "d", Binary: "b", ProcessID: "2"},
		"a definition": {Definition: "x", Binary: "b", ProcessID: "1"},
		"a binary":     {Definition: "d", Binary: "x", ProcessID: "1"},
	} {
		if got := sameLive(a, map[string]liveState{"live": now}); got == "" {
			t.Errorf("%s was not noticed", name)
		}
	}
}

func serviceMeta(dir string) service.Metadata {
	return service.Metadata{Manager: "launchd", ServiceID: "scratch", DefinitionPath: filepath.Join(dir, "absent.plist")}
}

// testPorts are four free ports. LBUR_TEST_PORT_BASE keeps them in a thread's own block.
func testPorts(t *testing.T) portSet {
	t.Helper()
	if v := os.Getenv("LBUR_TEST_PORT_BASE"); v != "" {
		base, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []int{base, base + 1, base + 2, base + 3} {
			if portBusy(p) {
				t.Fatalf("port %d is in use", p)
			}
		}
		return portSet{base, base + 1, base + 2, base + 3}
	}
	var got []int
	for i := 0; i < 4; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		got = append(got, l.Addr().(*net.TCPAddr).Port)
	}
	return portSet{got[0], got[1], got[2], got[3]}
}

// goDir moves to go/, where the kit's builds run.
func goDir(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
}

func scratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lbur-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// failOnAbort turns the drill's stop into a test failure.
func failOnAbort(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		if a, ok := r.(abort); ok {
			t.Fatal(a.why)
		}
		panic(r)
	}
}

// targets is every release target, and what the workflow calls each one.
var targets = []struct{ goos, goarch string }{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}}

// prepare reads a release the way upgrade does, for any target, from any computer. It does
// not run the binary it reads, which may be for another system.
func prepare(t *testing.T, files http.Handler, pub ed25519.PublicKey, version, goos, goarch string) []byte {
	t.Helper()
	srv := httptest.NewServer(files)
	defer srv.Close()
	dir := t.TempDir()
	c, err := upgrade.Prepare(context.Background(), upgrade.Source{API: srv.URL + "/latest", Files: srv.URL + "/files", Client: upgrade.NewClient()},
		upgrade.PrepareOptions{Tag: "v" + version, GOOS: goos, GOARCH: goarch, Dir: dir,
			Keys: []upgrade.Key{{ID: keyID, Public: base64.StdEncoding.EncodeToString(pub)}},
			Run:  func(string) (string, error) { return version, nil }})
	if err != nil {
		t.Fatalf("%s/%s: %v", goos, goarch, err)
	}
	b, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// entries lists an archive, by the name each file would have once unpacked.
func entries(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	if strings.HasSuffix(path, ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			out = append(out, strings.TrimPrefix(f.Name, "./"))
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, strings.TrimPrefix(h.Name, "./"))
		}
	}
	slices.Sort(out)
	return out
}

// The first launchd run failed because the kit always wrote a tar.gz, and upgrade reads a zip on
// macOS. Linux was right by luck, so the systemd run could not see it. Every target is read here
// from any computer, the way upgrade reads it.
func TestKitIsPackagedLikeARelease(t *testing.T) {
	if _, err := exec.LookPath("zip"); err != nil {
		t.Skip("the release workflow's zip is not on this computer")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		kit := scratch(t)
		body := []byte("a binary for " + target.goos + "/" + target.goarch)
		binary := filepath.Join(kit, "binary")
		if err := os.WriteFile(binary, body, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := publish(kit, newVersion, binary, target.goos, target.goarch, priv); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(filepath.Join(kit, "releases")))))
		if got := prepare(t, mux, pub, newVersion, target.goos, target.goarch); !bytes.Equal(got, body) {
			t.Errorf("%s/%s: read %q", target.goos, target.goarch, got)
		}
		archive := filepath.Join(kit, "releases", "v"+newVersion, upgrade.AssetArchive(target.goos, target.goarch))
		if isZip := strings.HasSuffix(archive, ".zip"); isZip != (target.goos == "darwin") {
			t.Errorf("%s: zip on macOS only: %s", target.goos, archive)
		}
		want := []string{"LICENSE", "README.md", "THIRD_PARTY_NOTICES.txt", upgrade.AssetBinary(target.goos, target.goarch)}
		slices.Sort(want)
		if got := entries(t, archive); !slices.Equal(got, want) {
			t.Errorf("%s/%s holds %v, want %v", target.goos, target.goarch, got, want)
		}
	}
}

// The real script: what the workflow publishes must be what upgrade reads, and hold what the
// kit's archives hold. LBUR_RELEASE_SCRIPT points at another copy of it. A missing script fails
// this test rather than leaving it to check nothing.
func TestReleaseScriptMatches(t *testing.T) {
	script := os.Getenv("LBUR_RELEASE_SCRIPT")
	if script == "" {
		script = filepath.Join(testutil.RepoRoot(t), "scripts", "release", "build.sh")
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the release build script: %v", err)
	}
	if _, err := exec.LookPath("zip"); err != nil {
		t.Skip("the release workflow's zip is not on this computer")
	}
	out := scratch(t)
	if err := os.MkdirAll(filepath.Join(out, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		name := upgrade.AssetBinary(target.goos, target.goarch)
		if err := os.WriteFile(filepath.Join(out, "bin", name), []byte("a binary for "+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []string{"package", "checksums"} {
		cmd := exec.Command("bash", script, step, out)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build.sh %s: %v\n%s", step, err, b)
		}
	}
	dist := filepath.Join(out, "dist")
	checks, err := os.ReadFile(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	files := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if name == "checksums.txt.sig" {
			w.Write(signature(priv, checks))
			return
		}
		http.ServeFile(w, r, filepath.Join(dist, name))
	})
	for _, target := range targets {
		name := upgrade.AssetBinary(target.goos, target.goarch)
		if got := prepare(t, files, pub, newVersion, target.goos, target.goarch); string(got) != "a binary for "+name {
			t.Errorf("%s: read %q", name, got)
		}
		kit := scratch(t)
		binary := filepath.Join(kit, "binary")
		if err := os.WriteFile(binary, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		mine := filepath.Join(kit, upgrade.AssetArchive(target.goos, target.goarch))
		if err := packRelease(mine, binary, target.goos, target.goarch); err != nil {
			t.Fatal(err)
		}
		if got, want := entries(t, mine), entries(t, filepath.Join(dist, upgrade.AssetArchive(target.goos, target.goarch))); !slices.Equal(got, want) {
			t.Errorf("%s: the kit holds %v, the release %v", name, got, want)
		}
	}
}

// A real upgrade and rollback on this computer, with no gateway and no service manager: the download
// path on the system this runs on, which a rehearsal must not be the first thing to reach.
func TestUpgradeOnThisComputer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries")
	}
	if os.Getuid() == 0 {
		t.Skip("a root refuses uid 0")
	}
	goDir(t)
	work := scratch(t)
	kit := filepath.Join(work, "kit")
	if err := buildKit(kit, goruntime.GOOS, goruntime.GOARCH); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &drill{ctx: ctx, work: work, root: filepath.Join(work, "root"), bin: filepath.Join(work, "bin", "local-base"), kit: kit, ports: testPorts(t), latest: newVersion}
	defer failOnAbort(t)
	srv, err := serveReleases(kit, &d.latest, d.ports.release)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	d.installOld()
	d.must("init")
	if code, out, errb := d.lb("upgrade", "--yes"); code != 0 {
		t.Fatalf("upgrade: exit %d\n%s%s", code, out, errb)
	}
	if got := versionOf(d.bin); got != newVersion {
		t.Errorf("the binary is %s", got)
	}
	if got := versionOf(d.bin + upgrade.PreviousSuffix); got != oldVersion {
		t.Errorf("the previous binary is %s", got)
	}
	if code, out, errb := d.lb("upgrade", "rollback"); code != 0 {
		t.Fatalf("rollback: exit %d\n%s%s", code, out, errb)
	}
	if got := versionOf(d.bin); got != oldVersion {
		t.Errorf("after rollback the binary is %s", got)
	}
}

// The whole drill rests on the fixture passing smoke with nothing but what a service gets: no
// environment, no flags. This runs it in the foreground, so it needs no service manager, and
// reports a broken fixture before anyone starts a rehearsal.
func TestFixturePassesSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries")
	}
	if os.Getuid() == 0 {
		t.Skip("a root refuses uid 0")
	}
	goDir(t)
	work := scratch(t)
	kit := filepath.Join(work, "kit")
	if err := buildKit(kit, goruntime.GOOS, goruntime.GOARCH); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testPorts(t)
	d := &drill{ctx: ctx, work: work, root: filepath.Join(work, "root"), bin: filepath.Join(work, "bin", "local-base"), kit: kit, ports: p, latest: newVersion}
	defer failOnAbort(t)
	d.installOld()
	d.fixture()

	// Exactly how the service definition starts it: --root and serve, nothing in the environment.
	cmd := exec.Command(d.bin, "--root", d.root, "serve")
	cmd.Env = d.env()
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	url := "http://127.0.0.1:" + strconv.Itoa(p.gateway) + "/health"
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if resp, err := http.Get(url); err == nil {
			var h struct{ Version string }
			json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if h.Version != oldVersion {
				t.Fatalf("%s serves %q, not the scratch gateway's %s", url, h.Version, oldVersion)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no gateway on %d:\n%s", p.gateway, logs.String())
		}
	}
	if code, out, errb := d.lb("smoke"); code != 0 {
		t.Fatalf("smoke: exit %d\n%s%s\n%s", code, out, errb, logs.String())
	}

	// A media model selected by default would point at a shared default port.
	if show := d.must("config", "show"); strings.Contains(show, "dreamshaper") || strings.Contains(show, "whisper") {
		t.Errorf("a default model is still selected:\n%s", show)
	}
}
