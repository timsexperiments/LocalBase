package conf

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// TestDeclarativeGolden replays tools/golden/config.ts: every document's
// validation message (before the CLI's redaction) or normalized
// configuration, its plans and its rendering match TS.
func TestDeclarativeGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/declarative.json")
	if err != nil {
		t.Fatal(err)
	}
	type settled struct {
		OK    json.RawMessage `json:"ok"`
		Error *string         `json:"error"`
	}
	var g struct {
		Base  string
		Cases []struct {
			Name, Doc     string
			Error         *string
			Configuration json.RawMessage
			Plan          settled
			PlanPending   settled
			PlanFresh     settled
			Rendered      string
		}
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 50 || g.Cases[1].Plan.OK == nil {
		t.Fatalf("the golden decoded %d cases", len(g.Cases))
	}
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	// The saved configuration the plans compare with: the one TS rendered.
	baseDoc, err := ParseDocument(g.Base, cat)
	if err != nil {
		t.Fatal(err)
	}
	current := Compose(&store.Config{Root: "/r", Selected: map[string][]string{}, Active: map[string]string{}}, baseDoc)
	if got := Render(current); got != g.Base {
		t.Errorf("render:\n%s\nwant\n%s", got, g.Base)
	}
	same := func(label string, got any, want json.RawMessage) {
		t.Helper()
		gb, _ := json.Marshal(got)
		var x, y any
		json.Unmarshal(gb, &x)
		json.Unmarshal(want, &y)
		if !reflect.DeepEqual(x, y) {
			t.Errorf("%s:\n got  %s\n want %s", label, gb, want)
		}
	}
	plan := func(label string, cur *store.Config, desired *store.Config, pending bool, want settled) {
		t.Helper()
		p, err := PlanChanges(cur, desired, pending, cat)
		switch {
		case want.Error != nil:
			if err == nil || err.Error() != *want.Error {
				t.Errorf("%s: error %v, want %q", label, err, *want.Error)
			}
		case err != nil:
			t.Errorf("%s: %v", label, err)
		default:
			same(label, p, want.OK)
		}
	}
	for _, c := range g.Cases {
		desired, err := ParseDocument(c.Doc, cat)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("%s: error %v\n want %q", c.Name, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		same(c.Name+" configuration", Document(desired), c.Configuration)
		plan(c.Name+" plan", current, desired, false, c.Plan)
		plan(c.Name+" plan (pending)", current, desired, true, c.PlanPending)
		plan(c.Name+" plan (nothing saved)", nil, desired, false, c.PlanFresh)
		if got := Render(desired); got != c.Rendered {
			t.Errorf("%s render:\n%s\nwant\n%s", c.Name, got, c.Rendered)
		}
	}
}
