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
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
)

// TestCLIConfigDeclarative compares `config validate|plan|apply|show` on twin
// roots: documents, every validation message, plans, pending restarts,
// apply on a fresh root, and the human output. No case can restart a
// service: restart-required applies use --restart never, and the automatic
// restart refusal runs against the foreground contract gateways.
func TestCLIConfigDeclarative(t *testing.T) {
	requireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	docs := t.TempDir()
	for _, r := range roots {
		if res := runCLI(t, "go", r, "--json", "init"); res.code != 0 {
			t.Fatalf("init: %s", res.stderr)
		}
	}
	compareCLI(t, roots, cliCase{args: []string{"--json", "config", "show"}})
	shown := runCLI(t, "go", gr, "--json", "config", "show")
	var env struct {
		Data struct{ Document string } `json:"data"`
	}
	json.Unmarshal([]byte(shown.stdout), &env)
	base := env.Data.Document
	if !strings.HasPrefix(base, "# Secrets and process overrides") {
		t.Fatalf("show: %s", shown.stdout)
	}
	edit := func(name string, pairs ...string) string {
		doc := base
		for i := 0; i < len(pairs); i += 2 {
			re := regexp.MustCompile(`(?m)` + pairs[i])
			if !re.MatchString(doc) {
				t.Fatalf("%s: %q does not match", name, pairs[i])
			}
			doc = re.ReplaceAllString(doc, pairs[i+1])
		}
		p := filepath.Join(docs, name+".toml")
		os.WriteFile(p, []byte(doc), 0o600)
		return p
	}
	write := func(name, doc string) string {
		p := filepath.Join(docs, name+".toml")
		os.WriteFile(p, []byte(doc), 0o600)
		return p
	}
	basePath := edit("base")
	change := edit("change", `^port = 2273$`, "port = 2274", `^percent = 15$`, "percent = 12.5",
		`^ctxSize = 131072$`, "ctxSize = 65536", `^parallel = "auto"$`, "parallel = 2")
	hot := edit("hot", `^ctxSize = 131072$`, "ctxSize = 32768", `^selectedTtsModels = \[\]$`, `selectedTtsModels = ["qwen3-tts-1.7b-base-q4_k_m"]`)
	invalid := map[string]string{
		"syntax":    write("syntax", "version = 1\nthis is not toml = \n"),
		"v2":        edit("v2", `^version = 1$`, "version = 2"),
		"vstring":   edit("vstring", `^version = 1$`, `version = "1"`),
		"only":      write("only", "version = 1\n"),
		"bad1":      write("bad1", "version = 1\n[gateway]\nhost = \"a b\"\nport = 0\nextra = 1\n"),
		"ws":        edit("ws", `^host = "127.0.0.1"$`, `host = " x"`),
		"empty":     edit("empty", `^host = "127.0.0.1"$`, `host = ""`),
		"long":      edit("long", `^host = "127.0.0.1"$`, `host = "`+strings.Repeat("a", 254)+`"`),
		"hostnum":   edit("hostnum", `^host = "127.0.0.1"$`, `host = 5`),
		"port0":     edit("port0", `^port = 2273$`, "port = 0"),
		"portbig":   edit("portbig", `^port = 2273$`, "port = 70000"),
		"portfloat": edit("portfloat", `^port = 18000$`, "port = 18000.5"),
		"portstr":   edit("portstr", `^port = 18000$`, `port = "18000"`),
		"extra":     edit("extra", `^port = 2273$`, "port = 2273\nzzz = 1\naaa = 2\n\"1\" = 3"),
		"rootextra": edit("rootextra", `^version = 1$`, "version = 1\nextra = true"),
		"subtable":  edit("subtable", `^\[gateway\]$`, "[gateway.sub]\nx = 1\n[gateway]"),
		"ctxmode":   edit("ctxmode", `^ctxMode = "auto"$`, `ctxMode = "fast"`),
		"noctxmode": edit("noctxmode", `^ctxMode = "auto"\n`, ""),
		"par5":      edit("par5", `^parallel = "auto"$`, "parallel = 5"),
		"ctxsmall":  edit("ctxsmall", `^ctxSize = 131072$`, "ctxSize = 1024"),
		"ctxbig":    edit("ctxbig", `^ctxSize = 131072$`, "ctxSize = 3000000000"),
		"pct":       edit("pct", `^percent = 15$`, "percent = 101"),
		"pctinf":    edit("pctinf", `^percent = 15$`, "percent = inf"),
		"pctnan":    edit("pctnan", `^percent = 15$`, "percent = nan"),
		"minneg":    edit("minneg", `^minimumGb = 8$`, "minimumGb = -1"),
		"memextra":  edit("memextra", `^\[memory\]$`, "[memory]\nfoo = 1"),
		"missing":   edit("missing", `(?s)\[memory\.acceleratorReserve\].*`, ""),
		"allowexp":  edit("allowexp", `^allowExperimental = false$`, `allowExperimental = "yes"`),
		"allownum":  edit("allownum", `^allowExperimental = false$`, "allowExperimental = 1", `^selectedTtsModels = \[\]$`, `selectedTtsModels = ["nope"]`),
		"modelsextra": edit("modelsextra", `^allowExperimental = false$`, "allowExperimental = false\nzzz = 1",
			`^selectedTtsModels = \[\]$`, `selectedTtsModels = ["nope"]`),
		"emptyid":      edit("emptyid", `^selectedTtsModels = \[\]$`, `selectedTtsModels = [""]`),
		"listtype":     edit("listtype", `^selectedTtsModels = \[\]$`, `selectedTtsModels = "x"`),
		"itemtype":     edit("itemtype", `^selectedTtsModels = \[\]$`, `selectedTtsModels = [1]`),
		"dup":          edit("dup", `^selectedTtsModels = \[\]$`, `selectedTtsModels = ["qwen3-tts-1.7b-base-q4_k_m", "qwen3-tts-1.7b-base-q4_k_m"]`),
		"experimental": edit("experimental", `^selectedTtsModels = \[\]$`, `selectedTtsModels = ["kokoro-82m-v1.0-q8"]`),
		"badmodels": edit("badmodels", `^selectedLlmModels = \["(.*)"\]$`, `selectedLlmModels = ["nope", "$1"]`,
			`^activeSttModel = ".*"$`, `activeSttModel = "qwen2.5-coder-7b-instruct-q4_k_m"`),
		"noactive":  edit("noactive", `^activeLlmModel = ".*"$`, `activeLlmModel = ""`, `^selectedTtsModels = \[\]$`, `selectedTtsModels = ["nope"]`),
		"nollm":     edit("nollm", `^selectedLlmModels = .*$`, "selectedLlmModels = []"),
		"activetts": edit("activetts", `^activeTtsModel = ""$`, `activeTtsModel = "nope"`),
		"video": edit("video", `^selectedVideoModels = \[\]$`, `selectedVideoModels = ["nope", "nope"]`,
			`^activeVideoModel = ""$`, `activeVideoModel = "nope"`),
	}
	names := make([]string, 0, len(invalid))
	for n := range invalid {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		compareCLI(t, roots, cliCase{args: []string{"--json", "config", "validate", "--file", invalid[n]}})
		// The raw message, before the CLI's redaction, also matches.
		compareRaw(t, invalid[n])
	}
	for _, args := range [][]string{
		{"--json", "config", "validate", "--file", basePath},
		{"--json", "config", "validate", "--file", change},
		{"--json", "config", "validate"},
		{"--json", "config", "validate", "--file", filepath.Join(docs, "absent.toml")},
		{"--json", "config", "plan", "--file", basePath},
		{"--json", "config", "plan", "--file", basePath, "--detailed-exit-code"},
		{"--json", "config", "plan", "--file", change},
		{"--json", "config", "plan", "--file", change, "--detailed-exit-code"},
		{"--json", "config", "plan", "--file", invalid["v2"]},
		{"--json", "config", "plan", basePath},
		{"--json", "config", "apply", "--file", basePath, "--restart", "sometimes"},
		// An unchanged document needs no service; --wait without a running
		// gateway is refused before anything is saved.
		{"--json", "config", "apply", "--file", basePath},
		{"--json", "config", "apply", "--file", hot, "--wait"},
		{"--json", "config", "apply", "--file", hot, "--restart", "never"},
		{"--json", "config", "plan", "--file", hot},
		{"--json", "config", "apply", "--file", change, "--restart", "never"},
		{"--json", "config", "plan", "--file", change, "--detailed-exit-code"},
		{"--json", "config", "plan", "--file", basePath},
		{"--json", "config", "show"},
		{"--json", "config", "apply", "--file", change, "--restart", "never"},
	} {
		compareCLI(t, roots, cliCase{args: args})
	}
	// Each implementation reads the other's pending-restart record.
	for _, impl := range []string{"ts", "go"} {
		other := map[string]string{"ts": gr, "go": ts}[impl]
		if res := runCLI(t, impl, other, "--json", "config", "show"); res.code != 0 || !strings.Contains(res.stdout, `"pendingRestart":true`) {
			t.Errorf("%s reading the other root: %d %s", impl, res.code, res.stdout)
		}
	}
	// Human output: plan lines and the rendered document.
	for _, args := range [][]string{
		{"config", "plan", "--file", basePath},
		{"config", "show"},
		{"config", "apply", "--file", basePath, "--restart", "never"},
	} {
		a, b := runCLI(t, "ts", ts, args...), runCLI(t, "go", gr, args...)
		if a.code != b.code || strings.ReplaceAll(a.stdout, ts, "<root>") != strings.ReplaceAll(b.stdout, gr, "<root>") ||
			strings.ReplaceAll(a.stderr, ts, "<root>") != strings.ReplaceAll(b.stderr, gr, "<root>") {
			t.Errorf("%v:\n ts %d %q %q\n go %d %q %q", args, a.code, a.stdout, a.stderr, b.code, b.stdout, b.stderr)
		}
	}
	// Apply to a root that does not exist yet creates it.
	fresh := [2]string{filepath.Join(filepath.Dir(ts), "fresh-ts"), filepath.Join(filepath.Dir(gr), "fresh-go")}
	compareCLI(t, fresh, cliCase{args: []string{"--json", "config", "show"}})
	compareCLI(t, fresh, cliCase{args: []string{"--json", "config", "plan", "--file", change}})
	compareCLI(t, fresh, cliCase{args: []string{"--json", "config", "apply", "--file", invalid["experimental"], "--restart", "never"}})
	compareCLI(t, fresh, cliCase{args: []string{"--json", "config", "apply", "--file", change, "--restart", "never"}})
	compareCLI(t, fresh, cliCase{args: []string{"--json", "config", "show"}})
	list := func(dir string) []string {
		var out []string
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if rel, _ := filepath.Rel(dir, p); rel != "." && !strings.HasPrefix(rel, "logs") {
				out = append(out, rel)
			}
			return nil
		})
		return out
	}
	if a, b := list(fresh[0]), list(fresh[1]); !slices.Equal(a, b) {
		t.Errorf("fresh root files:\n ts %v\n go %v", a, b)
	}
	// Stdin is the same document as the file.
	cmd := exec.Command(goCLI(t), "--root", gr, "--json", "config", "plan", "--file", "-")
	cmd.Env = cleanEnv()
	b, _ := os.ReadFile(change)
	cmd.Stdin = bytes.NewReader(b)
	out, _ := cmd.Output()
	if want := runCLI(t, "go", gr, "--json", "config", "plan", "--file", change).stdout; string(out) != want {
		t.Errorf("stdin plan:\n %s\nwant %s", out, want)
	}
}

