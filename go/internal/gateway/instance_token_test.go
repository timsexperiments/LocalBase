package gateway

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

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
