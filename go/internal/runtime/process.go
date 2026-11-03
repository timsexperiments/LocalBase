package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// Process is one supervised backend. It runs in its own process group so a
// stop reaches any children, and its stderr goes to the structured log.
type Process struct {
	Name string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	// endGuard stops the group's guard once the backend has exited (startGuard).
	endGuard func()
	guardPID int
	// ForeignListenerOK skips the listener check: a custom launch may start
	// something outside its group that serves the port (a container, say).
	ForeignListenerOK bool
	// startup keeps the first bytes of output, raw, for the placement check
	// (DESIGN §20.5): never decoded, bounded.
	startupMu sync.Mutex
	startup   []byte
}

const startupCapture = 4 << 20

// StartupLog is the raw start of the backend's output (stdout and stderr,
// as lines arrive).
func (p *Process) StartupLog() []byte {
	p.startupMu.Lock()
	defer p.startupMu.Unlock()
	return append([]byte(nil), p.startup...)
}

const maxLogLine = 4096

// outputLineBuffer is how much of an output line is kept for the startup capture.
const outputLineBuffer = 64 * 1024

// outputDrainLimit is how long Done waits for the output to end after the process and its
// group are gone. Reaching end-of-file normally takes no time at all; the limit is only for
// a descendant outside the group that holds the pipes open. A variable for its test.
var outputDrainLimit = 2 * time.Second

// StartProcess launches bin with args. Output lines are logged at debug
// level, bounded per line, under the runtime's name.
func StartProcess(name, bin string, args []string, env []string, log *slog.Logger) (*Process, error) {
	return StartProcessIn(name, bin, "", args, env, log)
}

// StartProcessIn is StartProcess with a working directory ("" = inherit).
func StartProcessIn(name, bin, dir string, args []string, env []string, log *slog.Logger) (*Process, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if env != nil {
		cmd.Env = env
	}
	// Our own pipes: cmd.Wait closes those from StdoutPipe as soon as the child exits, losing
	// whatever is still unread, which may be the reason it crashed.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return nil, err
	}
	p := &Process{Name: name, cmd: cmd, done: make(chan struct{})}
	drained := make(chan struct{})
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); p.drainOutput(stdout, log) }()
	go func() { defer drains.Done(); p.drainOutput(stderr, log) }()
	go func() { drains.Wait(); close(drained) }()
	p.startGuard(log)
	go func() {
		p.err = cmd.Wait()
		if p.endGuard != nil {
			p.endGuard()
		}
		p.endGroup(log)
		// All the output, unless a descendant outside the group holds the pipes open: that
		// one can outlive us, so the wait has a limit and Done always comes.
		select {
		case <-drained:
		case <-time.After(outputDrainLimit):
		}
		stdout.Close()
		stderr.Close()
		close(p.done)
	}()
	return p, nil
}

// drainOutput logs r's lines until it ends, however long they are. Giving up on a long line
// would stop reading, and a child whose pipe fills up then blocks for good.
func (p *Process) drainOutput(r io.Reader, log *slog.Logger) {
	br := bufio.NewReaderSize(r, outputLineBuffer)
	for {
		chunk, more, err := br.ReadLine()
		if err != nil {
			return
		}
		p.startupMu.Lock()
		if len(p.startup) < startupCapture {
			p.startup = append(p.startup, chunk...)
			p.startup = append(p.startup, '\n')
		}
		p.startupMu.Unlock()
		line := string(chunk)
		if len(line) > maxLogLine {
			line = line[:maxLogLine]
		}
		log.Debug(redactOutputURLs(line), "event", "runtime.output", "runtime", p.Name)
		for more { // the rest of a line too long to keep
			if _, more, err = br.ReadLine(); err != nil {
				return
			}
		}
	}
}

// sampleFootprint reads a backend's memory. A variable so a test can hold a sample.
var sampleFootprint = memory.Footprint

func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done is closed when the process exits.
func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) ExitErr() error { return p.err }

