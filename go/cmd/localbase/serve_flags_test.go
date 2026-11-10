package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// serve refuses a --log-level it doesn't know, before it starts anything, as logs --level does,
// instead of running at info (review 01-M3). The bad --port makes main fail fast too, but on the port.
func TestServeRefusesAnUnknownLogLevel(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	for _, level := range []string{"warning", "err", "INFO "} {
		code, out := runApp(t, "--root", root, "serve", "--log-level", level, "--port", "0")
		want := `log-level: Invalid option: expected one of "debug"|"info"|"warn"|"error"`
		if code != 2 || !strings.Contains(out, want) {
			t.Errorf("--log-level %q: exit %d, %s", level, code, out)
		}
	}
	// What worked before still does: these get past the level to the port.
	for _, level := range []string{"INFO", "warn", "debug", "error"} {
		if code, out := runApp(t, "--root", root, "serve", "--log-level", level, "--port", "0"); code != 2 || !strings.Contains(out, "--port must be") {
			t.Errorf("--log-level %q: exit %d, %s", level, code, out)
		}
	}
}

// A stop signal that arrives while serve is still starting waits its turn: serve finishes starting,
// stops in order, releases the root's lock and exits 0 (review 01-M6). Before, the handler was only
// installed once startup was done, so the signal took Go's default action and no deferred cleanup
// ran. That can't be shown against the old code from inside a test: the same SIGTERM would end the
// test process itself.
func TestASignalDuringStartupStopsInOrder(t *testing.T) {
	dir := noAuthRoot(t)
	port := strconv.Itoa(testutil.FreePort())
	afterSignals = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
	defer func() { afterSignals = func() {} }()
	done := make(chan struct{})
	var code int
	var out string
	go func() {
		defer close(done)
		code, out = runApp(t, "--root", dir, "serve", "--no-auth", "--no-llm", "--port", port)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("serve didn't stop on a signal sent during its startup")
	}
	if code != 0 {
		t.Errorf("exit %d after a signal during startup: %s", code, out)
	}
	if state, _, _ := (&root.Root{Dir: dir}).OwnerState(); state == "running" {
		t.Errorf("the gateway lock is still held after serve returned (%s)", state)
	}
}
