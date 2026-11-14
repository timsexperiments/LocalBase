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

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// The CLI tests run Go's command line on scratch roots. The pinned ones (golden_test.go) compare
// every run with a transcript golden; until step 7 they compared it with TS's.

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

func runCLI(t *testing.T, root string, args ...string) cliResult {
	t.Helper()
	return runCLIEnv(t, root, nil, args...)
}

// runCLIEnv runs a command with env entries replacing cleanEnv's.
func runCLIEnv(t *testing.T, root string, env []string, args ...string) cliResult {
	t.Helper()
	full := append([]string{"--root", root}, args...)
	p := pinOf(t)
	var cmd *exec.Cmd
	if p != nil {
		// A pinned test's runs are the lab build on the fixed host (golden_test.go).
		cmd = exec.Command(goLabCLI(t), full...)
		env = append([]string{"LOCALBASE_LAB_HOST_FILE=" + p.host}, env...)
	} else {
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
		p.record(t, root, args, res)
	}
	return res
}

var volatileValue = regexp.MustCompile(`key_[0-9a-f-]{36}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|lb_[A-Za-z0-9_-]{32}|\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)

// cliRoot is a pinned test's root: <scratch>/go, the Go half of the twin roots R3a recorded on.
func cliRoot(t *testing.T) string {
	base := fsguard.ScratchDir(t)
	base, _ = filepath.EvalSymlinks(base)
	return filepath.Join(base, "go")
}

// initRoot runs `init` on a pinned test's root.
func initRoot(t *testing.T, root string) {
	t.Helper()
	if res := runCLI(t, root, "--json", "init"); res.code != 0 {
		t.Fatalf("init: %s%s", res.stdout, res.stderr)
	}
}

// runCase runs one command of a pinned test, which its transcript pins.
func runCase(t *testing.T, root string, args ...string) cliResult {
	t.Helper()
	return runCLI(t, root, args...)
}

func TestCLIInitAndKeys(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	for _, args := range [][]string{
		{"--json", "init"},
		{"--json", "keys", "list"},
		{"--json", "keys", "create", "--name", "ci", "--scopes", "models:read,inference:chat"},
		{"--json", "keys", "create"},
		{"--json", "keys", "create", "--scopes", ""},
		{"--json", "keys", "create", "--scopes", "bogus"},
		{"--json", "keys", "create", "--expires-days", "0"},
		{"--json", "keys", "create", "--name"},
		{"--json", "keys", "revoke", "key_missing"},
		{"--json", "keys", "rotate"},
		{"--json", "keys", "bogus"},
		{"--json", "keys", "list"},
	} {
		runCase(t, gr, args...)
	}
	// Mutations on the first key.
	r := runCLI(t, gr, "--json", "keys", "list")
	var env struct {
		Data struct{ Keys []struct{ ID string } }
	}
	json.Unmarshal([]byte(r.stdout), &env)
	if len(env.Data.Keys) == 0 {
		t.Fatalf("no keys: %s", r.stdout)
	}
	for _, args := range [][]string{{"keys", "scopes", "", "--scopes", "inference:speech,models:read"}, {"keys", "rotate", ""}, {"keys", "revoke", ""}} {
		a := append([]string{"--json"}, args...)
		a[3] = env.Data.Keys[0].ID
		if r := runCLI(t, gr, a...); r.code != 0 {
			t.Fatalf("%v: %d %s %s", a, r.code, r.stdout, r.stderr)
		}
	}
}

func TestCLIModels(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	initRoot(t, gr)
	for _, args := range [][]string{
		{"--json", "models", "catalog"},
		{"--json", "models", "catalog", "--kind", "image", "--sort", "memory"},
		{"--json", "models", "catalog", "--capability", "tool-calling", "--fits"},
		{"--json", "models", "catalog", "--search", "qwen coder", "--sort", "context", "--reverse"},
		{"--json", "models", "catalog", "--license", "open", "--license", "conditional", "--sort", "name"},
		{"--json", "models", "catalog", "--no-installed", "--kind", "stt"},
		{"--json", "models", "catalog", "--kind", "bogus"},
		{"--json", "models", "recommend"},
		{"--json", "models", "recommend", "--kind", "stt", "--vram", "8"},
		{"--json", "models", "recommend", "--kind", "image", "--vram", "24"},
		{"--json", "models", "list"},
		{"--json", "models", "list", "--kind", "llm"},
		{"--json", "models", "show", "qwen2.5-coder-1.5b-instruct-q4_k_m"},
		{"--json", "models", "show", "qwen3-tts-1.7b-base-q4_k_m"},
		{"--json", "models", "show", "wan2.1-t2v-1.3b-q8_0"},
		{"--json", "models", "show", "qwen3-embedding-0.6b-q8_0"},
		{"--json", "models", "show", "nope"},
		{"--json", "models", "install", "nope"},
		{"--json", "doctor"},
		{"--json", "reset"},
		{"--json", "reset", "--yes"},
		{"--json", "keys", "list"},
		{"--json", "uninstall"},
		{"--json", "uninstall", "-y"},
		{"--json", "reset", "--yes"},
	} {
		runCase(t, gr, args...)
	}
}

func TestCLIConfigure(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	tomlPath := filepath.Join(filepath.Dir(gr), "overrides.toml")
	fsguard.WriteFile(tomlPath, []byte("ctxSize = 65536\nparallel = 2\nselectedSttModels = []\nactiveSttModel = \"\"\n[memory.systemReserve]\npercent = 20\n"), 0o600)
	badToml := filepath.Join(filepath.Dir(gr), "bad.toml")
	fsguard.WriteFile(badToml, []byte("bogus = 1\n"), 0o600)
	for _, args := range [][]string{
		{"--json", "configure"},
		{"--json", "configure", "--defaults", "--no-create-key"},
		{"--json", "configure", "--parallel", "3", "--ctx-mode", "explicit"},
		// A kept former entry is not a built-in id: both refuse it before
		// the root's catalog loads (TS assertCatalogModels, #368).
		{"--json", "configure", "--llm-models", "qwen2.5-coder-1.5b-instruct-q4_k_m,clef-flash-q4_k_m", "--active-llm", "clef-flash-q4_k_m"},
		{"--json", "configure", "--llm-models", "qwen3.5-4b-q4_k_m,clef-flash-q4_k_m", "--active-llm", "clef-flash-q4_k_m"},
		{"--json", "configure", "--stt-models", "", "--tts-models", "qwen3-tts-1.7b-base-q4_k_m"},
		{"--json", "configure", "--config", tomlPath},
		{"--json", "config", "show"}, // the memory change marked a pending restart
		{"--json", "configure", "--config", badToml},
		{"--json", "configure", "--llm-models", "nope"},
		{"--json", "configure", "--parallel", "9"},
		{"--json", "configure", "--active-llm", "qwen3-tts-1.7b-base-q4_k_m"},
		{"--json", "configure", "--tts-models", "kokoro-82m-v1.0-q8"},
		{"--json", "configure", "--otel-endpoint", "ftp://x"},
		{"--json", "configure", "--otel-endpoint", "http://collector:4318", "--otel-headers", "x-token=abc", "--otel-sample-ratio", "50"},
		{"--json", "configure", "--hf-token", "hf_test"},
		{"--json", "configure", "--create-key"},
	} {
		runCase(t, gr, args...)
	}
}

// TestCLILogs reads the shared gateway's log with Go's logs command and checks its filters and
// refusals itself: the log holds what the package's other tests did to the gateway, so it can't
// be pinned as a transcript.
func TestCLILogs(t *testing.T) {
	for _, tg := range harness.Targets(t) {
		if tg.Root == "" {
			continue
		}
		harness.Do(t, "GET", tg.BaseURL+"/v1/models", harness.Bearer(tg.Key), "")
		harness.Do(t, "GET", tg.BaseURL+"/nope", harness.Bearer(tg.Key), "")
		checkGoLogs(t, tg)
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
		r := runCLI(t, tg.Root, append([]string{"--json", "logs"}, args...)...)
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
		r := runCLI(t, tg.Root, append([]string{"--json", "logs"}, bad...)...)
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

// TestCLISmoke runs the smoke check against the shared gateway, with a real model.
func TestCLISmoke(t *testing.T) {
	for _, tg := range harness.Targets(t) {
		if tg.Root == "" || !tg.RealModel {
			continue
		}
		r := runCLI(t, tg.Root, "--json", "smoke", "--timeout", "120")
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
		keys := runCLI(t, tg.Root, "--json", "keys", "list")
		if strings.Contains(keys.stdout, "smoke (temporary)") {
			t.Errorf("%s: smoke key left behind", tg.Name)
		}
	}
}
