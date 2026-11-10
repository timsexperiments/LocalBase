package infer

import "testing"

func TestAnchorPattern(t *testing.T) {
	for in, want := range map[string]string{"a": "^a$", "^a$": "^a$", "a|b": "^(?:a|b)$", `a\$`: `^a\$$`, "^a": "^a$"} {
		if got := anchorPattern(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
