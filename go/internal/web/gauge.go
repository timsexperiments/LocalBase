package web

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// gaugeSegmentCount is how many equal segments the memory gauge has.
const (
	gaugeSegmentCount = 48
	gaugeHeight       = 20
)

// gaugeSegment is one piece of the memory gauge (DESIGN §22.22).
type gaugeSegment struct {
	X, Y, W, H int
	Kind       string // other | models | free
}

// gaugeSegments lays out memory in use across the gauge: other apps, then models, then
// free. Anything in use shows at least one segment, and the two together never fill the
// gauge unless memory really is full.
func gaugeSegments(other, models, capacity int64) []gaugeSegment {
	count := func(n int64) int {
		if n <= 0 || capacity <= 0 {
			return 0
		}
		return max(1, int((n*gaugeSegmentCount+capacity/2)/capacity))
	}
	o, m := count(other), count(models)
	for o+m > gaugeSegmentCount || (o+m == gaugeSegmentCount && other+models < capacity) {
		if o >= m {
			o--
		} else {
			m--
		}
	}
	out := make([]gaugeSegment, gaugeSegmentCount)
	for i := range out {
		// Models are tall blocks, other apps short ones, and free memory a rule with a
		// longer one every fourth: they differ by shape, not only color.
		switch x := i * 10; {
		case i < o:
			out[i] = gaugeSegment{X: x, Y: gaugeHeight - 11, W: 8, H: 11, Kind: "other"}
		case i < o+m:
			out[i] = gaugeSegment{X: x, W: 8, H: gaugeHeight, Kind: "models"}
		case i%4 == 0:
			out[i] = gaugeSegment{X: x + 3, W: 2, H: gaugeHeight, Kind: "free"}
		default:
			out[i] = gaugeSegment{X: x + 3, Y: 6, W: 2, H: gaugeHeight - 6, Kind: "free"}
		}
	}
	return out
}

// computerName is the name shown for this computer. Scratch gateways and tests set
// LOCALBASE_COMPUTER_NAME, because a hostname often holds its owner's name.
func computerName() string {
	if n := strings.TrimSpace(os.Getenv("LOCALBASE_COMPUTER_NAME")); n != "" {
		return n
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "This computer"
	}
	return strings.TrimSuffix(host, ".local")
}

// memoryStrip is the host-and-memory strip (DESIGN §25): this computer's name, its state
// in words, and its memory. Nil where the gateway has no memory reading.
func (u *UI) memoryStrip(r *http.Request) map[string]any {
	if u.d.Memory == nil || u.d.LLM == nil {
		return nil
	}
	snap := u.d.Memory.Snapshot(r.Context())
	capacity := u.d.Memory.Topology().SystemCapacity
	var models int64
	// The plan, not LLM.Footprint: that runs a process per call, and this refreshes on a
	// timer. It covers the text model only until host.memory.localbase exists.
	if id, plan, _ := u.d.LLM.Running(); id != "" {
		models = plan.Demand.UnifiedBytes
	}
	word, state := "Ready", "ready"
	switch st := u.d.LLM.Snapshot(); {
	case st.State == "starting":
		word, state = "Getting the model ready", "load"
	case st.State == "draining":
		word, state = "Finishing", "load"
	case snap.Pressure == "critical" || snap.Pressure == "constrained":
		word, state = "Memory is tight", "short"
	}
	free := "Free memory unknown"
	if snap.Available >= 0 {
		free = fmt.Sprintf("%.0f GB free", float64(snap.Available)/(1<<30))
	}
	return map[string]any{"Host": computerName(), "Word": word, "State": state, "Free": free, "Capacity": capacity,
		"Available": snap.Available, "Models": models, "Other": max(capacity-max(snap.Available, 0)-models, 0)}
}

// stripFragment is the strip's refresh.
func (u *UI) stripFragment(w http.ResponseWriter, r *http.Request) {
	u.fragment(w, r, "running", "memory-strip", u.memoryStrip(r))
}
