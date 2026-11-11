package contract

import (
	"os/exec"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestChatJSAgainstTS runs go/tools/jstest: the Go UI's chat and markdown
// modules against the TS UI functions they port (bun, with the TS sources).
func TestChatJSAgainstTS(t *testing.T) {
	harness.RequireTS(t)
	cmd := exec.Command("bun", "test", "go/tools/jstest")
	cmd.Dir = testutil.RepoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bun test: %v\n%s", err, out)
	}
}
