package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Backends run in their own process groups, so they outlive a gateway that dies
// without stopping them. Two things deal with that. A guard process in the group
// ends it when the gateway goes away (see startGuard), and each backend is recorded
// under the root, so the next `serve` can stop any that survived (DESIGN §29).

// backendRecord says what was started, in enough detail to tell it from a later
// process that was given the same pid.
type backendRecord struct {
	Runtime   string `json:"runtime"`
	PID       int    `json:"pid"`
	StartTime string `json:"startTime"`
	Exe       string `json:"exe"`
	Port      int    `json:"port"`
}

func backendRecordsDir(root string) string { return filepath.Join(root, "runtime", "backends") }

// processIdentity reads a process's start time and executable. ok is false when
// the process does not exist or cannot be read.
func processIdentity(pid int) (start, exe string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", "", false
	}
	start = strings.TrimSpace(string(out))
	if goruntime.GOOS == "linux" {
		exe, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			return "", "", false
		}
	} else {
		out, err = exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return "", "", false
		}
		exe = strings.TrimSpace(string(out))
	}
	return start, exe, start != "" && exe != ""
}

// recordBackend writes the record, and removes it when the process exits. A root
// of "" records nothing. A backend that cannot be recorded still runs: its guard
// still ends it with the gateway, but a later sweep cannot find it.
func recordBackend(root, runtimeName string, port int, p *Process, log *slog.Logger) {
	if root == "" {
		return
	}
	start, exe, ok := processIdentity(p.PID())
	if !ok {
		log.Warn("Could not read a backend's identity, so a later start cannot clean it up.", "event", "runtime.record-failed",
			"runtime", runtimeName, "pid", p.PID())
		return
	}
	b, err := json.Marshal(backendRecord{Runtime: runtimeName, PID: p.PID(), StartTime: start, Exe: exe, Port: port})
	if err == nil {
		dir := backendRecordsDir(root)
		if err = os.MkdirAll(dir, 0o700); err == nil {
			path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", runtimeName, p.PID()))
			if err = obs.WritePrivateAtomic(path, b); err == nil {
				go func() {
					<-p.Done()
					_ = os.Remove(path)
				}()
				return
			}
		}
	}
	log.Warn("Could not record a backend, so a later start cannot clean it up.", "event", "runtime.record-failed",
		"runtime", runtimeName, "pid", p.PID(), "error", err.Error())
}

const sweepGrace = 5 * time.Second

// SweepBackends stops backends a previous gateway on this root left running. Call
// it holding the root's lock, before any backend starts. It stops a process only
// when its pid, start time and executable all match the record, so a process that
// was later given the same pid is never touched. It returns how many it stopped.
func SweepBackends(root string, log *slog.Logger) int {
	entries, err := os.ReadDir(backendRecordsDir(root))
	if err != nil {
		return 0
	}
	stopped := 0
	for _, e := range entries {
		path := filepath.Join(backendRecordsDir(root), e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec backendRecord
		if json.Unmarshal(b, &rec) != nil || rec.PID <= 1 {
			_ = os.Remove(path)
			continue
		}
		if sweepOne(rec) {
			stopped++
			log.Warn("Stopped a backend left running by an earlier gateway.", "event", "runtime.survivor-stopped",
				"runtime", rec.Runtime, "pid", rec.PID, "port", rec.Port)
		}
		_ = os.Remove(path)
	}
	return stopped
}

func sweepOne(rec backendRecord) bool {
	start, exe, alive := processIdentity(rec.PID)
	if alive && (start != rec.StartTime || exe != rec.Exe) {
		return false // the pid now belongs to something else, and so does its group
	}
	// It exists but cannot be read, such as another user's process on Linux: no
	// proof it is ours, so it is left alone.
	if !alive && pidExists(rec.PID) {
		return false
	}
	// No process has this pid, but its group may still have members. That group is
	// still ours: a pid is not reused while a group with that id exists.
	if !alive && !groupAlive(rec.PID) {
		return false
	}
	_ = syscall.Kill(-rec.PID, syscall.SIGTERM)
	for deadline := time.Now().Add(sweepGrace); alive && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		_, _, alive = processIdentity(rec.PID)
	}
	// The leader is gone or out of time. Whatever is left of the group ends now.
	_ = syscall.Kill(-rec.PID, syscall.SIGKILL)
	for deadline := time.Now().Add(sweepGrace); groupAlive(rec.PID) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func pidExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// guardScript waits for its stdin to close, which happens only when the gateway is
// gone, then ends its own process group. It ignores the TERM a normal stop sends
// the group; the gateway kills it directly once the backend has exited.
const guardScript = `trap '' TERM INT HUP
read -r _
kill -TERM 0
i=0
while [ $i -lt 5 ]; do
  sleep 1
  kill -0 "$1" 2>/dev/null || break
  i=$((i+1))
done
kill -KILL 0`

// startGuard joins the backend's process group with a small shell holding the read
// end of a pipe. The gateway holds the write end, and the kernel closes it however
// the gateway dies. This works the same on macOS and Linux, and unlike Pdeathsig it
// cannot fire because one OS thread of the gateway ended. A guard that cannot start
// leaves the sweep as the only cleanup.
func (p *Process) startGuard(log *slog.Logger) {
	r, w, err := os.Pipe()
	if err != nil {
		log.Warn("Could not start a backend's guard.", "event", "runtime.guard-failed", "error", err.Error())
		return
	}
	defer r.Close()
	pid := p.PID()
	guard := exec.Command("/bin/sh", "-c", guardScript, "guard", strconv.Itoa(pid))
	guard.Stdin = r
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pid}
	if err := guard.Start(); err != nil {
		w.Close()
		log.Warn("Could not start a backend's guard.", "event", "runtime.guard-failed", "error", err.Error())
		return
	}
	done := make(chan struct{})
	go func() {
		_ = guard.Wait()
		close(done)
	}()
	p.guardPID = guard.Process.Pid
	p.endGuard = func() {
		_ = guard.Process.Kill()
		_ = w.Close()
		<-done
	}
}

// listenerPIDs asks who is listening on a port. ok is false when that cannot be
// found out: no lsof, or nothing it is allowed to see.
func listenerPIDs(port int) (pids []int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, lsofPath(), "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil, false
	}
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids, len(pids) > 0
}

// lsofPath finds lsof where a service's short PATH would miss it.
func lsofPath() string {
	if p, err := exec.LookPath("lsof"); err == nil {
		return p
	}
	return "/usr/sbin/lsof"
}

// foreignListener returns a pid listening on port from outside this process's
// group, or 0 when there is none or it cannot be told.
func (p *Process) foreignListener(port int) int {
	pids, ok := listenerPIDs(port)
	if !ok {
		return 0
	}
	for _, pid := range pids {
		if pgid, err := syscall.Getpgid(pid); err != nil || pgid != p.PID() {
			return pid
		}
	}
	return 0
}
