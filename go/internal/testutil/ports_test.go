package testutil

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestPortRange(t *testing.T) {
	if l, h, err := PortRange(""); err != nil || l != 25000 || h != 25499 {
		t.Errorf("default: %d %d %v", l, h, err)
	}
	if l, h, err := PortRange("26000-26499"); err != nil || l != 26000 || h != 26499 {
		t.Errorf("set: %d %d %v", l, h, err)
	}
	for _, bad := range []string{"26000", "x-y", "26499-26000", "80-90", "60000-70000", "-"} {
		if _, _, err := PortRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFreePortStaysInBlockAndDoesNotRepeat(t *testing.T) {
	t.Setenv(PortsEnv, "25600-25620") // spans the excluded 25611-25613
	seen := map[int]bool{}
	for range 10 {
		p := FreePort()
		if !PortAllowed(p) || seen[p] {
			t.Errorf("port %d (seen %v)", p, seen)
		}
		seen[p] = true
	}
	t.Setenv(PortsEnv, "24173-24173")
	if PortAllowed(24173) {
		t.Error("24173 is never allowed")
	}
	defer func() {
		if recover() == nil {
			t.Error("a block with no usable port should panic")
		}
	}()
	FreePort()
}

// roomInBlock is the start of the block, if it has room for these tests to stay inside it.
func roomInBlock(t *testing.T) int {
	t.Helper()
	low, high, err := PortRange(os.Getenv(PortsEnv))
	if err != nil {
		t.Fatal(err)
	}
	if high-low+1 < 232 {
		t.Skipf("the block %d-%d is too small for these tests", low, high)
	}
	return low
}

// holdPort is a separate process that takes a port and keeps its share until told to stop, as a
// test binary does for its whole run.
type portHolder struct {
	cmd   *exec.Cmd
	ports []int
}

func holdPort(t *testing.T, block string) *portHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsAPort$")
	cmd.Env = append(os.Environ(), "LOCALBASE_PORT_HELPER=1", PortsEnv+"="+block)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &portHolder{cmd: cmd}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the helper said nothing: %v", err)
	}
	for _, field := range strings.Fields(line) {
		p, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("the helper said %q", line)
		}
		h.ports = append(h.ports, p)
	}
	if len(h.ports) == 0 {
		t.Fatal("the helper handed out no ports")
	}
	return h
}

// Not a test: what holdPort runs.
func TestHelperHoldsAPort(t *testing.T) {
	if os.Getenv("LOCALBASE_PORT_HELPER") == "" {
		t.Skip("run by holdPort")
	}
	// More than a part holds, so that it walks the whole of what it was given.
	line := []string{}
	for range 64 {
		line = append(line, strconv.Itoa(FreePort()))
	}
	fmt.Println(strings.Join(line, " "))
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// The bug: two test binaries sharing a block could be handed the same free port, and whichever
// bound second lost its backend. A part of the block each makes that impossible.
func TestProcessesGetSeparatePartsOfTheBlock(t *testing.T) {
	low := roomInBlock(t)
	block := fmt.Sprintf("%d-%d", low, low+95)
	owner := map[int]int{}
	for process := range 6 {
		for _, p := range holdPort(t, block).ports {
			if p < low || p > low+95 {
				t.Errorf("process %d was handed port %d, outside %s", process, p, block)
			}
			if other, taken := owner[p]; taken && other != process {
				t.Errorf("processes %d and %d were both handed port %d", other, process, p)
			}
			owner[p] = process
		}
	}
	// How many ports were handed out isn't checked. Another test binary can be using some of these,
	// as `go test ./...` runs this package beside the others, and those are rightly skipped.
}

// With more processes than parts, the rest still work, as they did before there were parts.
func TestMoreProcessesThanPartsStillGetPorts(t *testing.T) {
	low := roomInBlock(t)
	block := fmt.Sprintf("%d-%d", low+100, low+131) // 2 parts of 16
	for range 3 {
		holdPort(t, block)
	}
}

// A process that dies gives its part back, without anything having to clean up.
func TestAKilledProcessGivesItsPartBack(t *testing.T) {
	low := roomInBlock(t)
	block := fmt.Sprintf("%d-%d", low+140, low+171) // 2 parts of 16
	part := func(p int) int { return (p - (low + 140)) / 16 }
	first, second := holdPort(t, block), holdPort(t, block)
	if part(first.ports[0]) == part(second.ports[0]) {
		t.Fatalf("two processes share a part: %d %d", first.ports[0], second.ports[0])
	}
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.cmd.Wait()
	if third := holdPort(t, block); part(third.ports[0]) != part(first.ports[0]) {
		t.Errorf("the killed process's part %d wasn't taken: got port %d, part %d", part(first.ports[0]), third.ports[0], part(third.ports[0]))
	}
}

// A block too small to divide works as it always did. This asks the decision itself, so it needs no
// port to be free.
func TestASmallBlockIsNotDivided(t *testing.T) {
	low := roomInBlock(t)
	blockHigh := low + 191
	got := takePart("small", low+180, blockHigh)
	if got.low != low+180 || got.high != blockHigh || got.lock != nil {
		t.Errorf("a block of 12 was divided: %+v", got)
	}
}

// A process using every port of its part still gets one, from the rest of the block.
func TestAFullPartFallsBackToTheRestOfTheBlock(t *testing.T) {
	low := roomInBlock(t)
	blockLow := low + 200
	t.Setenv(PortsEnv, fmt.Sprintf("%d-%d", blockLow, blockLow+31)) // 2 parts of 16
	first := FreePort()
	partLow := blockLow + (first-blockLow)/16*16
	for p := partLow; p < partLow+16; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			// Another test binary can be using one of these, and then this can't fill the part.
			t.Skipf("can't hold %d to fill the part: %v", p, err)
		}
		defer ln.Close()
	}
	if got := FreePort(); got < blockLow || got > blockLow+31 || (got >= partLow && got < partLow+16) {
		t.Errorf("with part %d-%d full, got %d", partLow, partLow+15, got)
	}
}
