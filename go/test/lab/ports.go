package lab

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// The reliability tools' port block; LOCALBASE_LAB_PORTS=<low>-<high> moves it.
// No lab process may bind anything outside it.
const (
	defaultPortLow  = 29000
	defaultPortHigh = 29499
)

type portBlock struct {
	mu        sync.Mutex
	low, high int
	taken     map[int]bool        // ever taken by this process
	inUse     map[int]bool        // held by a gateway now
	released  []int               // given back, oldest first
	free      func(port int) bool // whether nothing is listening on a port
	lockDir   string              // where ports are locked against other processes; "" is no lock
	locks     map[int]*os.File    // this process's locks, held for its life
}

func portIsFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func newPortBlock() *portBlock {
	b := &portBlock{low: defaultPortLow, high: defaultPortHigh, taken: map[int]bool{}, free: portIsFree,
		lockDir: filepath.Join(os.TempDir(), "localbase-lab-ports")}
	if raw := os.Getenv("LOCALBASE_LAB_PORTS"); raw != "" {
		lo, hi, ok := strings.Cut(raw, "-")
		l, err1 := strconv.Atoi(lo)
		h, err2 := strconv.Atoi(hi)
		if ok && err1 == nil && err2 == nil && 1024 < l && l < h && h < 65536 {
			b.low, b.high = l, h
		}
	}
	return b
}

// lock keeps a port from the other processes that use this block: go test runs test binaries side by
// side (-p), and each has a block over the same ports. The lock is a file's flock, held until this
// process ends, so a process that dies gives its ports back. Called with b.mu held.
func (b *portBlock) lock(p int) bool {
	if b.lockDir == "" {
		return true
	}
	if err := os.MkdirAll(b.lockDir, 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(b.lockDir, strconv.Itoa(p)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	if b.locks == nil {
		b.locks = map[int]*os.File{}
	}
	b.locks[p] = f
	return true
}

// take reserves n free ports. A port never taken before comes first, so a gateway restarted on
// fresh ports doesn't meet an old one's sockets. Only once the block has none left are ports
// gateways gave back taken again, the longest given back first: a test run more than once in one
// process (-count) would otherwise run out. A take that can't be met takes nothing.
func (b *portBlock) take(n int) ([]int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse == nil {
		b.inUse = map[int]bool{}
	}
	span := b.high - b.low + 1
	var out []int
	start := rand.IntN(span) //nolint:gosec // where to start looking for a free port, not a secret
	for i := 0; i < span && len(out) < n; i++ {
		p := b.low + (start+i)%span
		// Busy now, or another process's: looked at again next time.
		if b.taken[p] || !b.free(p) || !b.lock(p) {
			continue
		}
		b.taken[p], b.inUse[p] = true, true
		out = append(out, p)
	}
	for i := 0; i < len(b.released) && len(out) < n; {
		p := b.released[i]
		if !b.free(p) {
			i++
			continue
		}
		b.released = append(b.released[:i], b.released[i+1:]...)
		b.inUse[p] = true
		out = append(out, p)
	}
	if len(out) < n {
		b.giveBack(out)
		return nil, errors.New("lab: no free ports left in the block")
	}
	return out, nil
}

// release gives a gateway's ports back once it has ended.
func (b *portBlock) release(ports []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.giveBack(ports)
}

// giveBack is release with b.mu held. A port not in use is left as it is, so ports given back
// twice are queued once.
func (b *portBlock) giveBack(ports []int) {
	for _, p := range ports {
		if b.inUse[p] {
			delete(b.inUse, p)
			b.released = append(b.released, p)
		}
	}
}

// reclaim takes back ports a gateway gave back, for a restart on the same ports.
func (b *portBlock) reclaim(ports []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse == nil {
		b.inUse = map[int]bool{}
	}
	for _, p := range ports {
		if i := slices.Index(b.released, p); i >= 0 {
			b.released = append(b.released[:i], b.released[i+1:]...)
		}
		b.inUse[p] = true
	}
}
