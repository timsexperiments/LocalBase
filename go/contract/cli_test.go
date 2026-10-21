package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// CLI contract: the same command through both binaries, on twin scratch
// roots, must give the same exit code and the same JSON envelope apart from
// volatile values (ids, secrets, timestamps, the root path).

var (
	goCLIOnce sync.Once
	goCLIPath string
	goCLIErr  error
)

func goCLI(t *testing.T) string {
	goCLIOnce.Do(func() {
		dir, err := scratchTemp("lbgo-cli-bin-")
		if err != nil {
			goCLIErr = err
			return
		}
		goCLIPath = filepath.Join(dir, "local-base")
		if out, err := exec.Command("go", "build", "-o", goCLIPath, "../cmd/localbase").CombinedOutput(); err != nil {
			goCLIErr = err
			t.Log(string(out))
		}
	})
	if goCLIErr != nil {
		t.Fatal(goCLIErr)
	}
	return goCLIPath
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
	return append(env, "LOCALBASE_CATALOG_FEED_URL=off", "NO_COLOR=1")
}

func runCLI(t *testing.T, impl, root string, args ...string) cliResult {
	t.Helper()
	return runCLIEnv(t, impl, root, nil, args...)
}

// runCLIEnv runs a command with env entries replacing cleanEnv's.
func runCLIEnv(t *testing.T, impl, root string, env []string, args ...string) cliResult {
	t.Helper()
	full := append([]string{"--root", root}, args...)
	var cmd *exec.Cmd
	if impl == "ts" {
		cmd = exec.Command("bun", append([]string{"run", "src/cli.ts"}, full...)...)
		cmd.Dir = repoRoot()
	} else {
		cmd = exec.Command(goCLI(t), full...)
	}
	cmd.Env = cleanEnv()
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
	return cliResult{code, out.String(), errb.String()}
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
	args []string
	skip func(path string) bool
}

func twinRoots(t *testing.T) (string, string) {
	base := scratchDir(t)
	base, _ = filepath.EvalSymlinks(base)
	return filepath.Join(base, "ts"), filepath.Join(base, "go")
}

func compareCLI(t *testing.T, roots [2]string, c cliCase) {
	t.Helper()
	ts := runCLI(t, "ts", roots[0], c.args...)
	gr := runCLI(t, "go", roots[1], c.args...)
	label := strings.Join(c.args, " ")
	if ts.code != gr.code {
		t.Errorf("%s: exit ts=%d go=%d\nts: %s%s\ngo: %s%s", label, ts.code, gr.code, ts.stdout, ts.stderr, gr.stdout, gr.stderr)
		return
	}
	a, b := normalize(t, ts.stdout, roots[0]), normalize(t, gr.stdout, roots[1])
	skip := func(p string) bool { return strings.HasSuffix(p, ".prefix") || (c.skip != nil && c.skip(p)) }
	if d := diffJSON("", a, b, skip); len(d) > 0 {
		if len(d) > 20 {
			d = append(d[:20], "...")
		}
		t.Errorf("%s differs:\n  %s", label, strings.Join(d, "\n  "))
	}
}

func requireTS(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is not installed")
	}
	if *implFlag != "both" {
		t.Skip("run with -impl=both")
	}
}

func TestCLIInitAndKeys(t *testing.T) {
	requireTS(t)
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
		if d := diffJSON("", outs[0], outs[1], func(p string) bool { return strings.HasSuffix(p, ".prefix") }); len(d) > 0 {
			t.Errorf("%v differs: %v", args, d)
		}
	}
}

func TestCLIModels(t *testing.T) {
	requireTS(t)
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
		{args: []string{"--json", "uninstall", "-y"}},
		{args: []string{"--json", "reset", "--yes"}, skip: func(p string) bool { return p == ".error.message" }},
	} {
		compareCLI(t, roots, c)
	}
}

func TestCLIConfigure(t *testing.T) {
	requireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	msg := func(p string) bool { return p == ".error.message" }
	tomlPath := filepath.Join(filepath.Dir(ts), "overrides.toml")
	mustWriteFile(tomlPath, []byte("ctxSize = 65536\nparallel = 2\nselectedSttModels = []\nactiveSttModel = \"\"\n[memory.systemReserve]\npercent = 20\n"), 0o600)
	badToml := filepath.Join(filepath.Dir(ts), "bad.toml")
	mustWriteFile(badToml, []byte("bogus = 1\n"), 0o600)
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

// TestTSReadsGoLogs runs the TS `logs` command over the Go gateway's event
// file: every line Go wrote must pass the TS event schema.
func TestTSReadsGoLogs(t *testing.T) {
	requireTS(t)
	var goT *Target
	for _, tg := range targets {
		if tg.Name == "go" {
			goT = tg
		}
	}
	if goT == nil {
		t.Skip("no Go target")
	}
	// Make a few events of different kinds.
	do(t, "GET", goT.BaseURL+"/v1/models", bearer(goT.Key), "")
	do(t, "GET", goT.BaseURL+"/nope", bearer(goT.Key), "")
	do(t, "POST", goT.BaseURL+"/v1/chat/completions", bearer(goT.Key), `{"model":"x","messages":[]}`)
	do(t, "GET", goT.BaseURL+"/app/models", nil, "")
	raw, err := os.ReadFile(filepath.Join(goT.Root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1
	r := runCLI(t, "ts", goT.Root, "--json", "logs", "--limit", "5000")
	if r.code != 0 {
		t.Fatalf("ts logs: %d %s %s", r.code, r.stdout, r.stderr)
	}
	var env struct {
		Data struct{ Events []map[string]any }
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Events) != lines {
		t.Errorf("TS read %d of %d Go events; stderr: %s", len(env.Data.Events), lines, r.stderr)
	}
}

// TestCLILogs reads each launched gateway's log with both CLIs: the same
// file must give the same events.
func TestCLILogs(t *testing.T) {
	requireTS(t)
	for _, tg := range targets {
		if tg.Root == "" {
			continue
		}
		do(t, "GET", tg.BaseURL+"/v1/models", bearer(tg.Key), "")
		do(t, "GET", tg.BaseURL+"/nope", bearer(tg.Key), "")
		for _, args := range [][]string{{"--json", "logs"}, {"--json", "logs", "--limit", "2"}, {"--json", "logs", "--level", "warn"},
			{"--json", "logs", "--runtime", "gateway", "--since", "2020-01-01T00:00:00Z"}, {"--json", "logs", "--limit", "0"},
			{"--json", "logs", "--level", "loud"}} {
			ts := runCLI(t, "ts", tg.Root, args...)
			gr := runCLI(t, "go", tg.Root, args...)
			if ts.code != gr.code {
				t.Errorf("%s %v: exit ts=%d go=%d", tg.Name, args, ts.code, gr.code)
				continue
			}
			skip := func(p string) bool { return p == ".error.message" }
			if d := diffJSON("", normalize(t, ts.stdout, tg.Root), normalize(t, gr.stdout, tg.Root), skip); len(d) > 0 {
				if len(d) > 10 {
					d = d[:10]
				}
				t.Errorf("%s %v differs:\n  %s", tg.Name, args, strings.Join(d, "\n  "))
			}
		}
	}
}

// TestCLISmoke runs each CLI's smoke check against its own gateway.
func TestCLISmoke(t *testing.T) {
	requireTS(t)
	for _, tg := range targets {
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
