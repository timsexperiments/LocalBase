// Command upgrade-rehearsal runs `local-base upgrade` under a real service manager: a scratch
// root under its own service label, a throwaway signing key, a release served from loopback and
// a fake backend. No real models and no network. DESIGN §33.6.
//
//	go run ./tools/upgrade-rehearsal              # the whole drill, from go/
//	go run ./tools/upgrade-rehearsal --preflight  # the refusals only, changes nothing
//	go run ./tools/upgrade-rehearsal --cleanup    # remove what an interrupted run left
//
// It never starts, stops or runs anything of the live service. It reads the live service's
// state, and stops if that changes.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

const (
	defaultWork = "/private/tmp/lb-upgrade-rehearsal"
	markerName  = ".lb-upgrade-rehearsal"
	modulePath  = "github.com/timsexperiments/LocalBase/go/internal"
	keyID       = "rehearsal"
	modelID     = "rehearsal-tiny"
	backendID   = "rehearsal-fake"
	modelFile   = "rehearsal-tiny.gguf"

	gatewayPort = 29500
	llmPort     = 29501
	sttPort     = 29502
	releasePort = 29506

	oldVersion    = "0.90.0"
	newVersion    = "0.91.0"
	brokenVersion = "0.92.0"
)

// ports are everything this listens on. Nothing here takes a port from the command line.
var ports = []int{gatewayPort, llmPort, sttPort, releasePort}

// portSet is where the scratch root's gateway and backends listen.
type portSet struct{ gateway, llm, stt, release int }

var rehearsalPorts = portSet{gatewayPort, llmPort, sttPort, releasePort}

// identity is what a rehearsal would touch, and what it must never touch.
type identity struct {
	work, root, label string
	liveRoots         []string
	liveLabels        []string
	home              string
}

func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// refusals is every reason this identity may not run. The live service must never be touched,
// and everything under the work directory is deleted at the end.
func refusals(id identity) []string {
	var out []string
	for _, live := range id.liveRoots {
		if live == "" {
			continue
		}
		if inside(id.root, live) || inside(live, id.root) || inside(id.work, live) || inside(live, id.work) {
			out = append(out, fmt.Sprintf("%s overlaps the live root %s", id.work, live))
		}
	}
	for _, live := range id.liveLabels {
		if id.label == live {
			out = append(out, "the service label "+id.label+" is the live one")
		}
	}
	if id.work == "" || !filepath.IsAbs(id.work) || id.work == string(filepath.Separator) {
		out = append(out, fmt.Sprintf("%q is not a directory this can own", id.work))
	}
	if id.home != "" && inside(id.home, id.work) {
		out = append(out, id.work+" holds the home directory")
	}
	return out
}

// usableWork refuses a directory that exists and isn't ours, since the end of a run deletes it.
func usableWork(work string) error {
	if _, err := os.Stat(work); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(work, markerName)); err != nil {
		return fmt.Errorf("%s exists and was not made by this tool", work)
	}
	return fmt.Errorf("%s is left from an earlier run: run with --cleanup first", work)
}

func portBusy(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func identify(work string) (identity, service.Metadata, error) {
	if canon, err := root.Canonical(work); err == nil {
		work = canon
	}
	rootDir := filepath.Join(work, "root")
	home, _ := os.UserHomeDir()
	m, err := service.MetadataFor(rootDir)
	if err != nil {
		return identity{}, m, err
	}
	id := identity{work: work, root: rootDir, label: m.ServiceID, home: home}
	seen := map[string]bool{}
	for _, live := range []string{root.Default(), os.Getenv("LOCALBASE_ROOT")} {
		if live == "" {
			continue
		}
		if canon, err := root.Canonical(live); err == nil {
			live = canon
		}
		if seen[live] {
			continue
		}
		seen[live] = true
		id.liveRoots = append(id.liveRoots, live)
		lm, err := service.MetadataFor(live)
		if err != nil {
			return identity{}, m, err
		}
		id.liveLabels = append(id.liveLabels, lm.ServiceID)
	}
	return id, m, nil
}

// --- the live service, read only --------------------------------------------------------------

type liveState struct {
	Definition, Binary, ProcessID string
}

func fileSum(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "absent"
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// livePID asks the manager and changes nothing.
func livePID(m service.Metadata) string {
	var out []byte
	if m.Manager == "launchd" {
		out, _ = exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+m.ServiceID).Output()
		for _, line := range strings.Split(string(out), "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "pid = ") {
				return strings.TrimPrefix(t, "pid = ")
			}
		}
		return "none"
	}
	out, _ = exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", m.UnitName).Output()
	return strings.TrimSpace(string(out))
}

