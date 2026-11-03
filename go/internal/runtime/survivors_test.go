package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pidAlive(pid int) bool {
	_, _, ok := processIdentity(pid)
	return ok
}

// orphanLike starts a group leader on its own, as a dead gateway's backend looks to
// its successor: not a Process of this gateway, no guard, reaped when it ends.
func orphanLike(t *testing.T, script string) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

func writeRecord(t *testing.T, root string, rec backendRecord) string {
	t.Helper()
	dir := backendRecordsDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rec)
	path := filepath.Join(dir, "llm-"+strconv.Itoa(rec.PID)+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !pidAlive(pid) {
			return true
		}
	}
	return false
}

func TestSweepStopsARecordedSurvivor(t *testing.T) {
	root := t.TempDir()
	pid := orphanLike(t, "exec sleep 60")
	time.Sleep(50 * time.Millisecond) // past the exec, so the identity is sleep's
	start, exe, ok := processIdentity(pid)
	if !ok {
		t.Fatal("no identity")
	}
	path := writeRecord(t, root, backendRecord{Runtime: "llm", PID: pid, StartTime: start, Exe: exe})
	if n := SweepBackends(root, quietLog()); n != 1 {
		t.Errorf("stopped %d", n)
	}
	if !waitGone(pid, 2*time.Second) {
		t.Error("the survivor is still running")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the record is still there")
	}
}

// A pid reused by something else is never touched, whichever part differs.
func TestSweepSparesAReusedPid(t *testing.T) {
	pid := orphanLike(t, "exec sleep 60")
	time.Sleep(50 * time.Millisecond)
	start, exe, ok := processIdentity(pid)
	if !ok {
		t.Fatal("no identity")
	}
	for name, rec := range map[string]backendRecord{
		"start time": {PID: pid, StartTime: "Thu Jan  1 00:00:00 1970", Exe: exe},
		"executable": {PID: pid, StartTime: start, Exe: "/somewhere/else"},
	} {
		root := t.TempDir()
		path := writeRecord(t, root, rec)
		if n := SweepBackends(root, quietLog()); n != 0 {
			t.Errorf("%s: stopped %d", name, n)
		}
		if !pidAlive(pid) {
			t.Fatalf("%s: killed a process that is not ours", name)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: the stale record is still there", name)
		}
	}
}

// The whole group goes, including a member that outlived its leader.
func TestSweepStopsGroupWhoseLeaderIsGone(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "child")
	// The member ignores TERM, so only the group's KILL can end it.
	leader := orphanLike(t, `sh -c "trap '' TERM; exec sleep 60" & echo $! > `+out+`; exit 0`)
	var child int
	for deadline := time.Now().Add(3 * time.Second); child == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(out)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if !waitGone(leader, 3*time.Second) || child == 0 {
		t.Fatalf("setup: leader %d child %d", leader, child)
	}
	time.Sleep(50 * time.Millisecond) // the member is past its trap
	writeRecord(t, root, backendRecord{PID: leader, StartTime: "any", Exe: "any"})
	if n := SweepBackends(root, quietLog()); n != 1 {
		t.Errorf("stopped %d", n)
	}
	if !waitGone(child, 2*time.Second) {
		t.Error("the group's other member is still running")
	}
}

func TestSweepIgnoresWhatIsAlreadyGone(t *testing.T) {
	root := t.TempDir()
	pid := orphanLike(t, "exit 0")
	if !waitGone(pid, 3*time.Second) {
		t.Fatal("setup")
	}
	writeRecord(t, root, backendRecord{PID: pid, StartTime: "x", Exe: "y"})
	writeRecord(t, root, backendRecord{PID: 1, StartTime: "x", Exe: "y"})
	if n := SweepBackends(root, quietLog()); n != 0 {
		t.Errorf("stopped %d", n)
	}
	// A process whose identity cannot be read is not proof of anything: on Linux
	// that is a root process seen by a normal user.
	if os.Getuid() != 0 {
		rec := backendRecord{PID: 1, StartTime: "x", Exe: "y"}
		if sweepOne(rec) {
			t.Error("swept pid 1")
		}
	}
}

