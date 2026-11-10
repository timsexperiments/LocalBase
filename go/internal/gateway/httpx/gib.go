package httpx

import (
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// GiBText is a byte count as people read it, in GiB to one decimal, or "an unknown amount" when
// it is not known (negative).
func GiBText(v int64) string {
	if v < 0 {
		return "an unknown amount"
	}
	return fmt.Sprintf("%.1f GiB", float64(v)/float64(memory.GiB))
}