func snapshotLive(home string, labels []string, roots []string) (map[string]liveState, error) {
	out := map[string]liveState{}
	for i, label := range labels {
		m, err := service.MetadataFor(roots[i])
		if err != nil {
			return nil, err
		}
		out[label] = liveState{Definition: fileSum(m.DefinitionPath), Binary: fileSum(filepath.Join(home, ".local", "bin", "local-base")), ProcessID: livePID(m)}
	}
	return out, nil
}

func sameLive(a, b map[string]liveState) string {
	for label, was := range a {
		if now := b[label]; now != was {
			return fmt.Sprintf("the live service changed during this run (%s): %+v, was %+v", label, now, was)
		}
	}
	return ""
}

// --- the kit: binaries and signed releases ------------------------------------------------------

func goBuild(goos, goarch, ldflags, out, pkg string) error {
	// The flags scripts/release/build.sh uses.
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w "+ldflags, "-o", out, pkg)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, b)
	}
	return nil
}

// packRelease archives a binary the way scripts/release/build.sh does: the binary named for its
// target beside the license files, zip on macOS and tar.gz elsewhere, with the same two tools.
// TestReleaseScriptMatches holds this against the real script.
func packRelease(archive, binary, goos, goarch string) error {
	staging, err := os.MkdirTemp("", "lbur-package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	body, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	name := upgrade.AssetBinary(goos, goarch)
	if err := os.WriteFile(filepath.Join(staging, name), body, 0o755); err != nil { //nolint:gosec // G306: the packaged program has to run.
		return err
	}
	names := []string{"./" + name}
	for _, extra := range []string{"LICENSE", "README.md", "THIRD_PARTY_NOTICES.txt"} {
		if err := os.WriteFile(filepath.Join(staging, extra), []byte("A rehearsal placeholder.\n"), 0o600); err != nil {
			return err
		}
		names = append(names, "./"+extra)
	}
	var cmd *exec.Cmd
	if strings.HasSuffix(archive, ".zip") {
		cmd = exec.Command("zip", append([]string{"-qX", archive}, names...)...)
	} else {
		cmd = exec.Command("tar", append([]string{"-czf", archive}, names...)...)
		cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	}
	cmd.Dir = staging
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", cmd.Args[0], err, out)
	}
	return nil
}

// publish writes what a release has, signed the way a real one is.
func publish(kit, version, binary, goos, goarch string, priv ed25519.PrivateKey) error {
	archive := upgrade.AssetArchive(goos, goarch)
	if archive == "" {
		return fmt.Errorf("there is no release for %s/%s", goos, goarch)
	}
	dir, err := filepath.Abs(filepath.Join(kit, "releases", "v"+version))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, archive)
	if err := packRelease(path, binary, goos, goarch); err != nil {
		return err
	}
	packed, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(packed)
	checks := []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	for file, data := range map[string][]byte{"checksums.txt": checks, "checksums.txt.sig": signature(priv, checks)} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func signature(priv ed25519.PrivateKey, checks []byte) []byte {
	return []byte(keyID + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, checks)) + "\n")
}

// brokenBinary passes every check a download gets, and then cannot restart anything.
func brokenBinary(version string) string {
	return "#!/bin/sh\ncase \"$1\" in --version) echo " + version + " ;; *) echo 'this release is broken on purpose' >&2; exit 1 ;; esac\n"
}

