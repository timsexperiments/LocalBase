package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// mergeFuncs are where configure merges what the person gave (flags and a --config file) into the
// configuration. TestConfigureMergeTableCoversEverySetting reads them for every configuration field
// they set, so a setting the table doesn't cover fails it. When the merge moves, this list moves
// with it.
var mergeFuncs = []struct{ file, fn string }{{"configure.go", "mergeConfigure"}}

// mergeFieldsSet is every field of the configuration the merge functions assign, or take the
// address of to assign: cfg.X = …, cfg.X[k] = …, &cfg.X.
func mergeFieldsSet(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	found := 0
	for _, m := range mergeFuncs {
		f, err := parser.ParseFile(token.NewFileSet(), m.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != m.fn {
				continue
			}
			found++
			field := func(e ast.Expr) {
				if ix, ok := e.(*ast.IndexExpr); ok {
					e = ix.X
				}
				if sel, ok := e.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "cfg" {
						seen[sel.Sel.Name] = true
					}
				}
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, l := range n.Lhs {
						field(l)
					}
				case *ast.UnaryExpr:
					if n.Op == token.AND {
						field(n.X)
					}
				}
				return true
			})
		}
	}
	if found != len(mergeFuncs) {
		t.Fatalf("found %d of the merge functions %v", found, mergeFuncs)
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergeRoot is a configured scratch root, and how to run configure on it and read what it saved.
type mergeRoot struct {
	t    *testing.T
	root string
}

func newMergeRoot(t *testing.T) *mergeRoot {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("HF_TOKEN", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure --defaults: %s", out)
	}
	return &mergeRoot{t, root}
}

// configure runs configure with these flags, and a --config file holding toml when it isn't "".
func (m *mergeRoot) configure(toml string, flags ...string) (int, string) {
	m.t.Helper()
	args := []string{"--root", m.root, "configure", "--no-create-key"}
	if toml != "" {
		p := filepath.Join(m.t.TempDir(), "configure.toml")
		if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
			m.t.Fatal(err)
		}
		args = append(args, "--config", p)
	}
	return runApp(m.t, append(args, flags...)...)
}

