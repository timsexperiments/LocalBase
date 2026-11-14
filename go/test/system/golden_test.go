package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// The pinned CLI tests. Each of these compared the Go command line with TS's and tested nothing
// else, so step 7's R3a pinned Go's own results while TS still agreed with them, and R3b removed
// TS. A pinned test's Go CLI runs (harness.Pins says where):
//
//   - as the localbase_lab build on harness.FixedHostJSON's 64 GiB host, so what depends on the
//     host's memory (fit verdicts, systemMemoryGb, what --fits lists) is the same on every
//     runner. The lab build differs from the release only in that host seam, which
//     TestLabSeamFilesAreTagged holds to localbase_lab files;
//   - are recorded, normalized and in order, as the test's transcript, and compared with
//     testdata/golden/<platform>/<Test>.json.

// transcriptCall is one Go CLI run of a pinned test, normalized (scrub).
type transcriptCall struct {
	Args   []string `json:"args"`
	Code   int      `json:"code"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
	Masks  []string `json:"masks,omitempty"`
}

type pin struct {
	mu   sync.Mutex
	host string // the fixed host's file
	want []transcriptCall
	got  []transcriptCall
}

var pins sync.Map // top-level test name -> *pin

func pinOf(t testing.TB) *pin {
	name, _, _ := strings.Cut(t.Name(), "/")
	if p, ok := pins.Load(name); ok {
		return p.(*pin)
	}
	return nil
}

// pinnedFixture starts a test that pins values (harness.PinValues) and launches gateways: it
// returns the launch options that put them on the golden fixture.
func pinnedFixture(t *testing.T) []harness.Option {
	t.Helper()
	harness.PinValues(t)
	return []harness.Option{harness.GoldenFixture()}
}

// pinned starts a pinned CLI test: see the comment at the top of this file.
func pinned(t *testing.T) {
	t.Helper()
	harness.Pins(t)
	update := os.Getenv(fsguard.UpdateGoldenEnv) == "1"
	host := filepath.Join(fsguard.ScratchDir(t), "fixed-host.json")
	if err := fsguard.WriteFile(host, []byte(harness.FixedHostJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &pin{host: host}
	name := filepath.Join(harness.GoldenDir(), t.Name()+".json")
	golden := filepath.Join("testdata", "golden", name)
	if !update {
		b, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("no golden for %s (%v): record it with %s=1", t.Name(), err, fsguard.UpdateGoldenEnv)
		}
		var g struct {
			Calls []transcriptCall `json:"calls"`
		}
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatalf("%s: %v", golden, err)
		}
		p.want = g.Calls
	}
	pins.Store(t.Name(), p)
	t.Cleanup(func() {
		pins.Delete(t.Name())
		if update {
			if t.Failed() {
				t.Logf("%s not recorded: the test failed", golden)
				return
			}
			b, _ := json.MarshalIndent(struct {
				Calls []transcriptCall `json:"calls"`
			}{p.got}, "", "  ")
			if err := fsguard.WriteGolden(name, append(b, '\n')); err != nil {
				t.Error(err)
			}
			return
		}
		if len(p.got) < len(p.want) {
			t.Errorf("Go ran %d of the %d commands in %s; the first not run: %v", len(p.got), len(p.want), golden, p.want[len(p.got)].Args)
		}
	})
}

// record adds one Go CLI run to the test's transcript and compares it with the golden's.
func (p *pin) record(t *testing.T, root string, args []string, res cliResult) {
	t.Helper()
	var masks []string
	call := transcriptCall{Code: res.code, Args: make([]string, len(args))}
	for i, a := range args {
		call.Args[i] = scrub(a, root, args, nil)
	}
	call.Stdout = scrub(res.stdout, root, args, &masks)
	call.Stderr = scrub(res.stderr, root, args, &masks)
	sort.Strings(masks)
	call.Masks = slices.Compact(masks)
	p.mu.Lock()
	defer p.mu.Unlock()
	i := len(p.got)
	p.got = append(p.got, call)
	if p.want == nil {
		return // recording
	}
	if i >= len(p.want) {
		t.Errorf("Go ran a command the golden doesn't have (#%d): %v", i+1, call.Args)
		return
	}
	if d := diffCall(p.want[i], call); d != "" {
		t.Errorf("Go's command #%d (%v) differs from the golden:\n%s\nIf the change is intended, record again with %s=1.", i+1, call.Args, d, fsguard.UpdateGoldenEnv)
	}
}

// hostMask is a named part of an output that depends on the machine and not on LocalBase. Each
// call records the masks it needed, and a replay must need the same ones: a masked field that
// disappears fails the comparison instead of passing unnoticed.
type hostMask struct {
	name    string
	applies func(args []string) bool
	re      *regexp.Regexp
	repl    func(string) string
}

var (
	hardwareValue = regexp.MustCompile(`("(?:osName|ramGb|cpuModel|gpuName|gpuVramGb|isMac|isAppleSilicon)":)("[^"]*"|[^,}]*)`)
	hostMasks     = []hostMask{
		// doctor's hardware block: the machine's OS, CPU, RAM and GPU (sysinfo, not the lab host).
		{name: "doctor hardware", applies: isDoctor, re: regexp.MustCompile(`"hardware":\{[^{}]*\}`),
			repl: func(m string) string { return hardwareValue.ReplaceAllString(m, `$1"<host>"`) }},
		// doctor's human lines about the machine and the build: the version line carries the
		// commit and the Go toolchain, and Status follows the machine's RAM.
		{name: "doctor lines", applies: isDoctor, re: regexp.MustCompile(`(?m)^(LocalBase|OS|CPU|RAM|GPU|GPU VRAM|Status): .+$`),
			repl: func(m string) string { label, _, _ := strings.Cut(m, ": "); return label + ": <host>" }},
		// diagnostics' memory lines end in a fit verdict, which follows the machine's GPU memory:
		// internal/diagnostics reads memory.DetectSpecs, not the lab host. The size and its
		// source stay pinned.
		{name: "diagnostics fit", applies: isDiagnostics, re: regexp.MustCompile(`(?m)^Memory for [^:\n]+: [0-9.]+ GB \([^)\n]*\), \S+$`),
			repl: func(m string) string { return m[:strings.LastIndex(m, ", ")] + ", <host>" }},
	}
	loopbackPort = regexp.MustCompile(`(127\.0\.0\.1|localhost):\d+`)
	// A key's prefix and a voice's id are random; a diagnostics archive's size follows the
	// compression of what it holds, times included.
	keyPrefixJSON = regexp.MustCompile(`"prefix":"[A-Za-z0-9_-]{8}"`)
	keyPrefixText = regexp.MustCompile(`\bprefix=[A-Za-z0-9_-]{8}`)
	voiceID       = regexp.MustCompile(`\bvoice-[0-9a-f]{12}\b`)
	archiveBytes  = regexp.MustCompile(`("archive":\{[^{}]*"bytes":)\d+`)
)

