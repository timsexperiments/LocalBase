package memory

import "context"

// unsampled is a host whose processes are never measured (FootprintSampler), as a Controller
// whose sampler a test set to nil was: what a test checks is the host's figures alone.
type unsampled struct{ HostProvider }

func (unsampled) Footprint(context.Context, int) (int64, bool) { return 0, false }
