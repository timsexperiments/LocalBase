package web

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The choice ranking and catalog matching agree with TS model-choice.ts on
// the recorded Hub previews and edge cases (tools/golden/choice.ts).
func TestChoiceGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/choice.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Sets []struct {
			Name    string
			Choices []struct {
				Path      string
				SizeBytes int64
				Quant     *string
				Fit       *struct{ Verdict string }
			}
			Recommended *string
			Ordered     []string
		}
		Quants []struct {
			Quant *string
			Low   bool
		}
		Models []struct {
			ID      string
			Catalog struct {
				Name, DisplayName, Source, QualificationState, Trust string
			}
		}
		Matches []struct {
			Repo  string
			Match *string
		}
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	for _, s := range g.Sets {
		var choices []downloadChoice
		for _, c := range s.Choices {
			d := downloadChoice{Path: c.Path, SizeBytes: c.SizeBytes, Quant: c.Quant}
			if c.Fit != nil {
				d.Verdict = c.Fit.Verdict
			}
			choices = append(choices, d)
		}
		rec := recommendedChoice(choices)
		got := ""
		if rec >= 0 {
			got = choices[rec].Path
		}
		want := ""
		if s.Recommended != nil {
			want = *s.Recommended
		}
		if got != want {
			t.Errorf("%s: recommended %q, want %q", s.Name, got, want)
		}
		var ordered []string
		for _, i := range orderedChoices(choices) {
			ordered = append(ordered, choices[i].Path)
		}
		if !slices.Equal(ordered, s.Ordered) {
			t.Errorf("%s: ordered\n got %v\nwant %v", s.Name, ordered, s.Ordered)
		}
	}
	for _, q := range g.Quants {
		if lowQualityQuant(q.Quant) != q.Low {
			t.Errorf("lowQualityQuant(%v) = %v", q.Quant, !q.Low)
		}
	}
	var models []catalogIdentity
	for _, m := range g.Models {
		c := m.Catalog
		models = append(models, catalogIdentity{ID: m.ID, Name: c.Name, DisplayName: c.DisplayName, Source: c.Source, QualificationState: c.QualificationState, Trust: c.Trust})
	}
	for _, m := range g.Matches {
		i := qualifiedCatalogMatch(m.Repo, models)
		got := ""
		if i >= 0 {
			got = models[i].ID
		}
		want := ""
		if m.Match != nil {
			want = *m.Match
		}
		if got != want {
			t.Errorf("match %s: %q, want %q", m.Repo, got, want)
		}
	}
}
