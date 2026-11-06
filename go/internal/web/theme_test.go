package web

import (
	"bytes"
	"compress/gzip"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"io"
	"io/fs"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A visual direction is one file: no color outside it.
func TestThemeIsOneFile(t *testing.T) {
	hex := regexp.MustCompile(`#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{3}\b`)
	fn := regexp.MustCompile(`\brgba?\(|\bhsla?\(|\boklch\(`)
	check := func(path string, pattern ...*regexp.Regexp) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pattern {
			if m := p.Find(b); m != nil {
				t.Errorf("%s: a color outside the theme: %s", path, m)
			}
		}
	}
	check("css/input.css", hex, fn)
	files, _ := filepath.Glob("templates/*.html")
	six := regexp.MustCompile(`#[0-9a-fA-F]{6}\b`)
	for _, f := range files {
		check(f, six, fn)
	}
	js, _ := filepath.Glob("static/js/*.js")
	for _, f := range js {
		check(f, six)
	}
}

func TestThemeColorComesFromTheTheme(t *testing.T) {
	if themeColor("light") == "" || themeColor("dark") == "" || themeColor("light") == themeColor("dark") {
		t.Errorf("light %q, dark %q", themeColor("light"), themeColor("dark"))
	}
}

// The gauge: every place filled once, in order, by shape; a sliver still shows.
func TestGaugeSegments(t *testing.T) {
	count := func(s []gaugeSegment, kind string) (n int) {
		for _, x := range s {
			if x.Kind == kind {
				n++
			}
		}
		return n
	}
	const gb = int64(1) << 30
	s := gaugeSegments(37*gb, 3*gb, 64*gb)
	if len(s) != gaugeSegmentCount || count(s, "other") != 28 || count(s, "models") != 2 || count(s, "free") != 18 {
		t.Fatalf("%d other, %d models, %d free", count(s, "other"), count(s, "models"), count(s, "free"))
	}
	if s[0].Kind != "other" || s[47].Kind != "free" || s[28].Kind != "models" {
		t.Errorf("out of order: %+v", s)
	}
	if s[0].H >= s[28].H {
		t.Error("other apps and LocalBase are the same shape")
	}
	if got := gaugeSegments(0, gb/5, 64*gb); count(got, "models") != 0 || count(got, "sliver") != 1 {
		t.Error("a tiny share should be one thin mark, not a block")
	}
	if got := gaugeSegments(0, 1<<20, 64*gb); count(got, "models") != 0 || count(got, "sliver") != 1 || gibText(1<<20) != "<0.1 GiB" {
		t.Error("a share under 0.1 GiB should be a mark beside <0.1 GiB")
	}
	if got := gibText(int64(1<<30) / 20); got != "<0.1 GiB" {
		t.Errorf("the figure that rounds to nothing printed %q", got)
	}
	if got := gaugeSegments(0, 3*gb, 64*gb); count(got, "models") != 2 || count(got, "sliver") != 0 {
		t.Error("a real share should be blocks")
	}
	for _, none := range [][3]int64{{-1, 0, 64 * gb}, {0, 0, 0}, {-1, 3 * gb, 0}} {
		if got := gaugeSegments(none[0], none[1], none[2]); count(got, "unknown") != gaugeSegmentCount {
			t.Errorf("no reading %v drew a picture: %+v", none, got[0])
		}
	}
	for _, c := range []struct {
		st       runtime.Snapshot
		pressure string
		busy     bool
		word     string
	}{
		{runtime.Snapshot{State: "idle"}, "healthy", false, "Ready"},
		{runtime.Snapshot{State: "running"}, "healthy", false, "Ready"},
		{runtime.Snapshot{State: "idle"}, "healthy", true, "Busy"},
		{runtime.Snapshot{State: "starting"}, "healthy", true, "Getting the model ready"},
		{runtime.Snapshot{State: "running"}, "critical", true, "Not enough memory"},
		{runtime.Snapshot{State: "running"}, "constrained", false, "Memory is tight"},
		{runtime.Snapshot{State: "running"}, "constrained", true, "Busy"},
	} {
		if word, _ := stripWord(c.st, c.pressure, false, c.busy); word != c.word {
			t.Errorf("%+v, %s, %v: %q, want %q", c.st, c.pressure, c.busy, word, c.word)
		}
	}
	// Nearly full, but not full: a place stays free.
	if got := gaugeSegments(63*gb, 1<<29, 64*gb); count(got, "free") < 1 {
		t.Error("the gauge says full when it is not")
	}
	if got := gaugeSegments(64*gb, 0, 64*gb); count(got, "free") != 0 {
		t.Error("the gauge says free when it is full")
	}
	if got := gaugeSegments(0, 0, 0); count(got, "unknown") != gaugeSegmentCount {
		t.Error("no capacity")
	}
}

