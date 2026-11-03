package runtime

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// knownBug skips a test of an open bug unless LOCALBASE_CHAOS_ALL is set, and names the pull
// request that fixes it. That fix removes the line. tools/known-bugs-check.sh fails once the
// named pull request has merged, or if there is none.
func knownBug(t *testing.T, pr int, bug string) {
	t.Helper()
	if os.Getenv("LOCALBASE_CHAOS_ALL") == "" {
		t.Skipf("known bug, fixed by #%d: %s", pr, bug)
	}
}

// countLines counts the output lines that reach the log, taking a while over each.
type countLines struct {
	n     *atomic.Int64
	delay time.Duration
}

func (c countLines) Enabled(context.Context, slog.Level) bool { return true }
func (c countLines) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c countLines) WithGroup(string) slog.Handler            { return c }
func (c countLines) Handle(_ context.Context, r slog.Record) error {
	if _, err := strconv.Atoi(r.Message); err == nil {
		time.Sleep(c.delay)
		c.n.Add(1)
	}
	return nil
}

// What a process wrote before it exited reaches the log, however slowly the log takes it:
// it may be the reason for a crash. It writes more than a pipe holds, so it exits with some
// still in the pipe.
//
// Seen unprovoked as TestBackendOutputRedactsURLs on macOS CI, with an empty log after its full
// wait: job 125685743835 (#506 at fad49b3b) and job 125706211906 (#521 at fabba49a).
func TestProcessOutputSurvivesItsExit(t *testing.T) {
	knownBug(t, 522, "a backend's last output is lost when it exits: cmd.Wait closes the pipes while they are being read")
	const lines = 40000 // about 230 KB
	var n atomic.Int64
	p, err := StartProcess("child", "/bin/sh", []string{"-c", "seq 1 " + strconv.Itoa(lines)}, nil, slog.New(countLines{&n, 30 * time.Microsecond}))
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	for last := int64(-1); n.Load() != last; time.Sleep(600 * time.Millisecond) { // until it stops moving
		last = n.Load()
	}
	if got := n.Load(); got != lines {
		t.Errorf("%d of %d lines reached the log", got, lines)
	}
}

// A descendant can hold the output pipes open long after the process has gone, even from
// outside its process group, so nothing stops it. The process must still end: waiting for
// end-of-file with no limit would hang here. This guards the fix for the test above.
func TestProcessEndsWhileADescendantHoldsItsOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := `perl -MPOSIX -e 'POSIX::setsid(); open(F, ">", $ARGV[0]) or die; print F $$; close F; sleep 60' "$1" &
while [ ! -s "$1" ]; do :; done
echo started`
	var n atomic.Int64
	p, err := StartProcess("child", "/bin/sh", []string{"-c", script, "sh", pidFile}, nil, slog.New(countLines{&n, 0}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the process never ended while a descendant held its output")
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("the descendant never started, so this proved nothing: %v", err)
	}
}
