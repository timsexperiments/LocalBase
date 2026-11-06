//go:build !poolofone

package store_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Code that holds a connection, in a transaction or with rows open, and asks for another can wait for
// ever once the pool is full. A pool of one shows it at once, as a test that runs out of time. This
// runs those packages again built that way. It only sees paths their tests reach.
func TestNothingNeedsTwoConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs other packages' tests again")
	}
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// These hold a connection on purpose, to stand in for a writer or to fill the pool.
	const skip = "TestPolicyLookupDoesNotTakeTheWriteLock"
	for _, run := range [][]string{
		{"test", "-count=1", "-timeout=240s", "-skip", skip, "./internal/auth", "./internal/store", "./internal/gateway"},
		// A real gateway process, built with the same tag through GOFLAGS.
		{"test", "-count=1", "-timeout=240s", "-run", "TestLimitedChangesCannotPushOutWhoChangedKeys|TestRefusedChangesCannotPushOutWhoChangedWhat|TestInstallCancel", "./contract"},
	} {
		cmd := exec.Command("go", run...)
		cmd.Dir = moduleRoot
		cmd.Env = append(os.Environ(), "GOFLAGS=-tags=poolofone")
		started := time.Now()
		out, err := cmd.CombinedOutput()
		t.Logf("%s: %v", strings.Join(run[len(run)-1:], " "), time.Since(started).Round(time.Second))
		if err != nil {
			text := string(out)
			if i := strings.Index(text, "panic: test timed out"); i >= 0 {
				text = text[i:min(len(text), i+600)]
			} else if len(text) > 3000 {
				text = text[len(text)-3000:]
			}
			t.Errorf("with a pool of one: %v\n%s", err, text)
		}
	}
}
