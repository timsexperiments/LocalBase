package lab

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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
	taken     map[int]bool
}

func newPortBlock() *portBlock {
	b := &portBlock{low: defaultPortLow, high: defaultPortHigh, taken: map[int]bool{}}
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

// take reserves n free ports. They stay taken for this process's life, so a
// gateway restarted on fresh ports never meets an old one's sockets.
func (b *portBlock) take(n int) ([]int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	span := b.high - b.low + 1
	var out []int
	start := rand.IntN(span)
	for i := 0; i < span && len(out) < n; i++ {
		p := b.low + (start+i)%span
		if b.taken[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		ln.Close()
		b.taken[p] = true
		out = append(out, p)
	}
	if len(out) < n {
		return nil, errors.New("lab: no free ports left in the block")
	}
	return out, nil
}
