package root

import (
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
)

// InUse reports whether another live process holds this root: a gateway, or a command holding the
// operation lease. This process doesn't count: a command reading with its own lease isn't racing
// itself. What it can't tell counts as held, for a caller about to read files someone may be writing.
func InUse(dir string) bool {
	canonical, err := Canonical(dir)
	if err != nil {
		canonical = dir
	}
	switch state, owner, _ := (&Root{Dir: canonical}).OwnerState(); state {
	case "active":
		if owner.PID != os.Getpid() {
			return true
		}
	case "unknown":
		return true
	}
	pid, _, held := lease.Held(canonical)
	return held && pid != os.Getpid()
}