// How long a port may take to come free, for a backend that has just stopped, and
// how long a healthy answer waits to see the child survive.
const (
	portReleaseWait = time.Second
	groupGrace      = 2 * time.Second
	healthySettle   = 100 * time.Millisecond
)

// checkPortFree refuses a port something else holds, before a backend is spawned
// for it. The child would fail to bind and exit, while the other server answered
// the health probe: requests would then be relayed to a process this gateway did
// not start.
func checkPortFree(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(portReleaseWait)
	for {
		busy := portBusy(addr)
		if !busy {
			return nil
		}
		if time.Now().After(deadline) {
			owner := "another process"
			if pids, ok := listenerPIDs(port); ok {
				owner = fmt.Sprintf("another process (pid %d)", pids[0])
			}
			return fmt.Errorf("port %d on %s is already in use by %s, so no backend was started there; "+
				"stop that process or choose other ports", port, host, owner)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// portBusy tries both ways: on macOS a bind can succeed beside a wildcard listener,
// which a connection still reaches.
func portBusy(addr string) bool {
	if c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond); err == nil {
		c.Close()
		return true
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Is(err, syscall.EADDRINUSE)
	}
	ln.Close()
	return false
}

func urlPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(u.Port())
	return n
}

// WaitHealthy polls url until it returns 200, the process exits, or the
// startup budget runs out.
func (p *Process) WaitHealthy(ctx context.Context, url string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return fmt.Errorf("%s exited during startup: %v", p.Name, p.err)
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				// An answer proves nothing if the child is gone: it may be another
				// server on this port, which the child failed to bind.
				select {
				case <-p.done:
					return fmt.Errorf("%s exited during startup, and another process answered on its port: %v", p.Name, p.err)
				case <-time.After(healthySettle):
				}
				if !p.ForeignListenerOK {
					if port := urlPort(url); port > 0 {
						if pid := p.foreignListener(port); pid != 0 {
							return fmt.Errorf("%s did not answer its health check: pid %d, outside its process group, is listening on port %d", p.Name, pid, port)
						}
					}
				}
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("%s exited during startup: %v", p.Name, p.err)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s did not become healthy within %s", p.Name, budget)
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// endGroup ends what the leader left behind, such as the server a wrapper started,
// before anyone is told the process is done: that server holds the memory and the
// port. The wait is bounded, so Done always comes.
func (p *Process) endGroup(log *slog.Logger) {
	pgid := p.cmd.Process.Pid
	gone := func(d time.Duration) bool {
		for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if !groupAlive(pgid) {
				return true
			}
		}
		return !groupAlive(pgid)
	}
	if !groupAlive(pgid) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if gone(groupGrace) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if !gone(groupGrace) {
		log.Warn("A backend's process group is still there after SIGKILL; its memory may not be free.",
			"event", "runtime.group-survived", "runtime", p.Name, "pgid", pgid)
	}
}

// Stop sends SIGTERM to the process group, then SIGKILL after grace. It returns
// with the group gone.
func (p *Process) Stop(grace time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
}

var outputURLRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"<>]+`)

// redactOutputURLs takes credentials, query values and fragments out of URLs in a
// backend's output. llama-server logs the image URLs requests send.
func redactOutputURLs(line string) string {
	return outputURLRe.ReplaceAllStringFunc(line, acquire.RedactURL)
}

// spanned runs fn inside a backend lifecycle span (TS supervisor
// localbase.backend.start and localbase.backend.model_load); a failure is
// recorded on it, redacted.
func spanned(rt *otlp.Runtime, ctx context.Context, name, backendName string, fn func() error) error {
	_, sp := rt.Start(ctx, name, otlp.KindInternal, "localbase.backend", backendName)
	defer sp.End()
	err := fn()
	if err != nil {
		msg := obs.BoundedText(err.Error(), 2048)
		sp.RecordException("Error", msg)
		sp.SetError(msg)
	}
	return err
}
