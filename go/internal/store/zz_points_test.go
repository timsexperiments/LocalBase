package store

// Scratch probe, never for a real branch: where a live store's shared lock is held and where it is
// not, seen from a third process. Reliability, at the operator's and Go's request, to explain why
// the unfixed head keeps its lock on Linux and loses it on macOS.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A store that does one step at a time on request and says how its pool stands after each.
func zzPointsRole() {
	if os.Getenv("ZZ_ROLE") != "P" {
		return
	}
	path := os.Getenv("ZZ_DB")
	st, err := Open(path)
	if err != nil {
		fmt.Println("STEP open failed:", err)
		os.Exit(1)
	}
	pool := func() string {
		s := st.DB.Stats()
		return fmt.Sprintf("open=%d inuse=%d idle=%d closed=%d", s.OpenConnections, s.InUse, s.Idle,
			s.MaxIdleClosed+s.MaxIdleTimeClosed+s.MaxLifetimeClosed)
	}
	owner := "no"
	if _, err := os.Stat(path + "-wal-owner"); err == nil {
		owner = "yes"
	}
	fmt.Printf("STEP opened [%s, owner file %s]\n", pool(), owner)
	ctx := context.Background()
	in := bufio.NewScanner(os.Stdin)
	n := 0
	for in.Scan() {
		switch in.Text() {
		case "write":
			n++
			err := st.SetSetting(ctx, fmt.Sprintf("zz-p%d", n), "1")
			fmt.Printf("STEP wrote %v [%s]\n", err, pool())
		case "bare":
			f, err := os.Open(path) // the plain cancel: any close of the file drops the process's locks
			if err == nil {
				err = f.Close()
			}
			fmt.Printf("STEP bare open and close %v [%s]\n", err, pool())
		case "close":
			fmt.Println("STEP closed", st.Close())
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func zzPointsStart(t *testing.T, env ...string) *zzChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZZPoints$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &zzChild{cmd: cmd, in: in, out: bufio.NewScanner(out)}
}

func zzPointsLock(t *testing.T, env string) string {
	t.Helper()
	c := zzPointsStart(t, "ZZ_ROLE=L", env)
	line, _ := c.until("RESULT")
	_ = c.cmd.Wait()
	return strings.TrimPrefix(line, "RESULT ")
}

func TestZZPoints(t *testing.T) {
	zzPointsRole() // the store's child does its part and exits here
	zzRole(t)      // the lock checker's child does its part and exits here; the parent carries on
	sequences := map[string][]string{
		"points":  {"write", "write"},
		"control": {"bare", "write"},
	}
	var rows []string
	for _, name := range []string{"points", "control"} {
		for run := 1; run <= 3; run++ {
			path := filepath.Join(t.TempDir(), "local-base.db")
			env := "ZZ_DB=" + path
			a := zzPointsStart(t, "ZZ_ROLE=P", env)
			step, _ := a.until("STEP")
			parts := []string{fmt.Sprintf("%s: lock %s", strings.TrimPrefix(step, "STEP "), zzPointsLock(t, env))}
			for _, cmd := range sequences[name] {
				fmt.Fprintln(a.in, cmd)
				step, _ = a.until("STEP")
				parts = append(parts, fmt.Sprintf("%s: lock %s", strings.TrimPrefix(step, "STEP "), zzPointsLock(t, env)))
			}
			fmt.Fprintln(a.in, "close")
			_ = a.cmd.Wait()
			rows = append(rows, fmt.Sprintf("%s/%s/%d | %s", runtime.GOOS, name, run, strings.Join(parts, " | ")))
		}
	}
	// Failing is how the table reaches a CI log that only prints what fails.
	t.Errorf("POINTS (%s/%s)\n%s", runtime.GOOS, runtime.GOARCH, strings.Join(rows, "\n"))
}
