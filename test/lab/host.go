package lab

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const gib = 1 << 30

// HostSample is one change to the fake machine, kept so checks can ask what the
// gateway could have seen at a given moment.
type HostSample struct {
	At           time.Time
	AvailableGiB float64
	Pressure     string
}

// FakeHost is the file a lab gateway reads its memory from. A write is atomic,
// so the gateway always sees a whole reading.
type FakeHost struct {
	Path string
	Spec HostSpec

	mu      sync.Mutex
	history []HostSample
}

// Set makes availableGiB free at the given pressure (normal, constrained,
// critical or unknown). A negative figure reads as unmeasurable.
func (h *FakeHost) Set(availableGiB float64, pressure string) error {
	b, err := json.Marshal(map[string]any{
		"capacityBytes": int64(h.Spec.CapacityGiB * gib), "unified": h.Spec.Unified,
		"gpuWorkingSetBytes": int64(h.Spec.GPUWorkingSetGiB * gib),
		"availableBytes":     int64(availableGiB * gib), "pressure": pressure})
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", h.Path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.Path); err != nil {
		return err
	}
	h.mu.Lock()
	h.history = append(h.history, HostSample{At: time.Now(), AvailableGiB: availableGiB, Pressure: pressure})
	h.mu.Unlock()
	return nil
}

// History is every change so far, oldest first.
func (h *FakeHost) History() []HostSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HostSample(nil), h.history...)
}

// ReserveBytes is the fixture's system reserve on this host: max(15%, 8 GiB).
func (h *FakeHost) ReserveBytes() int64 {
	return max(int64(8*gib), int64(h.Spec.CapacityGiB*gib*15/100))
}
