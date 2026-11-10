package testutil

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Test gateways and backends listen on ports from one block, so concurrent work
// on this machine does not collide. PortsEnv chooses it as "low-high".
const (
	PortsEnv     = "LOCALBASE_TEST_PORTS"
	DefaultPorts = "25000-25499"
)

// PortRange parses a "low-high" block; an empty value is the default block.
func PortRange(v string) (low, high int, err error) {
	if v == "" {
		v = DefaultPorts
	}
	l, h, ok := strings.Cut(v, "-")
	low, lerr := strconv.Atoi(l)
	high, herr := strconv.Atoi(h)
	if !ok || lerr != nil || herr != nil || low < 1024 || high > 65535 || low > high {
		return 0, 0, fmt.Errorf("%s must be low-high between 1024 and 65535, got %q", PortsEnv, v)
	}
	return low, high, nil
}

// PortExcluded is true for ports other local services use, whatever the block.
func PortExcluded(p int) bool { return p == 24173 || (p >= 25611 && p <= 25613) }

// PortAllowed is true for a port tests may use under the current block.
func PortAllowed(p int) bool {
	low, high, err := PortRange(os.Getenv(PortsEnv))
	return err == nil && p >= low && p <= high && !PortExcluded(p)
}

var ports struct {
	sync.Mutex
	cursor int
	parts  map[string]part
}

// part is the piece of a block this process has for its whole run. The lock stays open so that it
// stays held, and the system lets it go however the process ends, even killed.
type part struct {
	low, high int
	lock      *os.File
}

const (
	maxParts     = 16
	minPartPorts = 16
)

// myPart narrows a block to this process's part of it. Test binaries run at once and share a block,
// and a port can be free when it is handed out and taken before it is bound: the one bound second
// then loses its backend, and its request gets a 503. Two processes can't both hold a part, so they
// can't be handed the same port. A block too small to divide, or a process that finds every part
// taken, uses the whole block as before.
func myPart(low, high int) (int, int) {
	key := fmt.Sprintf("%d-%d", low, high)
	p, ok := ports.parts[key]
	if !ok {
		p = takePart(key, low, high)
		if ports.parts == nil {
			ports.parts = map[string]part{}
		}
		ports.parts[key] = p
	}
	return p.low, p.high
}

func takePart(key string, low, high int) part {
	whole := part{low: low, high: high}
	size := high - low + 1
	count := min(maxParts, size/minPartPorts)
	if count < 2 {
		return whole
	}
	dir := partsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return whole
	}
	width, start := size/count, rand.IntN(count)
	for i := range count {
		n := (start + i) % count
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s-%d.lock", key, n)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return whole
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return part{low: low + n*width, high: low + n*width + width - 1, lock: f}
		}
		_ = f.Close() // Someone else's part, so nothing to keep.
	}
	return whole
}

// partsDir holds the locks on the parts of a block, one file a part.
func partsDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("localbase-test-ports-%d", os.Getuid()))
}

// FreeRun returns the first of n free ports in a row, in this process's part of the block, or if the
// part has no such run, in the rest of the block, as FreePort does. A run is for one gateway's
// runtimes (--runtime-port-base), which bind it well after it is handed out. A run from another
// process's part loses a port to that process's backends, and the runtime that loses can't start.
// FreePort walks on from past the run, so it doesn't hand those ports out again.
func FreeRun(n int) (int, bool) {
	blockLow, blockHigh, err := PortRange(os.Getenv(PortsEnv))
	if err != nil {
		panic(err)
	}
	ports.Lock()
	defer ports.Unlock()
	low, high := myPart(blockLow, blockHigh)
	base, ok := runIn(low, high, n)
	if !ok {
		base, ok = runIn(blockLow, blockHigh, n)
	}
	if ok {
		ports.cursor = base + n
	}
	return base, ok
}

// runIn is the first run of n free ports in low-high.
func runIn(low, high, n int) (int, bool) {
scan:
	for base := low; base+n-1 <= high; base++ {
		for p := base; p < base+n; p++ {
			if PortExcluded(p) {
				base = p
				continue scan
			}
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				base = p
				continue scan
			}
			_ = ln.Close() // A probe: the caller binds it.
		}
		return base, true
	}
	return 0, false
}

// FreePort returns a free port in this process's part of the block, or if every port there is in
// use, in the rest of the block. It walks from a random start rather than drawing at random: callers
// close the port before using it, and in a small block random draws would soon hand out one twice.
func FreePort() int {
	blockLow, blockHigh, err := PortRange(os.Getenv(PortsEnv))
	if err != nil {
		panic(err)
	}
	ports.Lock()
	defer ports.Unlock()
	low, high := myPart(blockLow, blockHigh)
	if p, ok := walkPorts(low, high); ok {
		return p
	}
	if p, ok := walkPorts(blockLow, blockHigh); ok {
		return p
	}
	panic(fmt.Sprintf("no free port in %d-%d (%s)", blockLow, blockHigh, PortsEnv))
}

func walkPorts(low, high int) (int, bool) {
	size := high - low + 1
	if ports.cursor < low || ports.cursor > high {
		ports.cursor = low + rand.IntN(size)
	}
	for range size {
		p := ports.cursor
		ports.cursor++
		if ports.cursor > high {
			ports.cursor = low
		}
		if PortExcluded(p) {
			continue
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			_ = ln.Close() // A probe: the caller binds it.
			return p, true
		}
	}
	return 0, false
}