// buildKit makes the binaries and signs the releases. The private key is never written down.
func buildKit(kit, goos, goarch string) error {
	if err := os.MkdirAll(kit, 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	flags := func(version string) string {
		return "-X " + modulePath + "/version.Version=" + version + " -X " + modulePath + "/version.Commit=rehearsal" +
			" -X " + modulePath + "/upgrade.testKey=" + keyID + ":" + base64.StdEncoding.EncodeToString(pub)
	}
	for version, name := range map[string]string{oldVersion: "local-base-old", newVersion: "local-base-new"} {
		if err := goBuild(goos, goarch, flags(version), filepath.Join(kit, name), "./cmd/localbase"); err != nil {
			return err
		}
	}
	if err := goBuild(goos, goarch, "", filepath.Join(kit, "fake-backend"), "./internal/runtime/fakebackend"); err != nil {
		return err
	}
	broken := filepath.Join(kit, "local-base-broken")
	if err := os.WriteFile(broken, []byte(brokenBinary(brokenVersion)), 0o755); err != nil { //nolint:gosec // G306: the broken release has to run, to fail its check.
		return err
	}
	if err := publish(kit, newVersion, filepath.Join(kit, "local-base-new"), goos, goarch, priv); err != nil {
		return err
	}
	return publish(kit, brokenVersion, broken, goos, goarch, priv)
}

// serveReleases answers the check, and serves the kit's files by tag.
func serveReleases(kit string, latest *string, port int) (*http.Server, error) {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		// If this can't be written, the upgrade under test fails its check, and the drill reports that.
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v" + *latest, "html_url": "https://example.test/v" + *latest})
	})
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(filepath.Join(kit, "releases")))))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	// Serve always returns an error, and on the way out it's ErrServerClosed. If it dies early, the
	// drill's next request fails and says so.
	go func() { _ = srv.Serve(l) }()
	return srv, nil
}

// --- the fixture --------------------------------------------------------------------------------

// patchConfig sets keys of a `config show` document, by section. A key this expects and does not
// find is an error, because a missing port is how a scratch root ends up on the live ones.
func patchConfig(doc string, set map[string]map[string]string) (string, error) {
	section := ""
	found := map[string]bool{}
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.Trim(t, "[]")
			continue
		}
		key, _, ok := strings.Cut(t, " = ")
		if !ok {
			continue
		}
		if value, want := set[section][key]; want {
			lines[i] = key + " = " + value
			found[section+"."+key] = true
		}
	}
	for s, keys := range set {
		for key := range keys {
			if !found[s+"."+key] {
				return "", fmt.Errorf("the config has no %s.%s", s, key)
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}

func scratchConfig(doc string, p portSet) (string, error) {
	model := strconv.Quote(modelID)
	return patchConfig(doc, map[string]map[string]string{
		"gateway": {"host": `"127.0.0.1"`, "port": strconv.Itoa(p.gateway)},
		"runtime": {"host": `"127.0.0.1"`, "port": strconv.Itoa(p.llm), "sttHost": `"127.0.0.1"`, "sttPort": strconv.Itoa(p.stt),
			"ctxSize": "2048", "ctxMode": `"explicit"`, "parallel": "1"},
		"models": {"selectedLlmModels": "[" + model + "]", "selectedSttModels": "[]", "selectedTtsModels": "[]", "selectedImageModels": "[]", "selectedVideoModels": "[]",
			"activeLlmModel": model, "activeSttModel": `""`, "activeTtsModel": `""`, "activeImageModel": `""`, "activeVideoModel": `""`},
		"memory.systemReserve":      {"percent": "1", "minimumGb": "0"},
		"memory.acceleratorReserve": {"percent": "1", "minimumGb": "0"},
	})
}

type drill struct {
	ctx                  context.Context
	work, root, bin, kit string
	meta                 service.Metadata
	ports                portSet
	latest               string
	fails                int
	liveWas              map[string]liveState
	id                   identity
}

// env is the children's environment: nothing inherited that could point at another root or
// reach the service definition, and the release only from loopback.
func (d *drill) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LOCALBASE_") || strings.HasPrefix(kv, "OTEL_") {
			continue
		}
		out = append(out, kv)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(d.ports.release)
	return append(out, "LOCALBASE_RELEASE_URL="+base+"/files", "LOCALBASE_RELEASE_API_URL="+base+"/latest")
}

