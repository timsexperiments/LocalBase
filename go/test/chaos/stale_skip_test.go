package chaos

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fix can reach a tree while its pull request is still open. Then the test of it goes on
// skipping, CI stays green without running it, and scripts/known-bugs-check.sh, which reads the
// pull request's state, has nothing to say. So a skip has to be true: after a clean run each
// skipped test runs again with its skip off, and one that passes fails the run.
var skips struct {
	sync.Mutex
	prs map[string]int
}

func noteSkip(test string, pr int) {
	skips.Lock()
	defer skips.Unlock()
	if skips.prs == nil {
		skips.prs = map[string]int{}
	}
	skips.prs[test] = pr
}

func reportStaleSkips() int {
	skips.Lock()
	defer skips.Unlock()
	code := 0
	for test, pr := range skips.prs {
		if passesWithItsSkipOff(test) {
			fmt.Fprintf(os.Stderr, "stale skip: %s passes with its skip off, so the bug it names (#%d) is fixed here and the test is not running. Remove its knownBug line.\n", test, pr)
			code = 1
		}
	}
	return code
}

func passesWithItsSkipOff(test string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	parts := strings.Split(test, "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+strings.Join(parts, "/"), "-test.count=1", "-test.v", "-test.timeout=4m")
	cmd.Env = append(os.Environ(), "LOCALBASE_CHAOS_ALL=1")
	cmd.WaitDelay = 10 * time.Second // a child left holding the pipe must not hold this up
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), "--- PASS: "+test+" ")
}

const stalePlant = "LOCALBASE_CHAOS_STALE_PLANT"

// The check can fail, and leaves alone what it should: this test runs itself with a skip whose
// test passes, which must fail the run, and with one whose test still fails, which must not.
func TestStaleSkipFailsTheRun(t *testing.T) {
	// Called pr so scripts/known-bugs-check.sh, which reads the source, takes these for the helper.
	const pr = noPRYet
	switch os.Getenv(stalePlant) {
	case "passes":
		t.Run("planted", func(t *testing.T) { knownBug(t, pr, "planted: passes with its skip off") })
		return
	case "fails":
		t.Run("planted", func(t *testing.T) {
			knownBug(t, pr, "planted: still fails with its skip off")
			t.Fatal("planted: the bug is still here")
		})
		return
	}
	run := func(mode string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStaleSkipFailsTheRun$", "-test.count=1")
		cmd.Env = append(os.Environ(), stalePlant+"="+mode, "LOCALBASE_CHAOS_ALL=")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("passes")
	if err == nil || !strings.Contains(out, "stale skip: TestStaleSkipFailsTheRun/planted") {
		t.Errorf("a skip whose test passes did not fail the run: err %v, output:\n%s", err, out)
	}
	if out, err := run("fails"); err != nil || strings.Contains(out, "stale skip") {
		t.Errorf("a skip whose test still fails was called stale: err %v, output:\n%s", err, out)
	}
}