// Readiness comes from the child: another server answering on the port does not
// count while the child is alive either.
func TestWaitHealthyRejectsForeignListener(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p, err := StartProcess("child", "/bin/sleep", []string{"30"}, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(time.Second)
	err = p.WaitHealthy(context.Background(), srv.URL, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("want a refusal naming pid %d: %v", os.Getpid(), err)
	}
	p.ForeignListenerOK = true // a custom launch may serve from outside its group
	if err := p.WaitHealthy(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Errorf("custom launch: %v", err)
	}
}

// A backend records itself for its whole life, and stops cleanly with its guard.
func TestBackendRecordFollowsTheProcess(t *testing.T) {
	root := t.TempDir()
	p, err := StartProcess("child", "/bin/sleep", []string{"30"}, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	recordBackend(root, "llm", 1234, p, quietLog())
	path := filepath.Join(backendRecordsDir(root), "llm-"+strconv.Itoa(p.PID())+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	began := time.Now()
	p.Stop(5 * time.Second)
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("a stop took %v", d)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err != nil {
			break
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the record outlived the process")
	}
	if groupAlive(p.PID()) {
		time.Sleep(200 * time.Millisecond)
		if groupAlive(p.PID()) {
			t.Error("the guard outlived the stop")
		}
	}
}

// The gateway dies with no chance to clean up: its backend still goes, with no
// successor to sweep it. The "gateway" here is this test binary, run again.
func TestBackendDiesWithItsGateway(t *testing.T) {
	if os.Getenv("LB_TEST_GUARD_PARENT") == "1" {
		p, err := StartProcess("child", "/bin/sleep", []string{"120"}, nil, quietLog())
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout.WriteString("pid=" + strconv.Itoa(p.PID()) + "\n")
		time.Sleep(time.Minute)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackendDiesWithItsGateway$", "-test.v")
	cmd.Env = append(os.Environ(), "LB_TEST_GUARD_PARENT=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := 0
	sc := bufio.NewScanner(out)
	for pid == 0 && sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "pid="); ok {
			pid, _ = strconv.Atoi(v)
		}
	}
	if pid == 0 {
		t.Fatal("the helper reported no backend")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if !pidAlive(pid) {
		t.Fatal("the backend never ran")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if !waitGone(pid, 3*time.Second) {
		t.Errorf("backend %d is still running after its gateway was killed", pid)
	}
}

// Media runtimes go through the same record as the LLM, so a sweep reaches them.
// Fake backends only: no real whisper, speech, image or video server has been run
// through this.
func TestServiceAndLLMRecordTheirBackends(t *testing.T) {
	for name, tc := range map[string]func(t *testing.T) (root string, pid func() int, stop func()){
		"stt": func(t *testing.T) (string, func() int, func()) {
			s, _ := newTestService(t)
			s.d.RootDir = t.TempDir()
			l, err := s.Acquire(context.Background(), "a")
			if err != nil {
				t.Fatal(err)
			}
			l.Release()
			return s.d.RootDir, func() int {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.cur.proc.PID()
			}, s.Close
		},
		"llm": func(t *testing.T) (string, func() int, func()) {
			m := newTestLLM(t, &bigHost{avail: 50_000_000_000})
			m.d.Root = t.TempDir()
			l, err := m.Acquire(context.Background(), modelA)
			if err != nil {
				t.Fatal(err)
			}
			l.Release()
			return m.d.Root, func() int {
				m.mu.Lock()
				defer m.mu.Unlock()
				return m.cur.proc.PID()
			}, m.Close
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, pid, stop := tc(t)
			defer stop()
			p := pid()
			// The successor's view: this process is a survivor the sweep may stop.
			if n := SweepBackends(root, quietLog()); n != 1 {
				t.Fatalf("swept %d", n)
			}
			if !waitGone(p, 3*time.Second) {
				t.Error("the backend is still running")
			}
		})
	}
}
