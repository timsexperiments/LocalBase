package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

func TestTheInstanceTokenMustMatch(t *testing.T) {
	o := &root.Owner{InstanceToken: "the-token"}
	for _, c := range []struct {
		name  string
		owner *root.Owner
		sent  string
		want  bool
	}{
		{"the token", o, "the-token", true},
		{"another of the same length", o, "the-tokex", false},
		{"a prefix", o, "the-toke", false},
		{"nothing sent", o, "", false},
		{"no owner", nil, "the-token", false},
		{"no owner and nothing sent", nil, "", false},
		{"an owner with no token, and nothing sent", &root.Owner{}, "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.sent != "" {
			r.Header.Set("X-Localbase-Instance-Token", c.sent)
		}
		if got := instanceTokenMatches(c.owner, r); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// The token is a secret compared with one a caller sends. No test can tell how long a comparison
// took from whether it answered true, so this reads the routes: they go through the helper above.
// It reads the gateway and every package under it, which is where its routes' code is.
func TestNoRouteComparesTheInstanceTokenItself(t *testing.T) {
	direct := regexp.MustCompile(`[!=]= *[a-zA-Z.]*InstanceToken\b|\bInstanceToken *[!=]= *[^" ]`)
	for _, name := range testutil.GoSources(t, ".", gatewayPackages) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range direct.FindAll(b, -1) {
			t.Errorf("%s compares it directly: %s", name, m)
		}
	}
}
