package runtime

import (
	"context"
	"testing"
	"time"
)

// The zero value of each test-hook struct is how the package runs, and a set hook is used.
func TestTheHooksZeroValueIsProduction(t *testing.T) {
	var h hooks
	if h.period() != 5*time.Second || h.settleLimit() != memorySettleTimeout || h.settleInterval() != memorySettleInterval {
		t.Errorf("hooks{}: period %v, settle %v, step %v", h.period(), h.settleLimit(), h.settleInterval())
	}
	h = hooks{sample: func(context.Context, int) (int64, bool) { return 42, true }, samplePeriod: time.Millisecond,
		settle: 2 * time.Millisecond, settleStep: 3 * time.Millisecond}
	if b, ok := h.footprint(context.Background(), 1); b != 42 || !ok || h.period() != time.Millisecond ||
		h.settleLimit() != 2*time.Millisecond || h.settleInterval() != 3*time.Millisecond {
		t.Errorf("set hooks: %d %v %v %v %v", b, ok, h.period(), h.settleLimit(), h.settleInterval())
	}

	var p processOptions
	if p.quietLimit() != 2*time.Second || p.capLimit() != 20*time.Second {
		t.Errorf("processOptions{}: %v %v", p.quietLimit(), p.capLimit())
	}
	p = processOptions{drainLimit: time.Millisecond, drainCap: 2 * time.Millisecond}
	if p.quietLimit() != time.Millisecond || p.capLimit() != 2*time.Millisecond {
		t.Errorf("set processOptions: %v %v", p.quietLimit(), p.capLimit())
	}

	var o installOptions
	if got := o.goManifest(); len(got) == 0 || &got[0] != &goManifestJSON[0] || o.source("https://x/a") != "https://x/a" {
		t.Error("installOptions{} isn't the embedded manifest and the pinned URL")
	}
	o = installOptions{manifest: []byte("{}"), fetchURL: func(string) string { return "local" }}
	if string(o.goManifest()) != "{}" || o.source("https://x/a") != "local" {
		t.Error("set installOptions aren't used")
	}
}
