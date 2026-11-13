package gateway

import (
	"context"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// unsampled is a host whose processes are never measured (memory.FootprintSampler), as a Controller
// whose sampler a test set to nil was: what a test checks is the host's figures alone.
type unsampled struct{ memory.HostProvider }

func (unsampled) Footprint(context.Context, int) (int64, bool) { return 0, false }

// measured is a host whose processes' footprints are what footprint says.
type measured struct {
	memory.HostProvider
	footprint func(context.Context, int) (int64, bool)
}

func (m measured) Footprint(ctx context.Context, pid int) (int64, bool) { return m.footprint(ctx, pid) }

// unsampledUnlessMeasured is h, unsampled unless it measures its own processes.
func unsampledUnlessMeasured(h memory.HostProvider) memory.HostProvider {
	if _, ok := h.(memory.FootprintSampler); ok {
		return h
	}
	return unsampled{h}
}
