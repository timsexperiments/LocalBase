package meta

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// A tier the catalog recorded wins; else a qualified entry is built-in and anything else is the user's.
func TestTier(t *testing.T) {
	for _, tc := range []struct {
		s    catalog.Spec
		want string
	}{
		{catalog.Spec{Tier: "remote", Trust: "qualified"}, "remote"},
		{catalog.Spec{Trust: "qualified"}, "built-in"},
		{catalog.Spec{Trust: "community"}, "user"},
		{catalog.Spec{}, "user"},
	} {
		if got := Tier(&tc.s); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.s, got, tc.want)
		}
	}
}