func (d *drill) lb(args ...string) (int, string, string) {
	if d.ctx.Err() != nil {
		d.stop("interrupted")
	}
	cmd := exec.CommandContext(d.ctx, d.bin, append([]string{"--root", d.root}, args...)...)
	cmd.Env = d.env()
	cmd.Dir = d.work
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
		errb.WriteString(err.Error())
	}
	return code, out.String(), errb.String()
}

func (d *drill) must(args ...string) string {
	code, out, errb := d.lb(args...)
	if code != 0 {
		d.stop(fmt.Sprintf("%s: exit %d\n%s%s", strings.Join(args, " "), code, out, errb))
	}
	return out
}

func (d *drill) ok(format string, a ...any) { fmt.Printf("ok   %s\n", fmt.Sprintf(format, a...)) }

func (d *drill) bad(format string, a ...any) {
	fmt.Printf("FAIL %s\n", fmt.Sprintf(format, a...))
	d.fails++
}

func (d *drill) check(good bool, format string, a ...any) {
	if good {
		d.ok(format, a...)
	} else {
		d.bad(format, a...)
	}
}

type abort struct{ why string }

func (d *drill) stop(why string) { panic(abort{why}) }

// guard stops everything the moment the live service looks different.
func (d *drill) guard() {
	now, err := snapshotLive(d.id.home, d.id.liveLabels, d.id.liveRoots)
	if err != nil {
		d.stop(err.Error())
	}
	if why := sameLive(d.liveWas, now); why != "" {
		d.stop(why)
	}
}

func (d *drill) installOld() {
	b, err := os.ReadFile(filepath.Join(d.kit, "local-base-old"))
	if err != nil {
		d.stop(err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(d.bin), 0o700); err != nil {
		d.stop(err.Error())
	}
	if err := os.WriteFile(d.bin+".new", b, 0o755); err != nil { //nolint:gosec // G306: the program under test has to run.
		d.stop(err.Error())
	}
	if err := os.Rename(d.bin+".new", d.bin); err != nil {
		d.stop(err.Error())
	}
}

func (d *drill) fixture() {
	fake := filepath.Join(d.work, "fake-backend")
	b, err := os.ReadFile(filepath.Join(d.kit, "fake-backend"))
	if err == nil {
		err = os.WriteFile(fake, b, 0o755) //nolint:gosec // G306: the fake backend has to run.
	}
	if err != nil {
		d.stop(err.Error())
	}
	d.must("init")

	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[goruntime.GOARCH]
	def, _ := json.Marshal(map[string]any{"schema": "localbase.backend/v1", "id": backendID, "name": "Rehearsal fake", "engine": "fake-engine",
		"adapter": "process-openai/v1", "tasks": []string{"chat"}, "formats": []string{"gguf"}, "capabilities": []string{},
		"architectures": map[string]any{"supported": []string{"rehearsal"}},
		"platforms":     []any{map[string]any{"os": goruntime.GOOS, "arch": arch, "accelerator": "cpu"}},
		"launch": map[string]any{"command": fake, "args": []string{"--host", "{host}", "--port", "{port}", "-m", "{model}"}, "healthPath": "/health",
			"memory": map[string]any{"weightsFactor": 1, "overheadGb": 0, "assumption": "a fake backend holds next to nothing"}}})
	defPath := filepath.Join(d.work, "backend.json")
	if err := os.WriteFile(defPath, def, 0o600); err != nil {
		d.stop(err.Error())
	}
	d.must("backends", "add", "--file", defPath)

	modelsDir := filepath.Join(d.root, "models", "llm")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		d.stop(err.Error())
	}
	file := filepath.Join(modelsDir, modelFile)
	if err := testutil.GGUF(file, map[string]any{"general.architecture": "rehearsal", "general.file_type": 15, "rehearsal.context_length": 4096}, 1<<20); err != nil {
		d.stop(err.Error())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		d.stop(err.Error())
	}
	sum := sha256.Sum256(data)
	entry, _ := json.Marshal(map[string]any{"modelId": modelID, "displayName": "Rehearsal tiny", "kind": "llm", "provider": "LocalBase tests", "family": "Rehearsal",
		"version": "test", "size": "tiny", "quant": "Q4_K_M", "storageGb": 0.01, "source": "https://huggingface.co/localbase/rehearsal-tiny", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts":       []any{map[string]any{"sourcePath": modelFile, "filename": modelFile, "expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"text"}, "outputModalities": []string{"text"}, "contextWindowTokens": 4096, "capabilities": []string{"text-generation"},
		"commercialStatus": "open", "catch": "Rehearsal data.", "notes": "Rehearsal fixture."})
	if err := os.MkdirAll(filepath.Join(d.root, "catalog.d"), 0o700); err != nil {
		d.stop(err.Error())
	}
	if err := os.WriteFile(filepath.Join(d.root, "catalog.d", modelID+".json"), entry, 0o600); err != nil {
		d.stop(err.Error())
	}

	doc, err := scratchConfig(d.must("config", "show"), d.ports)
	if err != nil {
		d.stop(err.Error())
	}
	cfg := filepath.Join(d.work, "config.toml")
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		d.stop(err.Error())
	}
	d.must("config", "apply", "--file", cfg, "--restart", "never")
	d.must("models", "backend", modelID, "--set", backendID)
}

