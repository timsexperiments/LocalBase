package testutil

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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
}

// FreePort returns a free port in the block. It walks the block from a random
// start rather than drawing at random: callers close the port before using it,
// and in a small block random draws would soon hand out one twice.
func FreePort() int {
	low, high, err := PortRange(os.Getenv(PortsEnv))
	if err != nil {
		panic(err)
	}
	size := high - low + 1
	ports.Lock()
	defer ports.Unlock()
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
			ln.Close()
			return p
		}
	}
	panic(fmt.Sprintf("no free port in %d-%d (%s)", low, high, PortsEnv))
}
