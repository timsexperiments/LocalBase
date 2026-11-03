package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/service"
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

// testPorts are three free ports. LBUR_TEST_PORT_BASE keeps them in a thread's own block.
func testPorts(t *testing.T) portSet {
	t.Helper()
	if v := os.Getenv("LBUR_TEST_PORT_BASE"); v != "" {
		base, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []int{base, base + 1, base + 2} {
			if portBusy(p) {
				t.Fatalf("port %d is in use", p)
			}
		}
		return portSet{base, base + 1, base + 2}
	}
	var got []int
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		got = append(got, l.Addr().(*net.TCPAddr).Port)
	}
	return portSet{got[0], got[1], got[2]}
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
	if wd, err := os.Getwd(); err != nil {
		t.Fatal(err)
	} else if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil {
		t.Fatal(err)
	} else {
		t.Cleanup(func() { os.Chdir(wd) })
	}
	work, err := os.MkdirTemp("", "lbur-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if work, err = filepath.EvalSymlinks(work); err != nil {
		t.Fatal(err)
	}
	kit := filepath.Join(work, "kit")
	if err := buildKit(kit, goruntime.GOOS, goruntime.GOARCH); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testPorts(t)
	d := &drill{ctx: ctx, work: work, root: filepath.Join(work, "root"), bin: filepath.Join(work, "bin", "local-base"), kit: kit, ports: p, latest: newVersion}
	defer func() {
		if r := recover(); r != nil {
			if a, ok := r.(abort); ok {
				t.Fatal(a.why)
			}
			panic(r)
		}
	}()
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
