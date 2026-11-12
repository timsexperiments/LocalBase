package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
)

const (
	noneChosen     = "LocalBase doesn’t pick a custom backend by itself. Choose one below to run this model."
	noneCompatible = "No backend was detected as compatible. Select a backend to run this model."
)

// LocalBase never picks a custom backend by itself (#534). When one that can run the model is among
// the choices, the panel says so; when none can, it says that instead. The two never swap.
func TestBlankBackendSaysWhy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(backends.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	def := `{"schema":"localbase.backend/v1","id":"my-custom","name":"My custom","engine":"x","adapter":"process-openai/v1","tasks":["chat"],"formats":["gguf"],` +
		`"capabilities":[],"architectures":{"supported":["x"]},"platforms":[{"os":"darwin","arch":"arm64","accelerator":"metal"}],` +
		`"launch":{"command":"/bin/sh","args":["{port}"],"memory":{"weightsFactor":1,"overheadGb":0,"assumption":"x"}}}`
	if err := os.WriteFile(filepath.Join(backends.Dir(root), "my-custom.json"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := backends.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	builtIn := ""
	for _, d := range reg.All() {
		if d.BuiltIn {
			builtIn = d.ID
			break
		}
	}
	if reg.Get("my-custom") == nil || builtIn == "" {
		t.Fatalf("registry: custom %v, a built-in %q", reg.Get("my-custom") != nil, builtIn)
	}
	rec := func(o ...backends.Finding) backends.Recommendation { return backends.Recommendation{Options: o} }
	for _, c := range []struct {
		name string
		rec  backends.Recommendation
		want bool
	}{
		{"a custom backend that can run it", rec(backends.Finding{BackendID: "my-custom", Rating: backends.Supported}), true},
		{"a custom backend that likely can", rec(backends.Finding{BackendID: "my-custom", Rating: backends.Likely}), true},
		{"a custom backend that can't", rec(backends.Finding{BackendID: "my-custom", Rating: backends.Incompatible}), false},
		{"only built-ins", rec(backends.Finding{BackendID: builtIn, Rating: backends.Supported}), false},
		{"nothing", rec(), false},
	} {
		if got := customChoice(reg, c.rec); got != c.want {
			t.Errorf("%s: customChoice %v, want %v", c.name, got, c.want)
		}
	}
	// The panel, both ways: each case shows its own words and never the other's.
	u := New(Deps{NoAuth: true})
	render := func(v backendView) string {
		var b strings.Builder
		if err := u.pages["details"].ExecuteTemplate(&b, "backend-panel", v); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	opts := []optionView{{ID: "my-custom", Name: "My custom", Rating: backends.Supported, RatingLabel: "Supported"}}
	custom := render(backendView{ModelID: "m", Options: opts, CustomChoice: true})
	if !strings.Contains(custom, ">No backend chosen<") || !strings.Contains(custom, noneChosen) || strings.Contains(custom, noneCompatible) {
		t.Errorf("a compatible custom backend among the choices: %.500s", custom)
	}
	// "Choose one below" is true: the choices come after the words.
	if at, list := strings.Index(custom, noneChosen), strings.Index(custom, `id="backend-select"`); at < 0 || list < at {
		t.Errorf("the choices aren't below the words: words at %d, list at %d", at, list)
	}
	none := render(backendView{ModelID: "m", Options: opts})
	if !strings.Contains(none, noneCompatible) || strings.Contains(none, noneChosen) || strings.Contains(none, "No backend chosen") {
		t.Errorf("no compatible backend: %.500s", none)
	}
}
