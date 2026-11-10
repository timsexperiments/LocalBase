package memory

import (
	"fmt"
)

// GiBText is a byte count as people read it, in GiB to one decimal, or "an unknown amount" when
// it is not known (negative).
func GiBText(v int64) string {
	if v < 0 {
		return "an unknown amount"
	}
	return fmt.Sprintf("%.1f GiB", float64(v)/float64(GiB))
}

// SizeText says a limit in MiB when it is whole MiB, else in bytes.
func SizeText(n int) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
