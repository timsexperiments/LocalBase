package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
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
		return "Text model busy", "load"
	case pressure == "constrained":
		return "Memory is tight", "short"
	}
	return "Ready", "ready"
}

// stripMemoryEvery is how old the strip's memory reading may be. Reading memory runs vm_stat
// on macOS, and the strip's state word refreshes every 3 s from every open tab: that word
// comes from the runtime's own state, and this reading is shared.
const stripMemoryEvery = 15 * time.Second

// memoryCache keeps one memory reading for everyone who asks within stripMemoryEvery.
type memoryCache struct {
	mu   sync.Mutex
	at   time.Time
	snap memory.Snapshot
}

func (c *memoryCache) read(now time.Time, fresh func() memory.Snapshot) memory.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || now.Sub(c.at) >= stripMemoryEvery {
		c.snap, c.at = fresh(), now
	}
	return c.snap
}

// memoryView is what the strip and Running share: the computer's own reading, in use and
// free, from one source. It draws no share for LocalBase, because the only figure there is
// covers the text model alone and would pass for all of LocalBase. textModel is that figure,
// and Running labels it for what it is, until host.memory.localbase can say the whole.
func (u *UI) memoryView(snap memory.Snapshot, textModel int64, source string) map[string]any {
	capacity := u.d.Memory.Topology().SystemCapacity
	known := capacity > 0 && snap.Available >= 0
	used, free := int64(-1), "Memory unavailable"
	if known {
		used = max(capacity-snap.Available, 0)
		free = fmt.Sprintf("%.0f GiB free", float64(snap.Available)/(1<<30))
	}
	word, state := stripWord(u.d.LLM.Snapshot(), snap.Pressure)
	// What a refresh must see change to redraw anything: the words, and a place on the gauge.
	places := int64(0)
	if capacity > 0 {
		places = used * gaugeSegmentCount / capacity
	}
	sum := fnv.New32a()
	sum.Write([]byte(word + "|" + free + "|" + computerName() + "|" + strconv.FormatInt(places, 10)))
	key := strconv.FormatUint(uint64(sum.Sum32()), 16)
	return map[string]any{"Host": computerName(), "Word": word, "State": state, "Free": free, "Known": known, "Key": key,
		"Capacity": capacity, "Available": snap.Available, "Used": used, "TextModel": textModel, "Source": source}
}

// memoryStrip is the host-and-memory strip (DESIGN §25): this computer's name, its state
// in words, and its memory. Nil where the gateway has no memory reading.
func (u *UI) memoryStrip(r *http.Request) map[string]any {
	if u.d.Memory == nil || u.d.LLM == nil {
		return nil
	}
	// Its own context: this reading is shared, and one request ending must not spoil it.
	snap := u.stripMemory.read(time.Now(), func() memory.Snapshot { return u.d.Memory.Snapshot(context.WithoutCancel(r.Context())) })
	return u.memoryView(snap, 0, "")
}

// stripFragment is the strip's refresh.
// nothing changed since the page drew it, it answers 204 and nothing is swapped.
func (u *UI) stripFragment(w http.ResponseWriter, r *http.Request) {
	strip := u.memoryStrip(r)
	if strip != nil && r.Header.Get("X-Strip-Key") == strip["Key"] {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u.fragment(w, r, "running", "memory-strip", strip)
}
