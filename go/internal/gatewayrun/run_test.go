package gatewayrun

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/audit"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// runRoot is a new root with the default configuration, and an Opener for it that counts its
// opens and closes.
type runRoot struct {
	dir    string
	mu     sync.Mutex
	opened int
	closed int
}

func newRunRoot(t *testing.T) *runRoot {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	rr := &runRoot{dir: filepath.Join(t.TempDir(), "root")}
	d, closeDeps, err := rr.load()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeps()
	cfg := conf.Default(d.Root.Dir, 16, d.Catalog)
	if err := conf.Check(cfg, d.Catalog); err != nil {
		t.Fatal(err)
	}
	for _, k := range store.Kinds {
		if err := os.MkdirAll(d.Root.ModelsDir(k), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Store.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	return rr
}

func (rr *runRoot) load() (Deps, func(), error) {
	r, err := root.Init(rr.dir)
	if err != nil {
		return Deps{}, nil, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return Deps{}, nil, err
	}
	cat, err := catalog.LoadRoot(r.Dir, nil)
	if err != nil {
		st.Close()
		return Deps{}, nil, err
	}
	d := Deps{Root: r, Store: st, Catalog: cat, Host: memory.NewHostProvider(), Audit: audit.New(r, io.Discard)}
	if cfg, err := st.LoadConfig(context.Background()); err == nil {
		d.Config = cfg
	} else if !errors.Is(err, store.ErrNotConfigured) {
		st.Close()
		return Deps{}, nil, err
	}
	return d, func() { st.Close() }, nil
}

// open is the Opener a run gets.
func (rr *runRoot) open() (Deps, func(), error) {
	d, closeDeps, err := rr.load()
	if err != nil {
		return Deps{}, nil, err
	}
	rr.mu.Lock()
	rr.opened++
	rr.mu.Unlock()
	return d, func() {
		closeDeps()
		rr.mu.Lock()
		rr.closed++
		rr.mu.Unlock()
	}, nil
}

func (rr *runRoot) counts() (opened, closed int) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.opened, rr.closed
}

// calls records what a run told its Reporter, in order.
type calls struct {
	mu  sync.Mutex
	got []string
}

func (c *calls) add(s string) { c.mu.Lock(); c.got = append(c.got, s); c.mu.Unlock() }
func (c *calls) Streaming()   { c.add("streaming") }
func (c *calls) Started(baseURL string, enabled map[string]bool, loginLink string) {
	c.add("started")
}
func (c *calls) Stopped() { c.add("stopped") }
func (c *calls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

// runOptions is what serve --no-auth on a port gives, as a user who isn't root.
func runOptions(port int) Options {
	return Options{NoAuth: true, Port: port, AuthMode: "either", effectiveUID: func() int { return 501 }}
}

// A gateway runs the backends it downloads and reads model files from anywhere, so as root all of
// that runs as root. It stops before it opens the root, binds or changes anything, unless asked not
// to, and says how to go ahead.
func TestRunRefusesRootBeforeOpeningIt(t *testing.T) {
	rr := newRunRoot(t)
	opts := runOptions(testutil.FreePort())
	opts.effectiveUID = func() int { return 0 }
	rep := &calls{}
	err := Run(opts, rr.open, rep)
	var bad *BadInput
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "Refusing to serve as root") || !strings.Contains(bad.Message, "--allow-root") {
		t.Fatalf("err %v", err)
	}
	if opened, _ := rr.counts(); opened != 0 {
		t.Errorf("the root was opened %d times", opened)
	}
	if got := rep.list(); len(got) != 0 {
		t.Errorf("reported %q", got)
	}
}

// The port here is held, so getting past the check ends at the bind and not in serving.
func TestRunAsRootWhenAskedTo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uid       int
		allowRoot bool
	}{{"root, allowed", 0, true}, {"an ordinary user", 501, false}} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newRunRoot(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			opts := runOptions(ln.Addr().(*net.TCPAddr).Port)
			opts.AllowRoot = tc.allowRoot
			opts.effectiveUID = func() int { return tc.uid }
			err = Run(opts, rr.open, &calls{})
			var bad *BadInput
			if err == nil || errors.As(err, &bad) || !strings.Contains(err.Error(), "address already in use") {
				t.Errorf("err %v", err)
			}
			if opened, closed := rr.counts(); opened != 1 || closed != 1 {
				t.Errorf("opened %d, closed %d", opened, closed)
			}
		})
	}
}

// A stop signal that arrives while the run is still starting waits its turn: it finishes starting,
// stops in order, closes what it opened, releases the root's lock and returns nil (review 01-M6).
// Before, the handler was only installed once startup was done, so the signal took Go's default
// action and no deferred cleanup ran. That can't be shown against the old code from inside a test:
// the same SIGTERM would end the test process itself.
func TestASignalDuringStartupStopsInOrder(t *testing.T) {
	rr := newRunRoot(t)
	opts := runOptions(testutil.FreePort())
	opts.afterSignals = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
	rep := &calls{}
	done := make(chan error, 1)
	go func() { done <- Run(opts, rr.open, rep) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("after a signal during startup: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run didn't stop on a signal sent during its startup")
	}
	if opened, closed := rr.counts(); opened != 1 || closed != 1 {
		t.Errorf("opened %d, closed %d", opened, closed)
	}
	if got, want := rep.list(), []string{"streaming", "started", "stopped"}; !slices.Equal(got, want) {
		t.Errorf("reported %q, want %q", got, want)
	}
	if state, _, _ := (&root.Root{Dir: rr.dir}).OwnerState(); state == "running" {
		t.Errorf("the gateway lock is still held after the run returned (%s)", state)
	}
}
