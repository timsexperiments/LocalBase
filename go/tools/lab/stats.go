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

func rssOf(pid int) int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	return kb * 1024
}

// Proc is one row of the process table.
type Proc struct {
	PID, PPID int
	State     string
	Command   string
}

// procs lists every process with a command under dir.
func procsUnder(dir string) ([]Proc, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,state=,command=").Output()
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if !strings.HasPrefix(f[3], dir+string(filepath.Separator)) {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, State: f[2], Command: filepath.Base(f[3])})
	}
	return procs, nil
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
ps -A -o pid=,command= | awk -v p=%q 'index($2, p) == 1 { print $1 }' | while read -r stray; do kill -KILL "$stray" 2>/dev/null; done`,
		os.Getpid(), pid, os.Getpid(), pid, pid, bin+string(filepath.Separator))
	w := exec.Command("sh", "-c", script)
	w.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if w.Start() == nil {
		go w.Wait()
	}
}

// Settled samples the gateway once it has freed what it can and its goroutine
// count has stopped moving, so a figure is not a GC or connection-close artifact.
func (g *Gateway) Settled(ctx context.Context) (Stats, error) {
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

// Zombies counts pid's children that have exited and not been waited for.
func Zombies(pid int) int {
	out, err := exec.Command("ps", "-A", "-o", "ppid=,state=").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == strconv.Itoa(pid) && strings.HasPrefix(f[1], "Z") {
			n++
		}
	}
	return n
}

// CPUSeconds is a process's total CPU time so far, from ps.
func CPUSeconds(pid int) float64 {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
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
