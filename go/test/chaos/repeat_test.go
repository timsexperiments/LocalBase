package chaos

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// A test run twice in one process (go test -count=2) starts its gateway under the same name both
// times, since the name is the test's. The second must start on a root of its own: the first may
// have left a damaged database behind, as the "damaged" case here does on purpose.
func TestAGatewayOfANameUsedBeforeStartsFresh(t *testing.T) {
	_, first, _ := start(t, lab.Options{})
	if took, err := first.Stop(30 * time.Second); err != nil {
		t.Fatalf("the first gateway did not stop cleanly after %v: %v", took.Round(time.Millisecond), err)
	}
	// What a test that damages its database leaves behind.
	if err := os.WriteFile(filepath.Join(first.Root, "local-base.db"), bytes.Repeat([]byte{0xDB}, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, second, c := start(t, lab.Options{})
	if second.Root == first.Root {
		t.Fatalf("the second gateway runs on the first one's root, %s", first.Root)
	}
	if r := c.Do(ctx, "GET", "/v1/models", second.Keys.Admin, "", nil); r.Err != nil || r.Status != 200 {
		t.Errorf("the second gateway: %d %v %.200s", r.Status, r.Err, r.Body)
	}
}
