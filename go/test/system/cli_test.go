package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// CLI contract: the same command through both binaries, on twin scratch
// roots, must give the same exit code and the same JSON envelope apart from
// volatile values (ids, secrets, timestamps, the root path).

var (
	goCLIOnce sync.Once
	goCLIPath string
	goCLIErr  error
)

// removeGoCLI ends the builds of the binary (plain and lab), which every test run would otherwise leave behind.
func removeGoCLI() {
	for _, p := range []string{goCLIPath, goLabCLIPath} {
		if p != "" {
			_ = fsguard.RemoveAll(filepath.Dir(p))
		}
	}
}

func goCLI(t *testing.T) string {
	goCLIOnce.Do(func() {
		dir, err := fsguard.ScratchTemp("lbgo-cli-bin-")
		if err != nil {
			goCLIErr = err
			return
		}
		goCLIPath = filepath.Join(dir, "local-base")
		if out, err := exec.Command("go", harness.GoBuildArgs(goCLIPath)...).CombinedOutput(); err != nil {
			goCLIErr = err
			t.Log(string(out))
		}
	})
	if goCLIErr != nil {
		t.Fatal(goCLIErr)
	}
	return goCLIPath
}

var (
	goLabCLIOnce sync.Once
	goLabCLIPath string
	goLabCLIErr  error
)

// goLabCLI is the localbase_lab build of the CLI: the one that can read its host's memory from a
// file (onFixedHost). Built once, like goCLI.
func goLabCLI(t *testing.T) string {
	goLabCLIOnce.Do(func() {
		dir, err := fsguard.ScratchTemp("lbgo-cli-lab-bin-")
		if err != nil {
			goLabCLIErr = err
			return
		}
		goLabCLIPath = filepath.Join(dir, "local-base")
		args := harness.GoBuildArgs(goLabCLIPath)
		args = append([]string{args[0], "-tags", "localbase_lab"}, args[1:]...)
		if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			goLabCLIErr = err
			t.Log(string(out))
		}
	})
	if goLabCLIErr != nil {
		t.Fatal(goLabCLIErr)
	}
	return goLabCLIPath
}

type cliResult struct {
	code   int
	stdout string
	stderr string
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LOCALBASE_") && !strings.HasPrefix(kv, "HF_TOKEN=") {
			env = append(env, kv)
		}
	}
	return harness.Guarded(append(env, "LOCALBASE_CATALOG_FEED_URL=off", "NO_COLOR=1"))
}

func runCLI(t *testing.T, impl, root string, args ...string) cliResult {
	t.Helper()
	return runCLIEnv(t, impl, root, nil, args...)
}

// runCLIEnv runs a command with env entries replacing cleanEnv's.
func runCLIEnv(t *testing.T, impl, root string, env []string, args ...string) cliResult {
	t.Helper()
	full := append([]string{"--root", root}, args...)
	p := pinOf(t)
	var cmd *exec.Cmd
	switch {
	case impl == "ts" && (p == nil || tsLive()):
		cmd = exec.Command("bun", append([]string{"run", "src/cli.ts"}, full...)...)
		cmd.Dir = testutil.RepoRoot(t)
	case p != nil:
		// A pinned test's Go runs, and its TS twin's under -impl=go, are the lab build on the
		// fixed host (golden_test.go).
		cmd = exec.Command(goLabCLI(t), full...)
		env = append([]string{"LOCALBASE_LAB_HOST_FILE=" + p.host}, env...)
	default:
		cmd = exec.Command(goCLI(t), full...)
	}
	cmd.Env = cleanEnv()
	cmd.Env = append(cmd.Env, harness.CoverEnv()...)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		cmd.Env = slices.DeleteFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
		if kv != name+"=" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	res := cliResult{code, out.String(), errb.String()}
	if p != nil {
		p.ran(t, impl, root, args, res)
	}
	return res
}

