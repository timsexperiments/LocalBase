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

// BytesText is the size of a file or a download as people read it: GiB, MiB or KiB to one decimal,
// else "N bytes". The unit is picked after rounding, so a size that rounds to 1024.0 of one unit
// says 1.0 of the next. SizeText is for a limit.
func BytesText(n int64) string {
	if n < 1<<10 {
		return fmt.Sprintf("%d bytes", n)
	}
	units := [...]string{"KiB", "MiB", "GiB"}
	v, u := float64(n)/(1<<10), 0
	for u < len(units)-1 && (v >= 1<<10 || fmt.Sprintf("%.1f", v) == "1024.0") {
		v, u = v/(1<<10), u+1
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

// SizeText says a limit in MiB when it is whole MiB, else in bytes.
func SizeText(n int) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
