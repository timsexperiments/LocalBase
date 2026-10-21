package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// fixtureHub serves the recorded Hub responses (testdata/hub, keyed by the
// public URL, as tools/golden/hub.ts records them).
func fixtureHub(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	misses := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := PublicEndpoint + r.URL.RequestURI()
		sum := sha256.Sum256([]byte(public))
		b, err := os.ReadFile(filepath.Join("testdata", "hub", hex.EncodeToString(sum[:])[:24]+".json"))
		if err != nil {
			misses++
			t.Logf("no fixture for %s", public)
			http.NotFound(w, r)
			return
		}
		var f struct {
			URL    string
			Status int
			Body   string
		}
		json.Unmarshal(b, &f)
		w.WriteHeader(f.Status)
		w.Write([]byte(f.Body))
	}))
	t.Cleanup(srv.Close)
	return srv, &misses
}

type outcome struct {
	OK    json.RawMessage `json:"ok"`
	Error *string         `json:"error"`
}

type golden struct {
	Models []struct {
		Repo   string          `json:"repo"`
		Info   json.RawMessage `json:"info"`
		Likely json.RawMessage `json:"likely"`
		Choice *outcome        `json:"choices"`
		VPred  *string         `json:"vPrediction"`
		Entry  []struct {
			Selector *string `json:"selector"`
			Result   outcome `json:"result"`
		} `json:"entries"`
	} `json:"models"`
	ExplicitID outcome `json:"explicitID"`
	BadID      outcome `json:"badID"`
	Searches   []struct {
		Kind   string  `json:"kind"`
		Query  string  `json:"query"`
		Limit  int     `json:"limit"`
		Result outcome `json:"result"`
	} `json:"searches"`
	Previews []struct {
		Repo    string          `json:"repo"`
		Preview outcome         `json:"preview"`
		Notes   json.RawMessage `json:"notes"`
	} `json:"previews"`
	References []struct {
		Value  string  `json:"value"`
		Result outcome `json:"result"`
	} `json:"references"`
	Quants []struct {
		Path  string  `json:"path"`
		Quant *string `json:"quant"`
		LLM   string  `json:"llm"`
		STT   string  `json:"stt"`
		Image string  `json:"image"`
	} `json:"quants"`
	Licenses []struct {
		License *string `json:"license"`
		Status  string  `json:"status"`
	} `json:"licenses"`
}

