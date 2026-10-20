package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"syscall"
	"time"
)

// Process is one supervised backend. It runs in its own process group so a
// stop reaches any children, and its stderr goes to the structured log.
type Process struct {
	Name string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

const maxLogLine = 4096

// StartProcess launches bin with args. Output lines are logged at debug
// level, bounded per line, under the runtime's name.
func StartProcess(name, bin string, args []string, env []string, log *slog.Logger) (*Process, error) {
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if env != nil {
		cmd.Env = env
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{Name: name, cmd: cmd, done: make(chan struct{})}
	drain := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if len(line) > maxLogLine {
				line = line[:maxLogLine]
			}
			log.Debug(line, "event", "runtime.output", "runtime", name)
		}
	}
	go drain(stdout)
	go drain(stderr)
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done is closed when the process exits.
func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) ExitErr() error { return p.err }

// WaitHealthy polls url until it returns 200, the process exits, or the
// startup budget runs out.
func (p *Process) WaitHealthy(ctx context.Context, url string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
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

// Stop sends SIGTERM to the process group, then SIGKILL after grace.
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