func (m *mergeRoot) saved() *store.Config {
	m.t.Helper()
	st, err := store.Open(filepath.Join(m.root, "local-base.db"))
	if err != nil {
		m.t.Fatal(err)
	}
	defer st.Close()
	c, err := st.LoadConfig(context.Background())
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

// eligibleIDs is the models of a kind configure accepts for selection on this platform, in id
// order. The catalog is small (one model per purpose), so a kind can have one, or none.
func eligibleIDs(t *testing.T, kind string) []string {
	t.Helper()
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range cat.All() {
		if s.Kind != kind || (kind == "llm" && !conf.CanBeDefaultLLM(s)) {
			continue
		}
		if conf.Ineligible(s, false) == "" {
			ids = append(ids, s.ModelID)
		}
	}
	sort.Strings(ids)
	return ids
}

// twoLLMs is two chat models configure accepts. LLM eligibility doesn't depend on the platform
// (only video's does), so a catalog without two is a change to the catalog, and a failure here.
func twoLLMs(t *testing.T) (string, string) {
	t.Helper()
	ids := eligibleIDs(t, "llm")
	if len(ids) < 2 {
		t.Fatalf("the catalog has %d chat models configure accepts, and these tests need two: %v", len(ids), ids)
	}
	return ids[0], ids[1]
}

// mergeSetting is one setting the merge decides: how to read it from a saved configuration, its
// flag and its TOML line (either can be absent), and two values to give it.
type mergeSetting struct {
	field      string // the configuration field, for the coverage check
	read       func(*store.Config) string
	flag, toml func(v string) []string // flag args; TOML lines
	a, b       string
}

// scalarSettings are the settings that name no model: the same rows everywhere, and no catalog.
func scalarSettings() []mergeSetting {
	str := func(name, key, field string, read func(*store.Config) string, a, b string) mergeSetting {
		return mergeSetting{field, read,
			func(v string) []string { return []string{"--" + name, v} },
			func(v string) []string { return []string{fmt.Sprintf("%s = %q", key, v)} }, a, b}
	}
	num := func(name, key, field string, read func(*store.Config) string, a, b string) mergeSetting {
		return mergeSetting{field, read,
			func(v string) []string { return []string{"--" + name, v} },
			func(v string) []string { return []string{key + " = " + v} }, a, b}
	}
	out := []mergeSetting{
		str("host", "host", "Host", func(c *store.Config) string { return c.Host }, "127.0.0.2", "127.0.0.3"),
		str("stt-host", "sttHost", "SttHost", func(c *store.Config) string { return c.SttHost }, "127.0.0.4", "127.0.0.5"),
		str("ctx-mode", "ctxMode", "CtxMode", func(c *store.Config) string { return c.CtxMode }, "explicit", "auto"),
		str("otel-endpoint", "otelEndpoint", "OtelEndpoint", func(c *store.Config) string { return c.OtelEndpoint }, "http://127.0.0.1:4318", "http://127.0.0.1:4319"),
		str("otel-headers", "otelHeaders", "OtelHeaders", func(c *store.Config) string { return c.OtelHeaders }, "x-a=1", "x-b=2"),
		// The file's parallel is a number (or "auto"); a quoted number is refused there.
		num("parallel", "parallel", "Parallel", func(c *store.Config) string { return c.Parallel }, "2", "3"),
		num("port", "port", "Port", func(c *store.Config) string { return fmt.Sprint(c.Port) }, "18001", "18002"),
		num("stt-port", "sttPort", "SttPort", func(c *store.Config) string { return fmt.Sprint(c.SttPort) }, "18003", "18004"),
		num("ctx-size", "ctxSize", "CtxSize", func(c *store.Config) string { return fmt.Sprint(c.CtxSize) }, "4096", "8192"),
		num("otel-sample-ratio", "otelSampleRatio", "OtelSampleRatio", func(c *store.Config) string { return fmt.Sprint(c.OtelSampleRatio) }, "25", "50"),
		// The reserves have no flag: only the file sets them.
		{"SystemReserve", func(c *store.Config) string { return fmt.Sprint(c.SystemReserve.Percent) }, nil,
			func(v string) []string { return []string{"[memory.systemReserve]", "percent = " + v, "minimumGb = 4"} }, "20", "30"},
		{"AcceleratorReserve", func(c *store.Config) string { return fmt.Sprint(c.AcceleratorReserve.Percent) }, nil,
			func(v string) []string {
				return []string{"[memory.acceleratorReserve]", "percent = " + v, "minimumGb = 1"}
			}, "12", "14"},
	}
	return out
}

// modelSettings are the selected and active models of each kind, from the catalog.
func modelSettings(t *testing.T) []mergeSetting {
	var out []mergeSetting
	for _, k := range kinds {
		ids := eligibleIDs(t, k)
		title := fieldTitle(k)
		// A video model is eligible only on a platform its runtime supports (conf.Ineligible,
		// VideoTarget: darwin/arm64 today), and the --config file checks that. So video's row is
		// the empty selection on every platform, to keep the rows the same on macOS and Linux. The
		// precedence it would show is the one loop every kind shares, pinned by the other four.
		if k == "video" {
			ids = nil
		}
		if len(ids) == 0 {
			t.Logf("%s: no %s model in this row, so its Selected row is the empty selection only (Active needs two)", k, k)
		}
		// Two selections to tell apart: two models, or (for a kind with one) that model and none.
		a, b := "", ""
		if len(ids) > 0 {
			a = ids[0]
		}
		if len(ids) > 1 {
			b = ids[1]
		}
		list := func(v string) string {
			if v == "" {
				return "[]"
			}
			return fmt.Sprintf("[%q]", v)
		}
		out = append(out, mergeSetting{"Selected", func(c *store.Config) string { return strings.Join(c.Selected[k], ",") },
			func(v string) []string { return []string{"--" + k + "-models", v} },
			func(v string) []string { return []string{fmt.Sprintf("selected%sModels = %s", title, list(v))} }, a, b})
		// The active model needs two selected to choose between: llm, stt and image have them.
		if len(ids) > 1 {
			out = append(out, mergeSetting{"Active", func(c *store.Config) string { return c.Active[k] },
				func(v string) []string { return []string{"--" + k + "-models", a + "," + b, "--active-" + k, v} },
				func(v string) []string {
					return []string{fmt.Sprintf("selected%sModels = [%q, %q]", title, a, b), fmt.Sprintf("active%sModel = %q", title, v)}
				}, a, b})
		}
	}
	return out
}

func mergeSettings(t *testing.T) []mergeSetting { return append(scalarSettings(), modelSettings(t)...) }

// What configure does with each setting, given on the command line, in the --config file, in both,
// or in neither, pinned before the merge is moved out of runConfigure (5-4). The flag wins over the
// file; with neither, the saved value stands.
func TestConfigureMergesFlagsAndTheConfigFile(t *testing.T) {
	for _, s := range mergeSettings(t) {
		name := s.field
		if s.flag != nil {
			name += " " + s.flag("…")[0]
		}
		t.Run(name, func(t *testing.T) {
			m := newMergeRoot(t)
			before := s.read(m.saved())
			if code, out := m.configure(""); code != 0 {
				t.Fatalf("neither: %s", out)
			}
			if got := s.read(m.saved()); got != before {
				t.Errorf("neither: %q, want the saved %q", got, before)
			}
			if code, out := m.configure(strings.Join(s.toml(s.b), "\n")); code != 0 {
				t.Fatalf("the file: %s", out)
			} else if got := s.read(m.saved()); got != s.b {
				t.Errorf("the file: %q, want %q", got, s.b)
			}
			if s.flag == nil {
				return
			}
			if code, out := m.configure("", s.flag(s.a)...); code != 0 {
				t.Fatalf("the flag: %s", out)
			} else if got := s.read(m.saved()); got != s.a {
				t.Errorf("the flag: %q, want %q", got, s.a)
			}
			if code, out := m.configure(strings.Join(s.toml(s.b), "\n"), s.flag(s.a)...); code != 0 {
				t.Fatalf("both: %s", out)
			} else if got := s.read(m.saved()); got != s.a {
				t.Errorf("both: %q, want the flag's %q", got, s.a)
			}
		})
	}
}

// Every configuration field the merge sets has a row in TestConfigureMergesFlagsAndTheConfigFile,
// or is covered by one of the tests named here. The list comes from the code (mergeFieldsSet).
func TestConfigureMergeTableCoversEverySetting(t *testing.T) {
	// No catalog here: this reads code. Selected and Active are the model rows (modelSettings), and
	// Active is also TestConfigureActiveFallsBackToTheFirstSelected.
	covered := map[string]string{
		"DefaultLLMs": "TestConfigureDefaultLLMModels",
		"HFToken":     "TestConfigureHFToken",
		"Selected":    "TestConfigureMergesFlagsAndTheConfigFile (modelSettings)",
		"Active":      "TestConfigureMergesFlagsAndTheConfigFile (modelSettings), TestConfigureActiveFallsBackToTheFirstSelected",
	}
	for _, s := range scalarSettings() {
		covered[s.field] = "TestConfigureMergesFlagsAndTheConfigFile"
	}
	for _, f := range mergeFieldsSet(t) {
		if covered[f] == "" {
			t.Errorf("the merge sets cfg.%s, and no test here covers it", f)
		}
	}
	t.Logf("fields the merge sets: %v", mergeFieldsSet(t))
}

// An active model that isn't among the selected ones falls back to the first selected, when
// neither the flag nor the file names one.
func TestConfigureActiveFallsBackToTheFirstSelected(t *testing.T) {
	a, b := twoLLMs(t)
	m := newMergeRoot(t)
	if code, out := m.configure("", "--llm-models", a+","+b, "--active-llm", b); code != 0 {
		t.Fatal(out)
	}
	if code, out := m.configure("", "--llm-models", a); code != 0 {
		t.Fatal(out)
	}
	if got := m.saved().Active["llm"]; got != a {
		t.Errorf("active %q, want the first selected %q", got, a)
	}
}

// --default-llm-models: one id becomes the active model; two or more are the defaults, and the
// active model stays.
func TestConfigureDefaultLLMModels(t *testing.T) {
	a, b := twoLLMs(t)
	m := newMergeRoot(t)
	if code, out := m.configure("", "--llm-models", a+","+b, "--active-llm", a, "--default-llm-models", b); code == 0 {
		t.Errorf("a single default unlike --active-llm was accepted: %s", out)
	}
	if code, out := m.configure("", "--llm-models", a+","+b, "--default-llm-models", b); code != 0 {
		t.Fatal(out)
	}
	if c := m.saved(); c.Active["llm"] != b || len(c.DefaultLLMs) != 0 {
		t.Errorf("one default: active %q defaults %v, want active %q and no list", c.Active["llm"], c.DefaultLLMs, b)
	}
	if code, out := m.configure("", "--llm-models", a+","+b, "--active-llm", a, "--default-llm-models", b+","+a); code != 0 {
		t.Fatal(out)
	}
	if c := m.saved(); c.Active["llm"] != a || !slices.Equal(c.DefaultLLMs, []string{b, a}) {
		t.Errorf("two defaults: active %q defaults %v, want active %q and [%s %s]", c.Active["llm"], c.DefaultLLMs, a, b, a)
	}
}

// --hf-token always replaces ("" clears). A token in the file only fills a missing one, or one
// equal to it; a different saved one stays, with a warning. With neither, a new root takes HF_TOKEN
// from the environment, and an existing one doesn't.
func TestConfigureHFToken(t *testing.T) {
	m := newMergeRoot(t)
	if got := m.saved().HFToken; got != "" {
		t.Fatalf("a new root with no HF_TOKEN has %q", got)
	}
	if code, out := m.configure(`hfToken = "hf_file1"`); code != 0 || m.saved().HFToken != "hf_file1" {
		t.Errorf("the file fills a missing token: %d %q %s", code, m.saved().HFToken, out)
	}
	code, out := m.configure(`hfToken = "hf_file2"`)
	if code != 0 || m.saved().HFToken != "hf_file1" || !strings.Contains(out, "the hfToken in the TOML file was not applied") {
		t.Errorf("a different file token: %d %q %s", code, m.saved().HFToken, out)
	}
	if code, out := m.configure(`hfToken = "hf_file1"`); code != 0 || m.saved().HFToken != "hf_file1" || strings.Contains(out, "not applied") {
		t.Errorf("the same file token: %d %q %s", code, m.saved().HFToken, out)
	}
	if code, out := m.configure(`hfToken = "hf_file2"`, "--hf-token", "hf_flag"); code != 0 || m.saved().HFToken != "hf_flag" {
		t.Errorf("the flag replaces: %d %q %s", code, m.saved().HFToken, out)
	}
	if code, out := m.configure("", "--hf-token", ""); code != 0 || m.saved().HFToken != "" {
		t.Errorf("--hf-token \"\" clears: %d %q %s", code, m.saved().HFToken, out)
	}
	t.Setenv("HF_TOKEN", "hf_env")
	if code, out := m.configure(""); code != 0 || m.saved().HFToken != "" {
		t.Errorf("an existing root took HF_TOKEN: %d %q %s", code, m.saved().HFToken, out)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	if code, out := runApp(t, "--root", fresh, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatal(out)
	}
	if got := (&mergeRoot{t, fresh}).saved().HFToken; got != "hf_env" {
		t.Errorf("a new root: %q, want HF_TOKEN's", got)
	}
}

// What configure refuses, and that a refusal saves nothing.
func TestConfigureRefusesBadInput(t *testing.T) {
	a, b := twoLLMs(t)
	for name, tc := range map[string]struct {
		toml  string
		flags []string
		want  string
	}{
		"no llm models":               {"", []string{"--llm-models", ""}, "llmModels: Too small"},
		"an unknown model":            {"", []string{"--llm-models", "no-such-model"}, "Invalid llm model ids: no-such-model"},
		"an empty active":             {"", []string{"--active-llm", ""}, "activeLlm: Too small"},
		"a default not selected":      {"", []string{"--llm-models", a, "--default-llm-models", b}, "must also be present in selectedLlmModels"},
		"duplicate defaults":          {"", []string{"--llm-models", a + "," + b, "--default-llm-models", a + "," + a}, "must not contain duplicates"},
		"a bad ctx-mode flag":         {"", []string{"--ctx-mode", "sometimes"}, `ctxMode: Invalid option`},
		"a bad ctx-mode in the file":  {`ctxMode = "sometimes"`, nil, `ctxMode: Invalid option`},
		"a bad otel endpoint":         {"", []string{"--otel-endpoint", "ftp://x"}, "otelEndpoint:"},
		"bad otel headers":            {"", []string{"--otel-headers", "no-equals"}, "otelHeaders:"},
		"a bad parallel flag":         {"", []string{"--parallel", "9"}, "Invalid parallel value: 9"},
		"a port out of range":         {"", []string{"--port", "70000"}, "port"},
		"a port out of range in file": {"port = 70000", nil, "port: must be an integer from 1 to 65535"},
		"a file that isn't TOML":      {"port = ", nil, "Invalid TOML configuration: "},
		"an unknown key in the file":  {"colour = \"red\"", nil, "colour: Unrecognized key"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMergeRoot(t)
			before := fmt.Sprintf("%+v", *m.saved())
			code, out := m.configure(tc.toml, tc.flags...)
			if code != 2 || !strings.Contains(out, tc.want) {
				t.Errorf("exit %d, want 2 and %q:\n%s", code, tc.want, out)
			}
			if after := fmt.Sprintf("%+v", *m.saved()); after != before {
				t.Errorf("a refused configure changed the configuration:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}