func isDoctor(args []string) bool { return slices.Contains(args, "doctor") }

func isDiagnostics(args []string) bool { return slices.Contains(args, "diagnostics") }

// scrub normalizes one string of a Go CLI run for the transcript: the run's root, the system temp
// directory's entries (each test's scratch), loopback ports, ids, secrets and times, and the
// host masks (named in masks when masks isn't nil).
func scrub(s, root string, args []string, masks *[]string) string {
	if root != "" {
		s = strings.ReplaceAll(s, root, "<root>")
	}
	for _, tmp := range tempDirs() {
		s = regexp.MustCompile(regexp.QuoteMeta(tmp)+`/[^/\s"']+`).ReplaceAllString(s, "<tmp>")
	}
	s = loopbackPort.ReplaceAllString(s, "$1:<port>")
	s = keyPrefixJSON.ReplaceAllString(s, `"prefix":"<prefix>"`)
	s = keyPrefixText.ReplaceAllString(s, "prefix=<prefix>")
	s = voiceID.ReplaceAllString(s, "voice-<id>")
	s = archiveBytes.ReplaceAllString(s, `$1"<bytes>"`)
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
	if masks != nil {
		for _, m := range hostMasks {
			if m.applies(args) && m.re.MatchString(s) {
				s = m.re.ReplaceAllStringFunc(s, m.repl)
				*masks = append(*masks, m.name)
			}
		}
	}
	return s
}

// tempDirs is the system temp directory as the CLI may print it: as given and with its links
// resolved (/var and /private/var on macOS), the longer first.
func tempDirs() []string {
	tmp := filepath.Clean(os.TempDir())
	dirs := []string{tmp}
	if real, err := filepath.EvalSymlinks(tmp); err == nil && real != tmp {
		dirs = append(dirs, real)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	return dirs
}

// diffCall says how a run differs from the golden's, or "".
func diffCall(want, got transcriptCall) string {
	var out []string
	if !slices.Equal(want.Args, got.Args) {
		out = append(out, fmt.Sprintf("args: golden %v", want.Args))
	}
	if want.Code != got.Code {
		out = append(out, fmt.Sprintf("exit code: golden %d, now %d", want.Code, got.Code))
	}
	if !slices.Equal(want.Masks, got.Masks) {
		out = append(out, fmt.Sprintf("host masks: golden %v, now %v", want.Masks, got.Masks))
	}
	for _, f := range []struct{ name, want, got string }{{"stdout", want.Stdout, got.Stdout}, {"stderr", want.Stderr, got.Stderr}} {
		if f.want == f.got {
			continue
		}
		var a, b any
		if json.Unmarshal([]byte(f.want), &a) == nil && json.Unmarshal([]byte(f.got), &b) == nil {
			if d := harness.DiffJSON("", a, b, nil); len(d) > 0 {
				if len(d) > 12 {
					d = append(d[:12], "...")
				}
				out = append(out, f.name+" (golden vs now):\n    "+strings.Join(d, "\n    "))
				continue
			}
		}
		out = append(out, f.name+": "+firstLineDiff(f.want, f.got))
	}
	return strings.Join(out, "\n")
}

func firstLineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: golden %q, now %q", i+1, x, y)
		}
	}
	return "differs"
}

// valueMask is a named part of a pinned value that depends on the machine, by DiffJSON path.
type valueMask struct {
	name  string
	match func(path string) bool
}

// maskJSON is v with every value a mask matches replaced by "<host>", and the names of the masks
// that matched, sorted.
func maskJSON(v any, path string, masks []valueMask) (any, []string) {
	seen := map[string]bool{}
	var walk func(v any, path string) any
	walk = func(v any, path string) any {
		for _, m := range masks {
			if m.match(path) {
				seen[m.name] = true
				return "<host>"
			}
		}
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, e := range x {
				out[k] = walk(e, path+"."+k)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e, fmt.Sprintf("%s[%d]", path, i))
			}
			return out
		}
		return v
	}
	out := walk(v, path)
	used := make([]string, 0, len(seen))
	for name := range seen {
		used = append(used, name)
	}
	sort.Strings(used)
	return out, used
}
