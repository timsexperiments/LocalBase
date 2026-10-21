package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

type feedGolden struct {
	Feeds []struct {
		Name, Text string
		OK         *struct {
			SchemaVersion int    `json:"schemaVersion"`
			SHA256        string `json:"sha256"`
			Models        int    `json:"models"`
			Annotations   *int   `json:"annotations"`
		} `json:"ok"`
		Error string `json:"error"`
	} `json:"feeds"`
	Annotations struct {
		Items  json.RawMessage `json:"items"`
		Result struct {
			Annotations json.RawMessage `json:"annotations"`
			Skipped     int             `json:"skipped"`
		} `json:"result"`
	} `json:"annotations"`
	Published struct {
		Annotations, Skipped int
	} `json:"published"`
	Application []struct {
		ModelID     string          `json:"modelId"`
		Summary     json.RawMessage `json:"summary"`
		Peaks       json.RawMessage `json:"peaks"`
		Unsupported json.RawMessage `json:"unsupported"`
	} `json:"application"`
	URLProblems []struct {
		URL     string  `json:"url"`
		Problem *string `json:"problem"`
	} `json:"urlProblems"`
}

func loadFeedGolden(t *testing.T) *feedGolden {
	t.Helper()
	b, err := os.ReadFile("testdata/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	var g feedGolden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// sameJSONValue compares two JSON documents ignoring key order.
func sameJSONValue(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%v: %s", err, a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return reflect.DeepEqual(x, y)
}

func TestParseFeedGolden(t *testing.T) {
	for _, c := range loadFeedGolden(t).Feeds {
		f, err := ParseFeed([]byte(c.Text))
		switch {
		case c.OK == nil:
			if err == nil || err.Error() != c.Error {
				t.Errorf("%s: got %v, want error %q", c.Name, err, c.Error)
			}
		case err != nil:
			t.Errorf("%s: %v", c.Name, err)
		default:
			ann := len(f.AnnotationItems)
			if f.SchemaVersion != c.OK.SchemaVersion || f.SHA256 != c.OK.SHA256 || len(f.Models) != c.OK.Models ||
				(c.OK.Annotations == nil) != (f.AnnotationItems == nil) || (c.OK.Annotations != nil && *c.OK.Annotations != ann) {
				t.Errorf("%s: %d %s %d models %d annotations; want %+v", c.Name, f.SchemaVersion, f.SHA256, len(f.Models), ann, *c.OK)
			}
		}
	}
}

func TestParseAnnotationsGolden(t *testing.T) {
	g := loadFeedGolden(t)
	v, err := jsjson.Parse(g.Annotations.Items)
	if err != nil {
		t.Fatal(err)
	}
	got, skipped := ParseAnnotations(v.([]any))
	b, _ := json.Marshal(got)
	if skipped != g.Annotations.Result.Skipped || !sameJSONValue(t, b, g.Annotations.Result.Annotations) {
		t.Errorf("skipped %d (want %d)\n got  %s\n want %s", skipped, g.Annotations.Result.Skipped, b, g.Annotations.Result.Annotations)
	}
}

func TestFeedURLProblemGolden(t *testing.T) {
	for _, c := range loadFeedGolden(t).URLProblems {
		want := ""
		if c.Problem != nil {
			want = *c.Problem
		}
		if got := FeedURLProblem(c.URL); got != want {
			t.Errorf("%q: %q, want %q", c.URL, got, want)
		}
	}
}

// TestAnnotationApplicationGolden applies the published feed's annotations
// to the built-in catalog and compares every model with TS.
func TestAnnotationApplicationGolden(t *testing.T) {
	g := loadFeedGolden(t)
	b, err := os.ReadFile("../../../catalog/feed-v2.json")
	if err != nil {
		t.Skip("published feed not found")
	}
	f, err := ParseFeed(b)
	if err != nil {
		t.Fatal(err)
	}
	ann, skipped := ParseAnnotations(f.AnnotationItems)
	if len(ann) != g.Published.Annotations || skipped != g.Published.Skipped {
		t.Fatalf("published annotations %d/%d, want %d/%d", len(ann), skipped, g.Published.Annotations, g.Published.Skipped)
	}
	cat, err := LoadLayers("", &RemoteLayer{URL: "feed", Annotations: ann})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range g.Application {
		s := cat.ByID(c.ModelID)
		if s == nil {
			t.Errorf("%s missing", c.ModelID)
			continue
		}
		summary, _ := json.Marshal(cat.AnnotationSummaryFor(s))
		peaks, _ := json.Marshal(nonNil(cat.PlannableFeedPeaks(s)))
		unsupported, _ := json.Marshal(nonNil(cat.EffectiveUnsupported(s)))
		if !sameJSONValue(t, summary, c.Summary) {
			t.Errorf("%s summary\n got  %s\n want %s", c.ModelID, summary, c.Summary)
		}
		if !sameJSONValue(t, peaks, c.Peaks) {
			t.Errorf("%s peaks\n got  %s\n want %s", c.ModelID, peaks, c.Peaks)
		}
		if !sameJSONValue(t, unsupported, c.Unsupported) {
			t.Errorf("%s unsupported\n got  %s\n want %s", c.ModelID, unsupported, c.Unsupported)
		}
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
