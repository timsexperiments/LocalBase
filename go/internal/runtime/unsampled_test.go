package runtime

import (
	"context"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// unsampled is a host whose processes are never measured (memory.FootprintSampler), as a Controller
// whose sampler a test set to nil was: what a test checks is the host's figures alone.
type unsampled struct{ memory.HostProvider }

func (unsampled) Footprint(context.Context, int) (int64, bool) { return 0, false }
