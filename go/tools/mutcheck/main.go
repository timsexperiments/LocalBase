// Command mutcheck breaks code on purpose and says whether the tests noticed.
//
//	go run ./tools/mutcheck mutations.json
//
// mutations.json is a list of {"name","file","from","to","pkg","run"}. From appears exactly
// once in file, and each mutation is applied alone. Run it from the module's root, on a tree
// you don't mind it editing: each file is put back afterwards.
//
// Each mutation has one of these results:
//
//	killed           it compiled, and the tests failed: the tests notice this
//	survived         it compiled, and the tests passed: they don't
//	did not compile  it proves nothing, so it never counts as either
//	no match         from isn't in the file exactly once, so nothing ran
//	stopped          the run went over its time limit or the disk floor: it proves nothing
//
// The tests must pass unmutated first, or a failure would say nothing about the mutation.
// The exit status is 0 only when every mutation was killed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Mutation is one deliberate break.
type Mutation struct {
	Name string `json:"name"`
	File string `json:"file"`
	From string `json:"from"`
	To   string `json:"to"`
	Pkg  string `json:"pkg"`
	Run  string `json:"run"`
}

// The results.
const (
	Killed        = "killed"
	Survived      = "survived"
	DidNotCompile = "did not compile"
	NoMatch       = "no match"
	Stopped       = "stopped"
)

// Result is what happened to one mutation.
type Result struct {
	Name, Outcome string
	Detail        string
}

type limits struct {
	timeout   time.Duration
	minFreeGB uint64
	dir       string
	freeGB    func(string) (uint64, error)
}

func main() {
	timeout := flag.Duration("timeout", 10*time.Minute, "longest one test run may take")
	floor := flag.Uint64("min-free-gb", 90, "stop a run when this disk has less free")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mutcheck [-timeout d] [-min-free-gb n] mutations.json")
		os.Exit(2)
	}
	raw, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var muts []Mutation
	if err := json.Unmarshal(raw, &muts); err != nil {
		fmt.Fprintln(os.Stderr, flag.Arg(0)+":", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	results, err := Check(ctx, muts, limits{timeout: *timeout, minFreeGB: *floor, dir: dir, freeGB: freeGB}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, r := range results {
		if r.Outcome != Killed {
			os.Exit(1)
		}
	}
}

// Check runs the mutations, writing each result as it comes.
func Check(ctx context.Context, muts []Mutation, l limits, out io.Writer) ([]Result, error) {
	baseline := map[string]bool{}
	for _, m := range muts {
		key := m.Pkg + "\x00" + m.Run
		if baseline[key] {
			continue
		}
		baseline[key] = true
		if failed, stopped, output := runTests(ctx, m.Pkg, m.Run, l); failed || stopped != "" {
			return nil, fmt.Errorf("the tests have to pass before any mutation: %s -run %q: %s\n%s", m.Pkg, m.Run, stopped, tail(output))
		}
	}
	var results []Result
	for _, m := range muts {
		r, err := checkOne(ctx, m, l)
		if err != nil {
			return results, err
		}
		fmt.Fprintf(out, "%-16s %s", r.Outcome, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(out, ": %s", r.Detail)
		}
		fmt.Fprintln(out)
		results = append(results, r)
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}

func checkOne(ctx context.Context, m Mutation, l limits) (Result, error) {
	r := Result{Name: m.Name}
	original, err := os.ReadFile(m.File)
	if err != nil {
		return r, err
	}
	if n := bytes.Count(original, []byte(m.From)); n != 1 || m.From == "" {
		r.Outcome, r.Detail = NoMatch, fmt.Sprintf("found %d times in %s", n, m.File)
		return r, nil
	}
	info, err := os.Stat(m.File)
	if err != nil {
		return r, err
	}
	mode := info.Mode().Perm()
	if err := os.WriteFile(m.File, bytes.Replace(original, []byte(m.From), []byte(m.To), 1), mode); err != nil {
		return r, err
	}
	defer func() {
		if werr := os.WriteFile(m.File, original, mode); werr != nil {
			fmt.Fprintf(os.Stderr, "mutcheck: could not put back %s: %v\n", m.File, werr)
		}
	}()
	// Everything the tests need, the tests included, before anything runs: a test that
	// doesn't build fails `go test` just as a noticed mutation does.
	if output, err := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "^$", m.Pkg).CombinedOutput(); err != nil {
		r.Outcome, r.Detail = DidNotCompile, firstLines(string(output), 3)
		return r, nil
	}
	failed, stopped, output := runTests(ctx, m.Pkg, m.Run, l)
	switch {
	case stopped != "":
		r.Outcome, r.Detail = Stopped, stopped
	case failed:
		r.Outcome, r.Detail = Killed, firstFailure(output)
	default:
		r.Outcome = Survived
	}
	return r, nil
}

// runTests runs tests in their own process group, ending the group at the time limit or the
// disk floor: a broken limit is what some mutations are for, and it can fill a disk.
func runTests(ctx context.Context, pkg, run string, l limits) (failed bool, stopped, output string) {
	if free, err := l.freeGB(l.dir); err != nil {
		return false, "could not read free disk: " + err.Error(), ""
	} else if free < l.minFreeGB {
		return false, fmt.Sprintf("%d GB free, under the %d GB floor: refused to start", free, l.minFreeGB), ""
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", run, pkg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return false, err.Error(), ""
	}
	var waitErr error
	done := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	kill := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	deadline := time.After(l.timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return waitErr != nil, "", buf.String()
		case <-ctx.Done():
			kill()
			return false, "interrupted", buf.String()
		case <-deadline:
			kill()
			return false, "over the " + l.timeout.String() + " time limit", buf.String()
		case <-tick.C:
			if free, err := l.freeGB(l.dir); err == nil && free < l.minFreeGB {
				kill()
				return false, fmt.Sprintf("%d GB free, under the %d GB floor: ended the run", free, l.minFreeGB), buf.String()
			}
		}
	}
}

func freeGB(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize) / (1 << 30), nil
}

func firstFailure(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "--- FAIL") {
			return strings.TrimSpace(line)
		}
	}
	return firstLines(output, 1)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}
