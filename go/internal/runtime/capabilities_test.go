package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type capCase struct {
	ModelID  string `json:"modelId"`
	Detected *struct {
		Capabilities    [][2]any `json:"capabilities"`
		InputModalities []string `json:"inputModalities"`
		Evidence        []string `json:"evidence"`
	} `json:"detected"`
	Runtime *struct {
		Capabilities    map[string]bool `json:"capabilities"`
		InputModalities []string        `json:"inputModalities"`
	} `json:"runtime"`
	Capabilities    []catalog.Capability `json:"capabilities"`
	InputModalities struct {
		Values []string `json:"values"`
		Source string   `json:"source"`
	} `json:"inputModalities"`
	Mismatches []Mismatch `json:"mismatches"`
}

func loadCapCases(t *testing.T) []capCase {
	b, err := os.ReadFile("testdata/capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct{ Cases []capCase }
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g.Cases
}

func (c capCase) detected() *Detected {
	if c.Detected == nil {
		return nil
	}
	d := Detected{Capabilities: Opinions{}, InputModalities: c.Detected.InputModalities, Evidence: c.Detected.Evidence}
	for _, kv := range c.Detected.Capabilities {
		d.set(kv[0].(string), kv[1].(bool))
	}
	return &d
}

// TestResolveCapabilitiesMatchesTypeScript feeds TS's detected opinions and
// synthetic runtime reports through the Go resolver.
func TestResolveCapabilitiesMatchesTypeScript(t *testing.T) {
	cat, _ := catalog.Load("")
	for _, c := range loadCapCases(t) {
		spec := cat.ByID(c.ModelID)
		if spec == nil {
			t.Fatalf("%s not in the Go catalog", c.ModelID)
		}
		var rt *store.RuntimeCapabilities
		if c.Runtime != nil {
			rt = &store.RuntimeCapabilities{Capabilities: c.Runtime.Capabilities, InputModalities: c.Runtime.InputModalities}
		}
		got := ResolveCapabilities(spec, c.detected(), rt)
		if !reflect.DeepEqual(nilIfEmpty(got.Capabilities), nilIfEmpty(c.Capabilities)) {
			t.Errorf("%s runtime=%v capabilities:\n go %v\n ts %v", c.ModelID, c.Runtime != nil, got.Capabilities, c.Capabilities)
		}
		if !reflect.DeepEqual(got.InputModalities, c.InputModalities.Values) || got.InputSource != c.InputModalities.Source {
			t.Errorf("%s input modalities: go %v/%s ts %v/%s", c.ModelID, got.InputModalities, got.InputSource,
				c.InputModalities.Values, c.InputModalities.Source)
		}
		if !reflect.DeepEqual(nilIfEmpty(got.Mismatches), nilIfEmpty(c.Mismatches)) {
			t.Errorf("%s mismatches:\n go %+v\n ts %+v", c.ModelID, got.Mismatches, c.Mismatches)
		}
	}
}

// TestDetectCapabilitiesMatchesTypeScript re-reads the files the golden was
// built from (LOCALBASE_TEST_MODELS_ROOT: the models/ directory, read only).
func TestDetectCapabilitiesMatchesTypeScript(t *testing.T) {
	root := os.Getenv("LOCALBASE_TEST_MODELS_ROOT")
	if root == "" {
		t.Skip("set LOCALBASE_TEST_MODELS_ROOT to the models directory the golden file was built from")
	}
	cat, _ := catalog.Load("")
	for _, c := range loadCapCases(t) {
		if c.Runtime != nil {
			continue
		}
		spec := cat.ByID(c.ModelID)
		got := DetectCapabilities(spec, filepath.Join(root, spec.Kind))
		want := c.detected()
		if (got == nil) != (want == nil) {
			t.Errorf("%s: go %+v ts %+v", c.ModelID, got, want)
			continue
		}
		if got != nil && (!reflect.DeepEqual(got.Order, want.Order) || !reflect.DeepEqual(got.Capabilities, want.Capabilities) ||
			!reflect.DeepEqual(got.InputModalities, want.InputModalities) || !reflect.DeepEqual(nilIfEmpty(got.Evidence), nilIfEmpty(want.Evidence))) {
			t.Errorf("%s:\n go %+v\n ts %+v", c.ModelID, *got, *want)
		}
	}
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestAnalyzeChatTemplate(t *testing.T) {
	if o := AnalyzeChatTemplate("{% for m in messages %}{{ m.content }}{% endfor %}"); o["tool-calling"] || o["reasoning"] {
		t.Errorf("plain template: %v", o)
	}
	if o := AnalyzeChatTemplate("{% if tools %}<think>{% endif %}"); !o["tool-calling"] || !o["reasoning"] {
		t.Errorf("tool template: %v", o)
	}
}
