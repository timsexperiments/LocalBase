package store

// Scratch probe, never for a real branch: a second process closing beside a live store, in each
// shape, then the first process killed or closed cleanly. Reliability, at the operator's request.

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const zzWrites = 20

// The four roles run in child processes, so that the locks are really another process's.
func zzRole(t *testing.T) bool {
	path := os.Getenv("ZZ_DB")
	switch os.Getenv("ZZ_ROLE") {
	case "":
		return false
	case "A": // a live store: a read and a write, then writes on request, until killed or told to close
		st, err := Open(path)
		if err != nil {
			fmt.Println("RESULT open", err)
			os.Exit(1)
		}
		ctx := context.Background()
		if _, _, err := st.Setting(ctx, "zz-a0"); err != nil {
			fmt.Println("RESULT read", err)
		}
		if err := st.SetSetting(ctx, "zz-a0", "1"); err != nil {
			fmt.Println("RESULT write", err)
		}
		fmt.Println("READY")
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			switch line := in.Text(); {
			case strings.HasPrefix(line, "write "):
				n, _ := strconv.Atoi(strings.TrimPrefix(line, "write "))
				for i := 1; i <= n; i++ {
					if err := st.SetSetting(ctx, fmt.Sprintf("zz-a%d", i), "1"); err != nil {
						fmt.Println("RESULT write", i, err)
					} else {
						fmt.Println("ACKED", i)
					}
				}
				fmt.Println("WROTE")
			case line == "close":
				fmt.Println("CLOSED", st.Close())
				os.Exit(0)
			}
		}
		os.Exit(0)
	case "B": // a second process, opening and closing in one shape
		shape := os.Getenv("ZZ_SHAPE")
		if shape == "store" {
			st, err := Open(path)
			if err == nil {
				err = st.Close()
			}
			fmt.Println("RESULT", err)
			os.Exit(0)
		}
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
		if err != nil {
			fmt.Println("RESULT open", err)
			os.Exit(0)
		}
		db.SetMaxOpenConns(1)
		var out []string
		run := func(q string) {
			var err error
			if strings.HasPrefix(q, "SELECT") {
				var n int
				err = db.QueryRow(q).Scan(&n)
			} else {
				_, err = db.Exec(q)
			}
			out = append(out, fmt.Sprintf("%s: %v", strings.Fields(q)[0], err))
		}
		switch shape {
		case "bare-read":
			run("SELECT count(*) FROM sqlite_master")
		case "excl-pragma":
			run("PRAGMA locking_mode=EXCLUSIVE")
		case "excl-select":
			run("PRAGMA locking_mode=EXCLUSIVE")
			run("SELECT count(*) FROM sqlite_master")
		case "excl-begin":
			run("PRAGMA locking_mode=EXCLUSIVE")
			run("BEGIN EXCLUSIVE")
			run("ROLLBACK")
		}
		fmt.Println("RESULT", strings.Join(out, "; "), "close:", db.Close())
		os.Exit(0)
	case "L": // a third process asks who holds SQLite's shared range, and costs nobody a lock
		f, err := os.Open(path)
		if err != nil {
			fmt.Println("RESULT", err)
			os.Exit(0)
		}
		lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: 0x40000002, Len: 510}
		err = syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk)
		f.Close()
		if err != nil {
			fmt.Println("RESULT", err)
		} else if lk.Type == syscall.F_UNLCK {
			fmt.Println("RESULT free")
		} else {
			fmt.Println("RESULT held by", lk.Pid)
		}
		os.Exit(0)
	}
	return true
}

type zzChild struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

func zzStart(t *testing.T, env ...string) *zzChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZZShapes$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &zzChild{cmd: cmd, in: in, out: bufio.NewScanner(out)}
}

// until reads lines until one starts with prefix, and returns it and every RESULT on the way.
func (c *zzChild) until(prefix string) (string, []string) {
	var notes []string
	for c.out.Scan() {
		line := c.out.Text()
		if strings.HasPrefix(line, "RESULT") {
			notes = append(notes, line)
		}
		if strings.HasPrefix(line, prefix) {
			return line, notes
		}
	}
	return "", notes
}

func zzOnce(t *testing.T, env ...string) string {
	t.Helper()
	c := zzStart(t, env...)
	line, _ := c.until("RESULT")
	_ = c.cmd.Wait()
	return strings.TrimPrefix(line, "RESULT ")
}

func zzWal(path string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(path+"-wal", &st); err != nil {
		return "gone"
	}
	return fmt.Sprintf("inode %d, %d bytes", st.Ino, st.Size)
}

func TestZZShapes(t *testing.T) {
	zzRole(t) // a child does its part and exits here; the parent carries on
	shapes := []string{"store", "bare-read", "excl-pragma", "excl-select", "excl-begin"}
	var rows []string
	for _, shape := range shapes {
		for _, end := range []string{"killed", "closed"} {
			for run := 1; run <= 3; run++ {
				path := filepath.Join(t.TempDir(), "local-base.db")
				env := "ZZ_DB=" + path
				a := zzStart(t, "ZZ_ROLE=A", env)
				if line, notes := a.until("READY"); line == "" {
					t.Fatalf("A never got ready: %v", notes)
				}
				lockBefore := zzOnce(t, "ZZ_ROLE=L", env)
				walBefore := zzWal(path)
				b := zzOnce(t, "ZZ_ROLE=B", "ZZ_SHAPE="+shape, env)
				walAfterB := zzWal(path)
				fmt.Fprintf(a.in, "write %d\n", zzWrites)
				_, failed := a.until("WROTE")
				acked := zzWrites - len(failed) // each write prints ACKED, or a RESULT line if it failed
				walAfterWrites := zzWal(path)
				if end == "killed" {
					_ = a.cmd.Process.Signal(syscall.SIGKILL)
				} else {
					fmt.Fprintln(a.in, "close")
				}
				_ = a.cmd.Wait()
				time.Sleep(50 * time.Millisecond)
				st, err := Open(path)
				found, check := -1, ""
				if err == nil {
					found = 0
					for i := 1; i <= zzWrites; i++ {
						if _, ok, err := st.Setting(context.Background(), fmt.Sprintf("zz-a%d", i)); err == nil && ok {
							found++
						}
					}
					_ = st.DB.QueryRow("PRAGMA integrity_check").Scan(&check)
					_ = st.Close()
				} else {
					check = err.Error()
				}
				rows = append(rows, fmt.Sprintf("%s/%s/%d | lock seen by a third process: %s | -wal before B: %s | B: %s | -wal after B: %s | A's writes acked %d, -wal then: %s | found after %s: %d of %d | integrity: %s",
					runtime.GOOS, shape, run, lockBefore, walBefore, b, walAfterB, acked, walAfterWrites, end, found, zzWrites, check))
			}
		}
	}
	// Failing is how the table reaches a CI log that only prints what fails.
	t.Errorf("RESULTS (%s/%s)\n%s", runtime.GOOS, runtime.GOARCH, strings.Join(rows, "\n"))
}
