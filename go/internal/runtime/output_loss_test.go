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

// The limit is on silence, not on how long the log takes: a slow log still gets the last
// lines, which are the ones that may say why. Here reading takes far longer than the limit.
func TestProcessOutputWaitsWhileItArrives(t *testing.T) {
	limit := outputDrainLimit
	outputDrainLimit = 300 * time.Millisecond
	defer func() { outputDrainLimit = limit }()
	const lines = 20000 // about 110 KB: more than the reader's buffer and the pipe together hold at once
	var n atomic.Int64
	p, err := StartProcess("child", "/bin/sh", []string{"-c", "seq 1 " + strconv.Itoa(lines)}, nil, slog.New(countLines{&n, 100 * time.Microsecond}))
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

// A descendant that keeps writing keeps the output "arriving", so the cap ends the wait.
func TestProcessEndsWhileADescendantKeepsWriting(t *testing.T) {
	limit, most := outputDrainLimit, outputDrainCap
	outputDrainCap = 3 * time.Second
	defer func() { outputDrainLimit, outputDrainCap = limit, most }()
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := `perl -MPOSIX -e 'POSIX::setsid(); open(F, ">", $ARGV[0]) or die; print F $$; close F; $|=1; while (1) { print "1\n"; select(undef, undef, undef, 0.05) }' "$1" &
while [ ! -s "$1" ]; do :; done`
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
		t.Fatal("the process never ended while a descendant kept writing")
	}
	if n.Load() == 0 {
		t.Fatal("the descendant wrote nothing, so this proved nothing")
	}
}

// With nothing holding its output, a process does not wait for the limit: stopping a backend
// costs nothing extra.
func TestProcessEndWaitsOnlyForItsOutput(t *testing.T) {
	limit := outputDrainLimit
	outputDrainLimit = time.Minute
	defer func() { outputDrainLimit = limit }()
	var n atomic.Int64
	p, err := StartProcess("child", "/bin/sh", []string{"-c", "echo 1"}, nil, slog.New(countLines{&n, 0}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the process waited for the drain limit with nothing holding its output")
	}
	if n.Load() != 1 {
		t.Errorf("%d lines reached the log, want 1", n.Load())
	}
}

// A line longer than any buffer costs only itself: the lines after it still arrive, and the
// child is not left blocked on a full pipe. The crash reason usually comes last.
func TestProcessOutputSurvivesALongLine(t *testing.T) {
	var n atomic.Int64
	script := `head -c 3000000 /dev/zero | tr '\0' x; echo; echo 1; echo 2`
	p, err := StartProcess("child", "/bin/sh", []string{"-c", script}, nil, slog.New(countLines{&n, 0}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("a long line left the child blocked")
	}
	if n.Load() != 2 {
		t.Errorf("%d of the 2 lines after the long one reached the log", n.Load())
	}
}