type status struct {
	Service struct {
		ServiceID string `json:"serviceId"`
		Enabled   *bool  `json:"enabled"`
		State     string `json:"state"`
		PID       *int   `json:"pid"`
	} `json:"service"`
	Gateway struct {
		State string `json:"state"`
	} `json:"gateway"`
}

func (d *drill) status() status {
	var wrapped struct {
		Data status `json:"data"`
	}
	_, out, _ := d.lb("--json", "status")
	// A status that can't be read stays empty, and no check accepts an empty status.
	_ = json.Unmarshal([]byte(out), &wrapped)
	return wrapped.Data
}

func (s status) running() bool {
	return s.Service.State == "running" && s.Service.Enabled != nil && *s.Service.Enabled && s.Gateway.State == "ready"
}

func (s status) pid() int {
	if s.Service.PID == nil {
		return 0
	}
	return *s.Service.PID
}

func (d *drill) waitRunning() status {
	var s status
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if s = d.status(); s.running() {
			return s
		}
	}
	return s
}

// served is the version the gateway itself reports.
func served() string {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(gatewayPort) + "/health")
	if err != nil {
		return "none"
	}
	defer resp.Body.Close()
	var h struct{ Version string }
	// An answer that can't be read gives no version, and no check accepts that.
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h)
	return h.Version
}

func versionOf(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "none"
	}
	return strings.TrimSpace(string(out))
}

// --- the drill ----------------------------------------------------------------------------------

