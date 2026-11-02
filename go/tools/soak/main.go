// Command soak runs many concurrent clients against a real gateway over fake
// backends and a fake memory host, through repeated cycles of ample memory,
// bursts, unloads, tight memory, an unmeasurable host, pressure and recovery,
// and ends with a SIGTERM under load and a restart. It writes a one-page result
// table and exits non-zero on any violation.
//
//	make soak            30 minutes
//	make soak-smoke      about 2 minutes
//	make soak-race       the smoke against a race-detector gateway
//
// Use tools/soak.sh rather than running it directly: that gives it its own
// session and a hard stop, so nothing outlives a run on a shared machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

type options struct {
	duration time.Duration
	race     bool
	out      string
	keep     bool
	seed     uint64
}

func main() {
	var o options
	flag.DurationVar(&o.duration, "duration", 30*time.Minute, "how long to run; whole cycles, at least one")
	smoke := flag.Bool("smoke", false, "one cycle, about 2 minutes")
	flag.BoolVar(&o.race, "race", false, "build the gateway with the race detector; any report fails the run")
	flag.StringVar(&o.out, "out", "", "result directory (default: a new directory under $TMPDIR/localbase-soak)")
	flag.BoolVar(&o.keep, "keep", false, "keep the scratch root and logs after a passing run")
	flag.Uint64Var(&o.seed, "seed", uint64(time.Now().UnixNano()), "traffic seed")
	flag.Parse()
	if *smoke {
		o.duration = 0
	}
	// Below the machine's other work, and children inherit it.
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	os.Exit(run(o))
}

func run(o options) int {
	if o.out == "" {
		o.out = filepath.Join(os.TempDir(), "localbase-soak", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(o.out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		return 2
	}
	cycles := max(1, int(o.duration/cycleLength()))
	// Far beyond the planned run: whatever hangs, the machine gets its CPU back.
	hard := time.Duration(cycles)*cycleLength() + 5*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), hard)
	defer cancel()
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()

	w, err := lab.NewWorkspace("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		return 2
	}
	w.Keep = o.keep
	defer w.Close()
	time.AfterFunc(hard+30*time.Second, func() {
		fmt.Fprintln(os.Stderr, "soak: hard deadline passed; stopping everything")
		w.KillStray()
		os.Exit(3)
	})
	fmt.Fprintf(os.Stderr, "soak: building (race=%v), %d cycle(s) of %v, results in %s\n", o.race, cycles, cycleLength(), o.out)
	if err := w.Build(ctx, o.race); err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		return 2
	}
	s, err := newSoak(ctx, w, o, cycles)
	if err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		return 2
	}
	s.execute(ctx)
	rep := s.report()
	path, err := rep.write(o.out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
	}
	fmt.Println(rep.markdown())
	fmt.Fprintf(os.Stderr, "soak: result %s\n", path)
	if rep.failed() {
		// A failing run keeps its evidence: the logs and the scratch root.
		w.Keep = true
		fmt.Fprintf(os.Stderr, "soak: FAILED; scratch kept at %s\n", w.Dir)
		return 1
	}
	if !o.keep {
		fmt.Fprintln(os.Stderr, "soak: "+strings.ToLower(rep.Verdict))
	}
	return 0
}
