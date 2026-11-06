package contract

import (
	"os/exec"
	"testing"
)

// TestChatJSAgainstTS runs go/tools/jstest: the Go UI's chat and markdown
// modules against the TS UI functions they port (bun, with the TS sources).
func TestChatJSAgainstTS(t *testing.T) {
	requireTS(t)
	cmd := exec.Command("bun", "test", "go/tools/jstest")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bun test: %v\n%s", err, out)
	}
}
