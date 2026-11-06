package lab

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// procRow is one process, however it was read.
type procRow struct {
	pid, ppid int
	state     string
	command   string // the program's path; empty for a zombie on Linux
	rss       int64
	cpu       float64 // seconds
}

// processTable reads /proc where there is one and ps elsewhere. A minimal Linux
// container has no ps, and macOS has no /proc.
func processTable() ([]procRow, error) {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return readProc("/proc")
	}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,state=,rss=,time=,command=").Output()
	if err != nil {
		return nil, err
	}
	var rows []procRow
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		rows = append(rows, procRow{pid: pid, ppid: ppid, state: f[2], rss: rss * 1024, cpu: parseCPUTime(f[4]), command: f[5]})
	}
	return rows, nil
}

// readProc reads a /proc tree. A process that exits meanwhile is simply missing.
func readProc(root string) ([]procRow, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var rows []procRow
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		row, err := parseProcStat(raw)
		if err != nil {
			continue
		}
		row.pid = pid
		if cmdline, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline")); err == nil {
			row.command, _, _ = strings.Cut(string(bytes.TrimRight(cmdline, "\x00")), "\x00")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// clockTicks is USER_HZ, 100 on every Linux Go supports.
const clockTicks = 100

// parseProcStat reads /proc/<pid>/stat. The program name is in parentheses and
// may itself hold spaces or parentheses, so fields count from the last ")".
func parseProcStat(raw []byte) (procRow, error) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return procRow{}, errors.New("no program name")
	}
	f := strings.Fields(string(raw[i+1:]))
	// f[0] is field 3 of proc(5): state, ppid ... utime 14, stime 15, rss 24.
	if len(f) < 22 {
		return procRow{}, errors.New("too few fields")
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procRow{}, err
	}
	utime, _ := strconv.ParseFloat(f[11], 64)
	stime, _ := strconv.ParseFloat(f[12], 64)
	pages, _ := strconv.ParseInt(f[21], 10, 64)
	return procRow{ppid: ppid, state: f[0], rss: pages * int64(os.Getpagesize()), cpu: (utime + stime) / clockTicks}, nil
}

func rowOf(pid int) (procRow, bool) {
	rows, err := processTable()
	if err != nil {
		return procRow{}, false
	}
	for _, r := range rows {
		if r.pid == pid {
			return r, true
		}
	}
	return procRow{}, false
}

func rssOf(pid int) int64 {
	if r, ok := rowOf(pid); ok {
		return r.rss
	}
	return -1
}

// CPUSeconds is a process's total CPU time so far.
func CPUSeconds(pid int) float64 {
	r, _ := rowOf(pid)
	return r.cpu
}

// Zombies counts pid's children that have exited and not been waited for.
func Zombies(pid int) int {
	rows, err := processTable()
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		if r.ppid == pid && strings.HasPrefix(r.state, "Z") {
			n++
		}
	}
	return n
}

// procsUnder lists every process running a program under dir.
func procsUnder(dir string) ([]Proc, error) {
	rows, err := processTable()
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, r := range rows {
		if strings.HasPrefix(r.command, dir+string(filepath.Separator)) {
			procs = append(procs, Proc{PID: r.pid, PPID: r.ppid, State: r.state, Command: filepath.Base(r.command)})
		}
	}
	return procs, nil
}
