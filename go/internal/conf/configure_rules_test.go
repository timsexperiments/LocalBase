package conf

import (
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// Only a built-in model of the asked kind passes, and what doesn't comes back in the order given.
func TestNotBuiltIn(t *testing.T) {
	builtIn, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var llm, stt string
	for _, s := range builtIn.All() {
		if llm == "" && s.Kind == "llm" {
			llm = s.ModelID
		}
		if stt == "" && s.Kind == "stt" {
			stt = s.ModelID
		}
	}
	if got := NotBuiltIn(builtIn, "llm", []string{llm}); len(got) != 0 {
		t.Errorf("a built-in LLM: %q", got)
	}
	if got := NotBuiltIn(builtIn, "llm", []string{"no-such", llm, stt, ""}); !slices.Equal(got, []string{"no-such", stt, ""}) {
		t.Errorf("mixed: %q", got)
	}
	if got := NotBuiltIn(builtIn, "stt", nil); got != nil {
		t.Errorf("none: %q", got)
	}
}

// An on/off setting changes when it moves between on and anything else; unset counts as off.
func TestToggleChanged(t *testing.T) {
	for _, tc := range []struct {
		was, value string
		want       bool
	}{{"", "on", true}, {"", "off", false}, {"off", "on", true}, {"on", "off", true}, {"on", "on", false}, {"off", "off", false}} {
		if got := ToggleChanged(tc.was, tc.value); got != tc.want {
			t.Errorf("%q -> %q: %v", tc.was, tc.value, got)
		}
	}
}