func loadGolden(t *testing.T) *golden {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// same compares a value with a golden JSON document, ignoring key order.
func same(t *testing.T, got any, want json.RawMessage) (bool, string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var x, y any
	json.Unmarshal(b, &x)
	json.Unmarshal(want, &y)
	return reflect.DeepEqual(x, y), string(b)
}

func check(t *testing.T, label string, got any, err error, want outcome) {
	t.Helper()
	switch {
	case want.Error != nil:
		if err == nil || err.Error() != *want.Error {
			t.Errorf("%s: got %v, want error %q", label, err, *want.Error)
		}
	case err != nil:
		t.Errorf("%s: %v", label, err)
	default:
		if ok, b := same(t, got, want.OK); !ok {
			t.Errorf("%s:\n got  %s\n want %s", label, b, want.OK)
		}
	}
}

// entryJSON is GenerateEntry's result in the TS GeneratedEntry shape.
func entryJSON(e *Entry) map[string]any {
	var ctx any
	if e.ContextLength != nil {
		ctx = *e.ContextLength
	}
	var projector any
	if e.Projector != nil {
		projector = e.Projector
	}
	return map[string]any{"spec": json.RawMessage(jsjson.Stringify(e.Spec)), "modelId": e.ModelID, "kind": e.Kind, "repo": e.Repo,
		"revision": e.Revision, "license": e.License, "pipelineTag": e.PipelineTag, "weights": e.Weights, "projector": projector,
		"detected": e.Detected, "contextLength": ctx}
}

func TestHubGolden(t *testing.T) {
	g := loadGolden(t)
	srv, misses := fixtureHub(t)
	c := &Client{Root: t.TempDir(), Endpoint: srv.URL}
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	infos := map[string]*ModelInfo{}
	for _, m := range g.Models {
		info, err := c.ModelInfo(ctx, m.Repo)
		var infoJSON any
		if err == nil {
			infos[m.Repo] = info
			var pred any
			if info.SchedulerPrediction != "" {
				pred = info.SchedulerPrediction
			}
			infoJSON = map[string]any{"id": info.ID, "sha": info.SHA, "schedulerPrediction": pred}
		} else {
			infoJSON = map[string]any{"error": err.Error()}
		}
		if ok, b := same(t, infoJSON, m.Info); !ok {
			t.Errorf("%s info:\n got  %s\n want %s", m.Repo, b, m.Info)
			continue
		}
		if info == nil {
			continue
		}
		likely := LikelyKind(info.PipelineTag, info.Tags, info.ID, info.Library)
		if ok, b := same(t, likely, m.Likely); !ok {
			t.Errorf("%s likely:\n got  %s\n want %s", m.Repo, b, m.Likely)
		}
		choices, err := EntryChoices(info)
		var cj any
		if err == nil {
			var paths []string
			for _, f := range choices.Files {
				paths = append(paths, f.Path)
			}
			var def, pipe, lora any
			if choices.Default != nil {
				def = choices.Default.Path
			}
			if choices.Pipeline != "" {
				pipe = choices.Pipeline
			}
			if choices.Lora != "" {
				lora = choices.Lora
			}
			cj = map[string]any{"kind": choices.Kind, "choices": paths, "default": def, "pipeline": pipe, "lora": lora}
		}
		check(t, m.Repo+" choices", cj, err, *m.Choice)
		var vp *string
		if v := VPrediction(info, ""); v != "" {
			vp = &v
		}
		if (vp == nil) != (m.VPred == nil) || (vp != nil && *vp != *m.VPred) {
			t.Errorf("%s vPrediction: %v want %v", m.Repo, vp, m.VPred)
		}
		for _, e := range m.Entry {
			ref := Reference{Repo: m.Repo}
			if e.Selector != nil {
				ref.Selector = *e.Selector
			}
			entry, err := GenerateEntry(cat, info, ref, "")
			var got any
			if err == nil {
				got = entryJSON(entry)
			}
			check(t, m.Repo+":"+ref.Selector, got, err, e.Result)
		}
	}
	whisper := infos["ggerganov/whisper.cpp"]
	e1, err := GenerateEntry(cat, whisper, Reference{Repo: "ggerganov/whisper.cpp", Selector: "ggml-base.en-q8_0.bin"}, "my-whisper")
	var got any
	if err == nil {
		got = json.RawMessage(jsjson.Stringify(e1.Spec))
	}
	check(t, "explicit id", got, err, g.ExplicitID)
	_, err = GenerateEntry(cat, whisper, Reference{Repo: "ggerganov/whisper.cpp"}, "Bad ID")
	check(t, "bad id", nil, err, g.BadID)

	for _, s := range g.Searches {
		hits, cached, err := c.Search(ctx, s.Kind, s.Query, s.Limit)
		var got any
		if err == nil {
			got = map[string]any{"hits": hits, "cached": cached}
		}
		check(t, "search "+s.Kind+" "+s.Query, got, err, s.Result)
	}
	for _, r := range g.References {
		ref, err := ParseReference(r.Value)
		check(t, "reference "+r.Value, ref, err, r.Result)
	}
	for _, q := range g.Quants {
		want := ""
		if q.Quant != nil {
			want = *q.Quant
		}
		if QuantOf(q.Path) != want || ModelIDFor(q.Path, "llm") != q.LLM || ModelIDFor(q.Path, "stt") != q.STT || ModelIDFor(q.Path, "image") != q.Image {
			t.Errorf("%s: quant %q ids %q %q %q; want %+v", q.Path, QuantOf(q.Path), ModelIDFor(q.Path, "llm"), ModelIDFor(q.Path, "stt"), ModelIDFor(q.Path, "image"), q)
		}
	}
	for _, l := range g.Licenses {
		in := ""
		if l.License != nil {
			in = *l.License
		}
		if got := CommercialStatus(in); got != l.Status {
			t.Errorf("license %q: %s, want %s", in, got, l.Status)
		}
	}
	entries := 0
	for _, m := range g.Models {
		entries += len(m.Entry)
	}
	t.Logf("compared %d repositories, %d entries, %d searches, %d references", len(g.Models), entries, len(g.Searches), len(g.References))
	if *misses > 0 {
		t.Errorf("%d requests had no fixture (query encoding differs from TS?)", *misses)
	}
}

// TestDiscoveryGolden previews every repository with the published feed's
// annotations in force (fit on a 64 GB machine) and compares the feed notes.
func TestDiscoveryGolden(t *testing.T) {
	g := loadGolden(t)
	srv, misses := fixtureHub(t)
	c := &Client{Root: t.TempDir(), Endpoint: srv.URL}
	b, err := os.ReadFile("../../../catalog/feed-v2.json")
	if err != nil {
		t.Skip("published feed not found")
	}
	f, err := catalog.ParseFeed(b)
	if err != nil {
		t.Fatal(err)
	}
	ann, _ := catalog.ParseAnnotations(f.AnnotationItems)
	cat, err := catalog.LoadLayers("", &catalog.RemoteLayer{URL: "feed", Annotations: ann})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range g.Previews {
		preview, err := PreviewRepository(context.Background(), c, cat, p.Repo, 64)
		var got any
		if err == nil {
			got = preview
		}
		check(t, "preview "+p.Repo, got, err, p.Preview)
		info, _ := c.ModelInfo(context.Background(), p.Repo)
		sha := ""
		if info != nil {
			sha = info.SHA
		}
		if ok, b := same(t, CatalogNotes(cat, p.Repo, sha), p.Notes); !ok {
			t.Errorf("notes %s:\n got  %s\n want %s", p.Repo, b, p.Notes)
		}
	}
	if *misses > 0 {
		t.Errorf("%d requests had no fixture", *misses)
	}
}

// TestDiscoverGolden: search with previews (the gateway's discovery) equals
// TS discoverModels for the same query, 64 GB, the feed off.
func TestDiscoverGolden(t *testing.T) {
	b, _ := os.ReadFile("testdata/golden.json")
	var g struct {
		Discovery outcome `json:"discovery"`
	}
	json.Unmarshal(b, &g)
	srv, misses := fixtureHub(t)
	c := &Client{Root: t.TempDir(), Endpoint: srv.URL}
	cat, _ := catalog.Load("")
	hits, cached, err := Discover(context.Background(), c, cat, "llm", "qwen coder", 64, 20, 12)
	var got any
	if err == nil {
		got = map[string]any{"hits": hits, "cached": cached}
	}
	check(t, "discover", got, err, g.Discovery)
	if *misses > 0 {
		t.Errorf("%d requests had no fixture", *misses)
	}
}

// TestFixturesPinned: the recorded Hub responses the goldens replay are
// exactly the pinned files (testdata/hub/PINS), none added or changed.
func TestFixturesPinned(t *testing.T) {
	pins, err := os.ReadFile("testdata/hub/PINS")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(pins)), "\n") {
		name, sum, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("PINS line %q", line)
		}
		want[name] = sum
	}
	files, _ := filepath.Glob("testdata/hub/*.json")
	if len(files) != len(want) {
		t.Errorf("%d fixtures, %d pinned", len(files), len(want))
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); want[filepath.Base(f)] != got {
			t.Errorf("%s: sha256 %s, pinned %q", filepath.Base(f), got, want[filepath.Base(f)])
		}
	}
}
