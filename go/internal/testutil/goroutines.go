package testutil

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Cleaner is the part of testing.TB that NoLeaks needs, kept apart from testing for the same
// reason as Fataler.
type Cleaner interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// Goroutines is the set of goroutines running when it was taken, by id.
type Goroutines map[int]bool

// ignoredGoroutines are goroutines a test doesn't own: the runtime's, the test runner's, and an
// HTTP client's idle connections, which outlive the server they talked to until the client is
// collected.
var ignoredGoroutines = []string{
	"testing.(*T).Run",
	"testing.(*M).",
	"testing.tRunner",
	"testing.runFuzzing",
	"runtime.goexit0",
	"os/signal.signal_recv",
	"runtime.ensureSigM",
	"net/http.(*persistConn).readLoop",
	"net/http.(*persistConn).writeLoop",
}

// Snapshot is the goroutines running now.
func Snapshot() Goroutines {
	now := Goroutines{}
	for _, g := range stacks() {
		now[g.id] = true
	}
	return now
}

// Leaked is the stack of every goroutine started since g that is still running after up to wait,
// leaving out what ignoredGoroutines and ignore name. It looks again every 10 ms, so a goroutine
// that is stopping gets that long to end.
func (g Goroutines) Leaked(wait time.Duration, ignore ...string) []string {
	deadline := time.Now().Add(wait)
	for {
		var left []string
		for _, s := range stacks() {
			if !g[s.id] && !ignored(s.text, ignore) {
				left = append(left, s.text)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// NoLeaks fails t if a goroutine started during the test is still running once its other
// Cleanups have run, after up to 5 s. Call it first in a test: Cleanups run last-registered first,
// so this one runs after the ones that stop what the test started. Tests that run in parallel
// would see each other's goroutines, so it is for tests that don't.
func NoLeaks(t Cleaner, ignore ...string) {
	t.Helper()
	before := Snapshot()
	t.Cleanup(func() {
		if left := before.Leaked(5*time.Second, ignore...); len(left) > 0 {
			t.Errorf("%d goroutines started by the test are still running:\n\n%s", len(left), strings.Join(left, "\n\n"))
		}
	})
}

type goroutine struct {
	id   int
	text string
}

func ignored(stack string, ignore []string) bool {
	for _, lists := range [][]string{ignoredGoroutines, ignore} {
		for _, s := range lists {
			if strings.Contains(stack, s) {
				return true
			}
		}
	}
	return false
}

// stacks is every goroutine's stack but the caller's own.
func stacks() []goroutine {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []goroutine
	for i, block := range bytes.Split(buf, []byte("\n\n")) {
		if i == 0 {
			continue // the caller's own goroutine comes first
		}
		head, _, _ := bytes.Cut(block, []byte("\n"))
		f := strings.Fields(string(head)) // "goroutine 12 [chan receive]:"
		if len(f) < 2 || f[0] != "goroutine" {
			continue
		}
		id, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out = append(out, goroutine{id: id, text: string(block)})
	}
	return out
}
