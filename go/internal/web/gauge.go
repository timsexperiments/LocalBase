package web

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strconv"
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

// stripWord says what this computer is doing, from what it is really doing. busy covers every
// modality and detached runs (host.activity, DESIGN §26).
func stripWord(st runtime.Snapshot, pressure string, busy bool) (word, state string) {
	switch {
	case st.State == "starting":
		return "Getting the model ready", "load"
	case st.State == "draining":
		return "Finishing", "load"
	case pressure == "critical":
		return "Not enough memory", "short"
	case busy:
		return "Busy", "load"
	case pressure == "constrained":
		return "Memory is tight", "short"
	}
	return "Ready", "ready"
}

// hostFigure is a figure the gateway may not have: nil means unknown, never 0.
type hostFigure struct {
	Bytes *int64 `json:"bytes"`
}

// hostReading is GET /_localbase/host (DESIGN §26).
type hostReading struct {
	Host struct {
		Name   *string `json:"name"`
		Memory struct {
			Total     hostFigure `json:"total"`
			Available hostFigure `json:"available"`
			Pressure  *string    `json:"pressure"`
			Other     hostFigure `json:"other"`
			LocalBase struct {
				Bytes    int64  `json:"bytes"`
				Source   string `json:"source"`
				Runtimes []struct {
					Runtime string  `json:"runtime"`
					ModelID *string `json:"modelId"`
					Bytes   int64   `json:"bytes"`
					Source  string  `json:"source"`
				} `json:"runtimes"`
			} `json:"localBase"`
		} `json:"memory"`
		Activity struct {
			Busy bool `json:"busy"`
		} `json:"activity"`
	} `json:"host"`
}

// readHost asks the gateway what this computer is doing. It shares one reading a second, so
// the strip's refresh spawns nothing per call.
func (u *UI) readHost(r *http.Request) (*hostReading, bool) {
	var h hostReading
	if u.apiCall(r, "GET", "/_localbase/host", nil, &h) != "" {
		return nil, false
	}
	return &h, true
}

// hostName is what this computer is called: the owner's name for it, else the machine's own.
// LOCALBASE_COMPUTER_NAME wins, so a scratch gateway never shows a real hostname.
func (u *UI) hostName(r *http.Request, h *hostReading) string {
	if strings.TrimSpace(os.Getenv("LOCALBASE_COMPUTER_NAME")) == "" && h != nil && h.Host.Name != nil && *h.Host.Name != "" {
		return *h.Host.Name
	}
	return computerName()
}

// sourceWords says how LocalBase's share was arrived at.
var sourceWords = map[string]string{"measured": "measured", "planned": "planned", "mixed": "measured and planned"}

// memoryView is what the strip and Running share. Every figure is the gateway's, from one reading.
func (u *UI) memoryView(r *http.Request, h *hostReading) map[string]any {
	m := h.Host.Memory
	known := m.Total.Bytes != nil && m.Available.Bytes != nil && m.Other.Bytes != nil
	capacity, available, other := int64(0), int64(-1), int64(-1)
	free := "Memory unavailable"
	if known {
		capacity, available, other = *m.Total.Bytes, *m.Available.Bytes, *m.Other.Bytes
		free = fmt.Sprintf("%.0f GiB free", float64(available)/(1<<30))
	}
	pressure := ""
	if m.Pressure != nil {
		pressure = *m.Pressure
	}
	var st runtime.Snapshot
	if u.d.LLM != nil {
		st = u.d.LLM.Snapshot()
	}
	word, state := stripWord(st, pressure, h.Host.Activity.Busy)
	name := u.hostName(r, h)
	// What a refresh must see change to redraw anything: the words, and places on the gauge.
	places := [2]int64{}
	if capacity > 0 {
		places = [2]int64{other * gaugeSegmentCount / capacity, m.LocalBase.Bytes * gaugeSegmentCount / capacity}
	}
	sum := fnv.New32a()
	fmt.Fprint(sum, word, "|", free, "|", name, "|", places)
	return map[string]any{"Host": name, "Word": word, "State": state, "Free": free, "Known": known, "Key": strconv.FormatUint(uint64(sum.Sum32()), 16),
		"Capacity": capacity, "Available": available, "Other": other, "Models": m.LocalBase.Bytes, "Source": sourceWords[m.LocalBase.Source]}
}

// memoryStrip is the host-and-memory strip (DESIGN §25): this computer's name, its state in
// words, and its memory. Nil for someone who can't read it.
func (u *UI) memoryStrip(r *http.Request) map[string]any {
	h, ok := u.readHost(r)
	if !ok {
		return nil
	}
	return u.memoryView(r, h)
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
