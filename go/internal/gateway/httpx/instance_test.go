package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
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
		if got := InstanceTokenMatches(c.owner, r); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
