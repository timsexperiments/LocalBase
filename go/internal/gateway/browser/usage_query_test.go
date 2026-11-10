package browser

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// What the browser sends usage keeps its query, and nothing else.
func TestUsageForwardsItsQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/app/api/_localbase/usage?range=7d&scope=all&group=model&group=route&secret=x", nil)
	got := forwardedQuery("/_localbase/usage", r)
	for _, want := range []string{"range=7d", "scope=all", "group=model", "group=route"} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded %q: missing %s", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("forwarded %q", got)
	}
}
