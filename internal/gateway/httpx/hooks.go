package httpx

import "context"

// RunHooks tell a run how its inner request progresses: queue() before
// admission (a queued run holds no memory admission), admit() once
// admitted, finish() with the inference's terminal source. A nil *RunHooks
// (every ordinary request) does nothing.
type RunHooks struct {
	Queued, Admitted func()
	Finished         func(source string)
}

type RunHooksKey struct{}

func RunHooksFrom(ctx context.Context) *RunHooks {
	h, _ := ctx.Value(RunHooksKey{}).(*RunHooks)
	return h
}

func (h *RunHooks) Queue() {
	if h != nil && h.Queued != nil {
		h.Queued()
	}
}

func (h *RunHooks) Admit() {
	if h != nil && h.Admitted != nil {
		h.Admitted()
	}
}

func (h *RunHooks) Finish(source string) {
	if h != nil && h.Finished != nil {
		h.Finished(source)
	}
}