func (d *drill) run() {
	d.installOld()
	fmt.Println("== a service on the old version")
	d.fixture()
	if d.status().Service.ServiceID != d.id.label {
		d.stop("this root's label is not the one checked: " + d.status().Service.ServiceID)
	}
	d.must("start")
	before := d.waitRunning()
	d.check(before.running(), "running under %s as %s: %+v", d.meta.Manager, d.id.label, before.Service.State)
	if !before.running() {
		d.stop("the service did not come up")
	}
	d.check(served() == oldVersion, "the gateway serves %s", oldVersion)
	code, out, errb := d.lb("smoke")
	d.check(code == 0, "smoke passes before anything changes")
	if code != 0 {
		d.stop("the fixture does not pass smoke, so nothing later means anything\n" + out + errb)
	}
	d.guard()

	fmt.Println("== --check, and what it leaves")
	code, out, errb = d.lb("upgrade", "--check")
	d.check(code == 0 && strings.Contains(out+errb, newVersion), "--check names %s", newVersion)
	d.check(d.status().pid() == before.pid() && versionOf(d.bin) == oldVersion, "--check changed nothing")

	fmt.Println("== upgrade")
	code, out, errb = d.lb("upgrade", "--yes")
	d.check(code == 0, "upgrade exits 0: %d\n%s%s", code, out, errb)
	after := d.waitRunning()
	d.check(versionOf(d.bin) == newVersion, "the binary is %s", newVersion)
	d.check(versionOf(d.bin+upgrade.PreviousSuffix) == oldVersion, "%s is kept as %s", oldVersion, upgrade.PreviousSuffix)
	d.check(after.running(), "the service is running and enabled again: %+v", after.Service.State)
	d.check(after.pid() != 0 && after.pid() != before.pid(), "a new process: %d, was %d", after.pid(), before.pid())
	d.check(served() == newVersion, "the gateway serves %s", newVersion)
	code, out, errb = d.lb("smoke")
	d.check(code == 0, "smoke passes on the new version: %s%s", out, errb)
	d.guard()

	fmt.Println("== rollback")
	code, out, errb = d.lb("upgrade", "rollback")
	d.check(code == 0, "rollback exits 0: %d\n%s%s", code, out, errb)
	back := d.waitRunning()
	d.check(versionOf(d.bin) == oldVersion, "the binary is %s again", oldVersion)
	d.check(versionOf(d.bin+upgrade.PreviousSuffix) == newVersion, "%s is kept, so it can be undone", newVersion)
	d.check(back.running() && served() == oldVersion, "the service is running and enabled on %s", oldVersion)
	d.guard()

	fmt.Println("== a release that will not start")
	d.latest = brokenVersion
	code, out, errb = d.lb("upgrade", "--yes")
	d.check(code != 0, "upgrade exits non-zero: %s%s", out, errb)
	restored := d.waitRunning()
	d.check(versionOf(d.bin) == oldVersion, "the binary went back to %s", oldVersion)
	d.check(restored.running() && served() == oldVersion, "the service is running and enabled on %s", oldVersion)
	code, out, errb = d.lb("smoke")
	d.check(code == 0, "smoke passes after going back: %s%s", out, errb)
	d.guard()
}

// removeOrSay is for a run that is already failing: the removal's own failure still has to show,
// or a throwaway service stays behind with nothing saying so.
func removeOrSay(work string, m service.Metadata, id identity) {
	if err := remove(work, m, id); err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal: removal:", err)
	}
}

// remove undoes a run: through the binary if it can, and through the manager for this one label
// if it cannot. identify and refusals have passed by the time this runs.
func remove(work string, m service.Metadata, id identity) error {
	bin := filepath.Join(work, "bin", "local-base")
	if _, err := os.Stat(filepath.Join(id.root, ".localbase-root.json")); err == nil {
		cmd := exec.Command(bin, "--root", id.root, "uninstall", "--yes")
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("uninstall through the binary failed, using the manager: %v\n%s", err, out)
		}
	}
	// The uninstall above has usually removed these already, so failing here is the normal case.
	if m.Manager == "launchd" {
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+m.ServiceID).Run()
	} else {
		_ = exec.Command("systemctl", "--user", "disable", "--now", m.UnitName).Run()
	}
	if err := os.Remove(m.DefinitionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.Manager != "launchd" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // Nothing here depends on it: the unit is already removed.
	}
	return os.RemoveAll(work)
}

func preflight(work string, id identity, m service.Metadata, cleanup bool) []string {
	bad := refusals(id)
	if !cleanup {
		if err := usableWork(work); err != nil {
			bad = append(bad, err.Error())
		}
		if _, err := os.Stat(m.DefinitionPath); err == nil {
			bad = append(bad, m.DefinitionPath+" exists: run with --cleanup first")
		}
		for _, p := range ports {
			if portBusy(p) {
				bad = append(bad, fmt.Sprintf("port %d is in use", p))
			}
		}
	} else if _, err := os.Stat(work); err == nil {
		if _, err := os.Stat(filepath.Join(work, markerName)); err != nil {
			bad = append(bad, work+" exists and was not made by this tool")
		}
	}
	return bad
}

func main() {
	os.Exit(mainCode())
}

