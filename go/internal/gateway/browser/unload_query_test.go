package browser

import (
	"net/http"
	"net/url"
	"testing"
)

// What the browser sends unload keeps only `wait`, and nothing else on the way.
func TestUnloadForwardsOnlyWait(t *testing.T) {
	req := &http.Request{URL: &url.URL{RawQuery: "wait=false&x=1"}}
	if q := forwardedQuery("/_localbase/models/m/unload", req); q != "wait=false" {
		t.Errorf("forwarded query %q", q)
	}
	if q := forwardedQuery("/_localbase/models/m", req); q != "" {
		t.Errorf("forwarded query elsewhere %q", q)
	}
}