func TestComputerName(t *testing.T) {
	t.Setenv("LOCALBASE_COMPUTER_NAME", "Mac Studio")
	if computerName() != "Mac Studio" {
		t.Error(computerName())
	}
}

// Budgets, in gzipped bytes (DESIGN §25). They hold the gzip wins, and leave a little room.
const (
	budgetPageAssets = 62 << 10 // CSS and every script a page loads, as sent
	budgetPageHTML   = 8 << 10
	budgetFonts      = 82 << 10 // every font file
)

func gzipSize(b []byte) int {
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return out.Len()
}

// pageAssets follows a page's stylesheet and scripts, and what those scripts import.
func pageAssets(html string) map[string]bool {
	seen := map[string]bool{}
	var follow func(name string)
	imports := regexp.MustCompile(`(?m)^\s*(?:import|export)[^;"']*from\s+["'](\./[^"']+)["']|^\s*import\s+["'](\./[^"']+)["']`)
	follow = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil || !strings.HasSuffix(name, ".js") {
			return
		}
		for _, m := range imports.FindAllSubmatch(b, -1) {
			rel := string(m[1])
			if rel == "" {
				rel = string(m[2])
			}
			follow(filepath.Join(filepath.Dir(name), rel))
		}
	}
	for _, m := range regexp.MustCompile(`/app/assets/([^"?]+\.(?:css|js))`).FindAllStringSubmatch(html, -1) {
		follow(m[1])
	}
	return seen
}

func TestSizeBudgets(t *testing.T) {
	cat, _ := catalog.Load("")
	cfg := &store.Config{Selected: map[string][]string{"llm": {"qwen3.5-4b-q4_k_m", "clef-flash-q4_k_m"}, "image": {"dreamshaper-8-lcm"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	u := New(Deps{NoAuth: true, Catalog: cat, LLM: llm})
	for _, path := range []string{"/app/studio", "/app/studio/image", "/app/studio/video", "/app/studio/speech", "/app/studio/transcribe", "/app/studio/decisions", "/app/chat"} {
		w := httptest.NewRecorder()
		u.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		html := w.Body.String()
		if n := gzipSize(w.Body.Bytes()); n > budgetPageHTML {
			t.Errorf("%s: HTML is %d bytes gzipped, budget %d", path, n, budgetPageHTML)
		}
		total := 0
		for name := range pageAssets(html) {
			b, err := staticFS.ReadFile("static/" + name)
			if err != nil {
				t.Errorf("%s: %s: %v", path, name, err)
				continue
			}
			total += gzipSize(b)
		}
		t.Logf("%s: assets %d, HTML %d", path, total, gzipSize(w.Body.Bytes()))
		if !strings.HasPrefix(path, "/app/chat") && total > budgetPageAssets {
			t.Errorf("%s: assets are %d bytes gzipped, budget %d", path, total, budgetPageAssets)
		}
		if strings.HasPrefix(path, "/app/chat") && total > budgetChatAssets {
			t.Errorf("%s: assets are %d bytes gzipped, budget %d", path, total, budgetChatAssets)
		}
	}
	fonts := 0
	fs.WalkDir(staticFS, "static/fonts", func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".woff2") {
			b, _ := staticFS.ReadFile(p)
			fonts += len(b)
		}
		return nil
	})
	t.Logf("fonts %d", fonts)
	if fonts > budgetFonts {
		t.Errorf("fonts are %d bytes, budget %d", fonts, budgetFonts)
	}
}

const budgetChatAssets = 86 << 10
