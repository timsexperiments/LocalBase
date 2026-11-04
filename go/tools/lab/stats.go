package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stats is the gateway process's resources at one moment.
type Stats struct {
	At          time.Time `json:"at"`
	Goroutines  int       `json:"goroutines"`
	FDs         int       `json:"fds"`
	Threads     int       `json:"threads"`
	HeapInuse   int64     `json:"heapInuse"`
	HeapAlloc   int64     `json:"heapAlloc"`
	HeapObjects int64     `json:"heapObjects"`
	Sys         int64     `json:"sys"`
	NumGC       int64     `json:"numGC"`
	RSS         int64     `json:"rss"`
	// Files is set by Settled, not Stats: counting them means asking the system.
	Files DatabaseFiles `json:"dbFiles"`
}

var debugClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// Stats reads the debug listener and ps.
func (g *Gateway) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+g.Debug+"/debug/stats", nil)
	resp, err := debugClient.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return s, err
	}
	s.At = time.Now()
	s.RSS = rssOf(g.Cmd.Process.Pid)
	return s, nil
}

// GoroutineDump writes the debug listener's goroutine profile to path, for a
// leak report.
func (g *Gateway) GoroutineDump(ctx context.Context, path string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+g.Debug+"/debug/pprof/goroutine?debug=1", nil)
	resp, err := debugClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// Proc is one row of the process table.
type Proc struct {
	PID, PPID int
	State     string
	Command   string
}

// Backends lists the fake backends running from this workspace, whoever their
// parent is now (an orphan's parent is init).
func (w *Workspace) Backends() ([]Proc, error) {
	all, err := procsUnder(w.Bin)
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, p := range all {
		if p.Command != "local-base" {
			out = append(out, p)
		}
	}
	return out, nil
}

// KillStray kills everything still running from this workspace's binaries:
// only exact paths under its own scratch directory.
func (w *Workspace) KillStray() {
	procs, err := procsUnder(w.Bin)
	if err != nil {
		return
	}
	for _, p := range procs {
		_ = syscall.Kill(p.PID, syscall.SIGKILL)
	}
}

// watchOwner stops the process group pid and every binary under bin if this
// process dies first (a panic or SIGKILL skips all cleanup). The watcher exits
// once pid is gone, unless it must clean up first.
func watchOwner(pid int, bin string) {
	script := fmt.Sprintf(`while kill -0 %d 2>/dev/null && kill -0 %d 2>/dev/null; do sleep 1; done
if kill -0 %d 2>/dev/null; then exit 0; fi
kill -TERM -%d 2>/dev/null
sleep 5
kill -KILL -%d 2>/dev/null
for exe in /proc/[0-9]*/exe; do
  case "$(readlink "$exe" 2>/dev/null)" in %s*) stray=${exe#/proc/}; kill -KILL "${stray%%/exe}" 2>/dev/null;; esac
done
if command -v ps >/dev/null; then ps -A -o pid=,command= | awk -v p=%q 'index($2, p) == 1 { print $1 }' | while read -r stray; do kill -KILL "$stray" 2>/dev/null; done; fi`,
		os.Getpid(), pid, os.Getpid(), pid, pid, bin+string(filepath.Separator), bin+string(filepath.Separator))
	w := exec.Command("sh", "-c", script)
	w.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if w.Start() == nil {
		go func() { _ = w.Wait() }()
	}
}

// Settled samples the gateway once it has freed what it can and its goroutine
// count has stopped moving, so a figure is not a GC or connection-close artifact.
func (g *Gateway) Settled(ctx context.Context) (Stats, error) {
	s, err := g.settle(ctx)
	if err != nil {
		return s, err
	}
	if s.Files, err = g.DatabaseFiles(); err != nil {
		return s, fmt.Errorf("counting the descriptors on the database: %w", err)
	}
	return s, nil
}

func (g *Gateway) settle(ctx context.Context) (Stats, error) {
	var last Stats
	for i := 0; i < 20; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+g.Debug+"/debug/free", nil)
		if resp, err := debugClient.Do(req); err == nil {
			resp.Body.Close()
		}
		s, err := g.Stats(ctx)
		if err != nil {
			return s, err
		}
		if i > 0 && s.Goroutines == last.Goroutines && s.FDs == last.FDs {
			return s, nil
		}
		last = s
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return s, ctx.Err()
		}
	}
	return last, nil
}

// parseCPUTime reads ps time: [[dd-]hh:]mm:ss[.xx].
func parseCPUTime(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		total = total*60 + v
	}
	return days*86400 + total
}
