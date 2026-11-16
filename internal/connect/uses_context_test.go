package connect

import "testing"

// A client whose template holds @CONTEXT@ says so; one whose doesn't, doesn't. The figure itself is
// no clue: a port or a version can have the same digits.
func TestUsesContext(t *testing.T) {
	target := Target{BaseURL: "http://127.0.0.1:8080/v1", Model: "m", Context: 8080}
	for id, want := range map[string]bool{"opencode": true, "zed": true, "curl": false, "python": false} {
		c := Find(id, target)
		if c == nil {
			t.Fatalf("no client %q", id)
		}
		if c.UsesContext != want {
			t.Errorf("%s: UsesContext %v, want %v", id, c.UsesContext, want)
		}
	}
}
