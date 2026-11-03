package runtime

import (
	"os"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
)

// The listing and the start agree: an adapter called startable has a case in a start
// path, and one that is not has none. A new adapter in either place fails this.
func TestStartableAdaptersHaveARunner(t *testing.T) {
	var source strings.Builder
	for _, f := range []string{"service.go", "target.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		source.Write(b)
	}
	for _, a := range backends.Adapters {
		has := strings.Contains(source.String(), `case "`+a+`"`)
		if has != backends.Startable(a) {
			t.Errorf("%s: startable=%v, has a start path=%v", a, backends.Startable(a), has)
		}
	}
}