func mainCode() int {
	work := flag.String("work", defaultWork, "Scratch directory, created and deleted by this; a fixed path gives a fixed label")
	kitDir := flag.String("kit", "", "Use a kit built with --build-kit")
	buildOnly := flag.String("build-kit", "", "Build a kit in this directory, and stop")
	goos := flag.String("goos", goruntime.GOOS, "With --build-kit: the system it is for")
	goarch := flag.String("goarch", goruntime.GOARCH, "With --build-kit: the architecture it is for")
	checksOnly := flag.Bool("preflight", false, "Run the refusals, and stop")
	cleanup := flag.Bool("cleanup", false, "Remove what an interrupted run left, and stop")
	flag.Parse()

	if *buildOnly != "" {
		if err := buildKit(*buildOnly, *goos, *goarch); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
			return 1
		}
		return 0
	}
	if os.Getuid() == 0 {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal: refusing to run as root")
		return 1
	}
	id, m, err := identify(*work)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
		return 1
	}
	fmt.Printf("manager:   %s\nlabel:     %s\nroot:      %s\nports:     %v (loopback)\n", m.Manager, id.label, id.root, ports)
	fmt.Printf("live, not touched: %s %s\n", strings.Join(id.liveLabels, " "), strings.Join(id.liveRoots, " "))
	if bad := preflight(id.work, id, m, *cleanup); len(bad) > 0 {
		for _, why := range bad {
			fmt.Fprintln(os.Stderr, "REFUSED:", why)
		}
		return 2
	}
	if *checksOnly {
		fmt.Println("preflight passed; nothing was changed")
		return 0
	}
	if *cleanup {
		if err := remove(id.work, m, id); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
			return 1
		}
		fmt.Println("removed")
		return 0
	}

	liveWas, err := snapshotLive(id.home, id.liveLabels, id.liveRoots)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
		return 1
	}
	if err := os.MkdirAll(id.work, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(id.work, markerName), []byte("made by tools/upgrade-rehearsal\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
		return 1
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	d := &drill{ctx: ctx, work: id.work, root: id.root, bin: filepath.Join(id.work, "bin", "local-base"), meta: m, ports: rehearsalPorts, latest: newVersion, liveWas: liveWas, id: id}
	d.kit = *kitDir
	if d.kit == "" {
		d.kit = filepath.Join(id.work, "kit")
		fmt.Println("== building the kit: a throwaway key, two binaries, a broken release")
		if err := buildKit(d.kit, goruntime.GOOS, goruntime.GOARCH); err != nil {
			removeOrSay(id.work, m, id)
			fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
			return 1
		}
	}
	srv, err := serveReleases(d.kit, &d.latest, d.ports.release)
	if err != nil {
		removeOrSay(id.work, m, id)
		fmt.Fprintln(os.Stderr, "upgrade-rehearsal:", err)
		return 1
	}

	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if a, ok := r.(abort); ok {
					done <- a.why
					return
				}
				done <- fmt.Sprint(r)
				return
			}
			done <- ""
		}()
		d.run()
	}()
	// An interrupt ends the command in progress and stops the drill at its next step, so
	// nothing is still running when the removal starts.
	why := <-done
	_ = srv.Close() // The drill is over, and the listener goes with the process either way.

	fmt.Println("== removing the throwaway service")
	if err := remove(id.work, m, id); err != nil {
		d.bad("removal: %v", err)
	}
	_, statErr := os.Stat(m.DefinitionPath)
	d.check(errors.Is(statErr, os.ErrNotExist), "the definition %s is gone", m.DefinitionPath)
	d.check(livePID(m) == "none" || livePID(m) == "0" || livePID(m) == "", "the manager has no process for the label")
	now, err := snapshotLive(id.home, id.liveLabels, id.liveRoots)
	if err == nil && why == "" {
		if changed := sameLive(liveWas, now); changed != "" {
			d.bad("%s", changed)
		} else {
			d.ok("the live service, its definition and its binary are unchanged")
		}
	}
	if why != "" {
		fmt.Println("STOPPED:", why)
		return 1
	}
	if d.fails > 0 {
		fmt.Printf("%d checks failed\n", d.fails)
		return 1
	}
	fmt.Println("all checks passed")
	return 0
}
