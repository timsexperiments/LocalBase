package web

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// gaugeSegmentCount is how many equal segments the memory gauge has.
const (
	gaugeSegmentCount = 48
	gaugeHeight       = 20
	// gibFloor is the least that prints as a figure. Less prints "<0.1 GiB" and draws no
	// segment, so a block never stands beside a number that says nothing is there.
	gibFloor = int64(1<<30) / 20
)

// gibText prints bytes for people.
func gibText(b int64) string {
	if b > 0 && b < gibFloor {
		return "<0.1 GiB"
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

// gaugeSegment is one piece of the memory gauge (DESIGN §22.22).
type gaugeSegment struct {
	X, Y, W, H int
	Kind       string // other | models | free | unknown
}

// gaugeSegments lays out memory in use across the gauge: other apps, then models, then
// free. Anything that prints as more than nothing shows at least one segment, and the two
// together never fill the gauge unless memory really is full. Without a reading (a negative
// other, or no capacity) it is a dashed rule: never an empty gauge, never a full one.
func gaugeSegments(other, models, capacity int64) []gaugeSegment {
	if other < 0 || capacity <= 0 {
		out := make([]gaugeSegment, gaugeSegmentCount)
		for i := range out {
			out[i] = gaugeSegment{X: i * 10, Y: gaugeHeight/2 - 1, W: 6, H: 2, Kind: "unknown"}
		}
		return out
	}
	count := func(n int64) int {
		if n < gibFloor {
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

// stripWord says what this computer is doing, from what it is really doing. Its words cover
// the text model and memory pressure until host.memory.localbase reports the other runtimes.
func stripWord(st runtime.Snapshot, pressure string) (word, state string) {
	switch {
	case st.State == "starting":
		return "Getting the model ready", "load"
	case st.State == "draining":
		return "Finishing", "load"
	case pressure == "critical":
		return "Not enough memory", "short"
	case st.Active+st.Waiting > 0:
		return "Busy", "load"
	case pressure == "constrained":
		return "Memory is tight", "short"
	}
	return "Ready", "ready"
}

// memoryView is what the strip and Running share. models is whatever the caller knows of
// the models' share, and source says how it knows, in words.
func (u *UI) memoryView(r *http.Request, models int64, source string) map[string]any {
	snap := u.d.Memory.Snapshot(r.Context())
	capacity := u.d.Memory.Topology().SystemCapacity
	known := capacity > 0 && snap.Available >= 0
	other, free := int64(-1), "Memory unavailable"
	if known {
		other = max(capacity-snap.Available-models, 0)
		free = fmt.Sprintf("%.0f GiB free", float64(snap.Available)/(1<<30))
	}
	word, state := stripWord(u.d.LLM.Snapshot(), snap.Pressure)
	return map[string]any{"Host": computerName(), "Word": word, "State": state, "Free": free, "Known": known,
		"Capacity": capacity, "Available": snap.Available, "Models": models, "Other": other, "Source": source}
}

// memoryStrip is the host-and-memory strip (DESIGN §25): this computer's name, its state
// in words, and its memory. Nil where the gateway has no memory reading.
func (u *UI) memoryStrip(r *http.Request) map[string]any {
	if u.d.Memory == nil || u.d.LLM == nil {
		return nil
	}
	var models int64
	// The plan, not LLM.Footprint: that runs a process per call, and this refreshes on a
	// timer. It covers the text model only until host.memory.localbase exists.
	if id, plan, _ := u.d.LLM.Running(); id != "" {
		models = plan.Demand.UnifiedBytes
	}
	return u.memoryView(r, models, "planned")
}

// stripFragment is the strip's refresh.
func (u *UI) stripFragment(w http.ResponseWriter, r *http.Request) {
	u.fragment(w, r, "running", "memory-strip", u.memoryStrip(r))
}