var volatileValue = regexp.MustCompile(`key_[0-9a-f-]{36}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|lb_[A-Za-z0-9_-]{32}|\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)

// normalize parses an envelope, replaces root paths and volatile values.
func normalize(t *testing.T, s, root string) any {
	t.Helper()
	s = strings.ReplaceAll(s, root, "<root>")
	s = volatileValue.ReplaceAllStringFunc(s, func(m string) string {
		switch {
		case strings.HasPrefix(m, "key_"):
			return "<key-id>"
		case strings.HasPrefix(m, "lb_"):
			return "<secret>"
		case len(m) == 36 && m[8] == '-':
			return "<uuid>"
		}
		return "<time>"
	})
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &v); err != nil {
		t.Fatalf("not one JSON envelope: %v\n%s", err, s)
	}
	return v
}

type cliCase struct {
	// goArgs replaces args on Go, for a flag TS does not have. An issuer in the answer is then Go's own.
	goArgs []string
	args   []string
	skip   func(path string) bool
	// goOnly names paths only Go's answer has, each a named difference in
	// dual_differences_test.go: Go must have it and TS must not, and it is left out of the diff.
	goOnly []string
}

func twinRoots(t *testing.T) (string, string) {
	base := fsguard.ScratchDir(t)
	base, _ = filepath.EvalSymlinks(base)
	ts := filepath.Join(base, "ts")
	if p := pinOf(t); p != nil {
		p.mu.Lock()
		p.twin[ts] = true
		p.mu.Unlock()
	}
	return ts, filepath.Join(base, "go")
}

// initOwnTwinRoots runs each implementation's own `init` on its twin root (ts, go), so neither
// reads a root the other made. A Go-made root can't be read by TS: Go's store is WAL (#647), and TS
// opens the database read-only (src/db/client.ts openReadOnlyDatabase), which SQLite refuses for a
// WAL file with no -shm.
func initOwnTwinRoots(t *testing.T, roots [2]string) {
	t.Helper()
	for i, impl := range []string{"ts", "go"} {
		if res := runCLI(t, impl, roots[i], "--json", "init"); res.code != 0 {
			t.Fatalf("%s init: %s%s", impl, res.stdout, res.stderr)
		}
	}
}

func compareCLI(t *testing.T, roots [2]string, c cliCase) {
	t.Helper()
	ts := runCLI(t, "ts", roots[0], c.args...)
	goArgs := c.args
	if c.goArgs != nil {
		goArgs = c.goArgs
	}
	gr := runCLI(t, "go", roots[1], goArgs...)
	if !tsLive() {
		return // the twin is Go itself (golden_test.go): Go's run is pinned by the transcript
	}
	label := strings.Join(c.args, " ")
	if ts.code != gr.code {
		t.Errorf("%s: exit ts=%d go=%d\nts: %s%s\ngo: %s%s", label, ts.code, gr.code, ts.stdout, ts.stderr, gr.stdout, gr.stderr)
		return
	}
	a, b := normalize(t, ts.stdout, roots[0]), normalize(t, gr.stdout, roots[1])
	for _, p := range c.goOnly {
		_, inTS := jsonAt(a, p)
		_, inGo := jsonAt(b, p)
		if inTS || !inGo {
			t.Errorf("%s: %s is a named Go-only difference, but ts has it: %v, go has it: %v", label, p, inTS, inGo)
		}
	}
	skip := func(p string) bool {
		return strings.HasSuffix(p, ".prefix") || (c.goArgs != nil && strings.HasSuffix(p, ".issuer")) || (c.skip != nil && c.skip(p)) ||
			slices.Contains(c.goOnly, p)
	}
	if d := harness.DiffJSON("", a, b, skip); len(d) > 0 {
		if len(d) > 20 {
			d = append(d[:20], "...")
		}
		t.Errorf("%s differs:\n  %s", label, strings.Join(d, "\n  "))
	}
}

func TestCLIInitAndKeys(t *testing.T) {
	pinned(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	errorMessage := func(p string) bool { return p == ".error.message" }
	for _, c := range []cliCase{
		{args: []string{"--json", "init"}},
		{args: []string{"--json", "keys", "list"}},
		{args: []string{"--json", "keys", "create", "--name", "ci", "--scopes", "models:read,inference:chat"}},
		{args: []string{"--json", "keys", "create"}},
		{args: []string{"--json", "keys", "create", "--scopes", ""}},
		{args: []string{"--json", "keys", "create", "--scopes", "bogus"}, skip: errorMessage},
		{args: []string{"--json", "keys", "create", "--expires-days", "0"}, skip: errorMessage},
		{args: []string{"--json", "keys", "create", "--name"}, skip: errorMessage},
		{args: []string{"--json", "keys", "revoke", "key_missing"}},
		{args: []string{"--json", "keys", "rotate"}, skip: errorMessage},
		{args: []string{"--json", "keys", "bogus"}, skip: errorMessage},
		{args: []string{"--json", "keys", "list"}},
	} {
		compareCLI(t, roots, c)
	}
	// Mutations on the first key of each root.
	ids := [2]string{}
	for i, impl := range []string{"ts", "go"} {
		r := runCLI(t, impl, roots[i], "--json", "keys", "list")
		var env struct {
			Data struct{ Keys []struct{ ID string } }
		}
		json.Unmarshal([]byte(r.stdout), &env)
		if len(env.Data.Keys) == 0 {
			t.Fatalf("%s: no keys", impl)
		}
		ids[i] = env.Data.Keys[0].ID
	}
	for _, args := range [][]string{{"keys", "scopes", "", "--scopes", "inference:speech,models:read"}, {"keys", "rotate", ""}, {"keys", "revoke", ""}} {
		var outs [2]any
		for i, impl := range []string{"ts", "go"} {
			a := append([]string{"--json"}, args...)
			a[3] = ids[i]
			r := runCLI(t, impl, roots[i], a...)
			if r.code != 0 {
				t.Fatalf("%s %v: %d %s %s", impl, a, r.code, r.stdout, r.stderr)
			}
			outs[i] = normalize(t, r.stdout, roots[i])
		}
		if d := harness.DiffJSON("", outs[0], outs[1], func(p string) bool { return strings.HasSuffix(p, ".prefix") }); len(d) > 0 {
			t.Errorf("%v differs: %v", args, d)
		}
	}
}

func TestCLIModels(t *testing.T) {
	pinned(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	for i, impl := range []string{"ts", "go"} {
		if r := runCLI(t, impl, roots[i], "--json", "init"); r.code != 0 {
			t.Fatalf("%s init: %s %s", impl, r.stdout, r.stderr)
		}
	}
	for _, c := range []cliCase{
		{args: []string{"--json", "models", "catalog"}},
		{args: []string{"--json", "models", "catalog", "--kind", "image", "--sort", "memory"}},
		{args: []string{"--json", "models", "catalog", "--capability", "tool-calling", "--fits"}},
		{args: []string{"--json", "models", "catalog", "--search", "qwen coder", "--sort", "context", "--reverse"}},
		{args: []string{"--json", "models", "catalog", "--license", "open", "--license", "conditional", "--sort", "name"}},
		{args: []string{"--json", "models", "catalog", "--no-installed", "--kind", "stt"}},
		{args: []string{"--json", "models", "catalog", "--kind", "bogus"}, skip: func(p string) bool { return p == ".error.message" }},
		{args: []string{"--json", "models", "recommend"}},
		{args: []string{"--json", "models", "recommend", "--kind", "stt", "--vram", "8"}},
		{args: []string{"--json", "models", "recommend", "--kind", "image", "--vram", "24"}},
		{args: []string{"--json", "models", "list"}},
		{args: []string{"--json", "models", "list", "--kind", "llm"}},
		{args: []string{"--json", "models", "show", "qwen2.5-coder-1.5b-instruct-q4_k_m"}},
		{args: []string{"--json", "models", "show", "qwen3-tts-1.7b-base-q4_k_m"}},
		{args: []string{"--json", "models", "show", "wan2.1-t2v-1.3b-q8_0"}},
		{args: []string{"--json", "models", "show", "qwen3-embedding-0.6b-q8_0"}},
		{args: []string{"--json", "models", "show", "nope"}},
		{args: []string{"--json", "models", "install", "nope"}},
		{args: []string{"--json", "doctor"}},
		{args: []string{"--json", "reset"}, skip: func(p string) bool { return p == ".error.message" }},
		{args: []string{"--json", "reset", "--yes"}},
		{args: []string{"--json", "keys", "list"}},
		{args: []string{"--json", "uninstall"}, skip: func(p string) bool { return p == ".error.message" }},
		{args: []string{"--json", "uninstall", "-y"}, goOnly: []string{goOnlyUninstallReadCopies}},
		{args: []string{"--json", "reset", "--yes"}, skip: func(p string) bool { return p == ".error.message" }},
	} {
		compareCLI(t, roots, c)
	}
}

func TestCLIConfigure(t *testing.T) {
	pinned(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	msg := func(p string) bool { return p == ".error.message" }
	tomlPath := filepath.Join(filepath.Dir(ts), "overrides.toml")
	fsguard.WriteFile(tomlPath, []byte("ctxSize = 65536\nparallel = 2\nselectedSttModels = []\nactiveSttModel = \"\"\n[memory.systemReserve]\npercent = 20\n"), 0o600)
	badToml := filepath.Join(filepath.Dir(ts), "bad.toml")
	fsguard.WriteFile(badToml, []byte("bogus = 1\n"), 0o600)
	for _, c := range []cliCase{
		{args: []string{"--json", "configure"}, skip: msg},
		{args: []string{"--json", "configure", "--defaults", "--no-create-key"}},
		{args: []string{"--json", "configure", "--parallel", "3", "--ctx-mode", "explicit"}},
		// A kept former entry is not a built-in id: both refuse it before
		// the root's catalog loads (TS assertCatalogModels, #368).
		{args: []string{"--json", "configure", "--llm-models", "qwen2.5-coder-1.5b-instruct-q4_k_m,clef-flash-q4_k_m", "--active-llm", "clef-flash-q4_k_m"}},
		{args: []string{"--json", "configure", "--llm-models", "qwen3.5-4b-q4_k_m,clef-flash-q4_k_m", "--active-llm", "clef-flash-q4_k_m"}},
		{args: []string{"--json", "configure", "--stt-models", "", "--tts-models", "qwen3-tts-1.7b-base-q4_k_m"}},
		{args: []string{"--json", "configure", "--config", tomlPath}},
		{args: []string{"--json", "config", "show"}}, // the memory change marked a pending restart
		{args: []string{"--json", "configure", "--config", badToml}, skip: msg},
		{args: []string{"--json", "configure", "--llm-models", "nope"}, skip: msg},
		{args: []string{"--json", "configure", "--parallel", "9"}, skip: msg},
		{args: []string{"--json", "configure", "--active-llm", "qwen3-tts-1.7b-base-q4_k_m"}, skip: msg},
		{args: []string{"--json", "configure", "--tts-models", "kokoro-82m-v1.0-q8"}, skip: msg},
		{args: []string{"--json", "configure", "--otel-endpoint", "ftp://x"}, skip: msg},
		{args: []string{"--json", "configure", "--otel-endpoint", "http://collector:4318", "--otel-headers", "x-token=abc", "--otel-sample-ratio", "50"}},
		{args: []string{"--json", "configure", "--hf-token", "hf_test"}},
		{args: []string{"--json", "configure", "--create-key"}, skip: func(p string) bool { return strings.HasPrefix(p, ".data.createdKey") }},
	} {
		compareCLI(t, roots, c)
	}
}

// TestCLILogs reads each launched gateway's log with both CLIs: the same
// file must give the same events. Go's answers are checked on their own too: the log holds what
// the package's other tests did to the shared gateway, so it can't be pinned as a transcript.
func TestCLILogs(t *testing.T) {
	eachCLI(t)
	for _, tg := range harness.Targets(t) {
		if tg.Root == "" {
			continue
		}
		harness.Do(t, "GET", tg.BaseURL+"/v1/models", harness.Bearer(tg.Key), "")
		harness.Do(t, "GET", tg.BaseURL+"/nope", harness.Bearer(tg.Key), "")
		checkGoLogs(t, tg)
		for _, args := range [][]string{{"--json", "logs"}, {"--json", "logs", "--limit", "2"}, {"--json", "logs", "--level", "warn"},
			{"--json", "logs", "--runtime", "gateway", "--since", "2020-01-01T00:00:00Z"}, {"--json", "logs", "--limit", "0"},
			{"--json", "logs", "--level", "loud"}} {
			gr := runCLI(t, "go", tg.Root, args...)
			if !tsLive() {
				continue
			}
			ts := runCLI(t, "ts", tg.Root, args...)
			if ts.code != gr.code {
				t.Errorf("%s %v: exit ts=%d go=%d", tg.Name, args, ts.code, gr.code)
				continue
			}
			skip := func(p string) bool { return p == ".error.message" }
			if d := harness.DiffJSON("", normalize(t, ts.stdout, tg.Root), normalize(t, gr.stdout, tg.Root), skip); len(d) > 0 {
				if len(d) > 10 {
					d = d[:10]
				}
				t.Errorf("%s %v differs:\n  %s", tg.Name, args, strings.Join(d, "\n  "))
			}
		}
	}
}

// eachCLI starts a test that runs each target's own CLI against its own gateway and checks what it
// says: under -impl=go Go's, under -impl=both TS's too. It compares neither with the other.
func eachCLI(t *testing.T) {
	t.Helper()
	switch harness.Impl() {
	case "both":
		harness.RequireTS(t)
	case "go":
	default:
		t.Skip("runs the Go CLI")
	}
}

// checkGoLogs checks what Go's logs command says about tg's log: every filter holds on what it
// returns, and the requests just made are there.
func checkGoLogs(t *testing.T, tg *harness.Target) {
	t.Helper()
	type event struct {
		Timestamp, Severity, Runtime string
		HTTP                         *struct {
			Method, Path string
			Status       int
		}
	}
	read := func(args ...string) []event {
		t.Helper()
		r := runCLI(t, "go", tg.Root, append([]string{"--json", "logs"}, args...)...)
		var env struct {
			OK   bool                     `json:"ok"`
			Data struct{ Events []event } `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || r.code != 0 || !env.OK {
			t.Fatalf("go logs %v: exit %d %v %s %s", args, r.code, err, r.stdout, r.stderr)
		}
		return env.Data.Events
	}
	all := read("--limit", "5000")
	var models, nope bool
	var seen []string
	for _, e := range all {
		if e.HTTP != nil {
			seen = append(seen, fmt.Sprintf("%s %s %d", e.HTTP.Method, e.HTTP.Path, e.HTTP.Status))
			models = models || e.HTTP.Method == "GET" && e.HTTP.Path == "/v1/models" && e.HTTP.Status == 200
			// An unmatched path is logged by name, never as sent.
			nope = nope || e.HTTP.Method == "GET" && e.HTTP.Path == "unmatched-route" && e.HTTP.Status == 404
		}
	}
	if !models || !nope {
		t.Errorf("go logs: GET /v1/models 200 %v, GET unmatched-route 404 %v, in %d events; requests: %v", models, nope, len(all), seen)
	}
	if n := len(read()); n == 0 || n > 200 || n > len(all) {
		t.Errorf("go logs: %d events by default (200 at most), of %d", n, len(all))
	}
	if n := len(read("--limit", "2")); n != min(2, len(all)) {
		t.Errorf("go logs --limit 2: %d events", n)
	}
	for _, e := range read("--level", "warn") {
		if e.Severity != "warn" {
			t.Errorf("go logs --level warn: a %s event", e.Severity)
		}
	}
	for _, e := range read("--runtime", "gateway", "--since", "2020-01-01T00:00:00Z") {
		if e.Runtime != "gateway" || e.Timestamp < "2020-01-01T00:00:00Z" {
			t.Errorf("go logs --runtime gateway --since 2020: a %s event at %s", e.Runtime, e.Timestamp)
		}
	}
	for _, bad := range [][]string{{"--limit", "0"}, {"--level", "loud"}} {
		r := runCLI(t, "go", tg.Root, append([]string{"--json", "logs"}, bad...)...)
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || r.code == 0 || env.OK || !strings.Contains(env.Error.Message, bad[0][2:]) {
			t.Errorf("go logs %v: exit %d %v %s", bad, r.code, err, r.stdout)
		}
	}
}

// TestCLISmoke runs each CLI's smoke check against its own gateway.
func TestCLISmoke(t *testing.T) {
	eachCLI(t)
	for _, tg := range harness.Targets(t) {
		if tg.Root == "" || !tg.RealModel {
			continue
		}
		r := runCLI(t, tg.Name, tg.Root, "--json", "smoke", "--timeout", "120")
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Passed bool `json:"passed"`
				Steps  []struct {
					Name string `json:"name"`
					OK   bool   `json:"ok"`
				} `json:"steps"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &env); err != nil || r.code != 0 || !env.Data.Passed || len(env.Data.Steps) != 2 {
			t.Errorf("%s smoke: exit %d %s %s", tg.Name, r.code, r.stdout, r.stderr)
		}
		// The temporary key is gone afterwards.
		keys := runCLI(t, tg.Name, tg.Root, "--json", "keys", "list")
		if strings.Contains(keys.stdout, "smoke (temporary)") {
			t.Errorf("%s: smoke key left behind", tg.Name)
		}
	}
}
