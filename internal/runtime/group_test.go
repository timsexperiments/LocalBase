package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A wrapper that started the real server and exited. readChild returns the
// server's pid, written by the script.
func wrapper(t *testing.T, script string) (*Process, func() int) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "child")
	p, err := StartProcess("wrapper", "/bin/sh", []string{"-c", strings.ReplaceAll(script, "$OUT", out)}, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return p, func() int {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			b, _ := os.ReadFile(out)
			if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
				return n
			}
		}
		t.Fatal("the wrapper reported no child")
		return 0
	}
}

// Done means the memory and the port are free: the whole group is gone, however
// the leader went. The reservation is released on it.
func TestDoneWaitsForTheWholeGroup(t *testing.T) {
	p, child := wrapper(t, `sleep 60 & echo $! > $OUT; exit 0`)
	pid := child()
	<-p.Done()
	if pidExists(pid) {
		t.Errorf("the wrapper's child %d is alive after Done", pid)
	}
}

// Stop returns with the group gone, not when the leader is.
func TestStopWaitsForTheWholeGroup(t *testing.T) {
	p, child := wrapper(t, `trap 'exit 0' TERM; sh -c "trap '' TERM; exec sleep 60" & echo $! > $OUT; wait`)
	pid := child()
	time.Sleep(100 * time.Millisecond) // the child is past its trap
	began := time.Now()
	p.Stop(5 * time.Second)
	if pidExists(pid) {
		t.Errorf("Stop returned after %v with child %d alive", time.Since(began), pid)
	}
}