// compareRaw compares the validation message before redaction (the CLI
// replaces messages containing "input:" and similar wholesale).
func compareRaw(t *testing.T, path string) {
	t.Helper()
	script := `import { readFileSync } from "node:fs";
import { parseConfiguration } from "./src/domains/config/declarative";
try { parseConfiguration(readFileSync(process.argv.at(-1)!, "utf8")); console.log("OK"); }
catch (e) { console.log((e as Error).message); }`
	cmd := exec.Command("bun", "-e", script, path)
	cmd.Dir = repoRoot()
	cmd.Env = cleanEnv()
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("bun: %v", err)
	}
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	got := "OK"
	if _, err := conf.ParseDocument(string(text), cat); err != nil {
		got = err.Error()
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("%s:\n go %s\n ts %s", filepath.Base(path), got, want)
	}
}

// TestCLIConfigForegroundGateway: with a foreground gateway on the root,
// a restart-required apply is refused before saving, and an unchanged
// document with --wait waits for its readiness.
func TestCLIConfigForegroundGateway(t *testing.T) {
	requireTS(t)
	for _, tg := range targets {
		t.Run(tg.Name, func(t *testing.T) {
			shown := runCLI(t, tg.Name, tg.Root, "--json", "config", "show")
			var env struct {
				Data struct{ Document string } `json:"data"`
			}
			json.Unmarshal([]byte(shown.stdout), &env)
			if env.Data.Document == "" {
				t.Fatalf("show: %s %s", shown.stdout, shown.stderr)
			}
			dir := t.TempDir()
			same := filepath.Join(dir, "same.toml")
			os.WriteFile(same, []byte(env.Data.Document), 0o600)
			moved := filepath.Join(dir, "moved.toml")
			os.WriteFile(moved, []byte(regexp.MustCompile(`(?m)^port = (\d+)$`).ReplaceAllString(env.Data.Document, "port = 1")), 0o600)
			res := runCLI(t, tg.Name, tg.Root, "--json", "config", "apply", "--file", moved)
			if res.code != 1 || !strings.Contains(res.stdout, "Cannot automatically restart this gateway") {
				t.Errorf("restart-required apply: %d %s", res.code, res.stdout)
			}
			res = runCLI(t, tg.Name, tg.Root, "--json", "config", "apply", "--file", same, "--wait")
			if res.code != 0 || !strings.Contains(res.stdout, `"activation":"unchanged"`) || !strings.Contains(res.stdout, `"readiness":"ready"`) {
				t.Errorf("unchanged apply --wait: %d %s %s", res.code, res.stdout, res.stderr)
			}
			if after := runCLI(t, tg.Name, tg.Root, "--json", "config", "show"); after.stdout != shown.stdout {
				t.Errorf("the configuration changed:\n %s\n %s", shown.stdout, after.stdout)
			}
		})
	}
}
