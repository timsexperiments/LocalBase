// Package memorytest has the memory test hosts other packages' tests share. memory's own tests keep
// their copy of Unsampled: they can't import this package, which imports memory.
package memorytest

import (
	"context"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Unsampled is a host whose processes are never measured (memory.FootprintSampler), as a Controller
// whose sampler a test set to nil was: what a test checks is the host's figures alone.
type Unsampled struct{ memory.HostProvider }

func (Unsampled) Footprint(context.Context, int) (int64, bool) { return 0, false }
