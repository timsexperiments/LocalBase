package memory

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// Every rung of doctor's verdict, word for word as doctor printed it before it moved here, and none of
// them changed by the escaping the command line gives a printed value.
func TestHardwareStatus(t *testing.T) {
	nvidia := []NvidiaGPU{{Name: "NVIDIA GeForce RTX 3060", MemoryMiB: 12288}}
	for _, tc := range []struct {
		s    Specs
		want string
	}{
		{Specs{IsAppleSilicon: true, RAMGB: 64}, "✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory."},
		{Specs{IsAppleSilicon: true, RAMGB: 128, GPUVramGB: 4}, "✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory."},
		{Specs{IsAppleSilicon: true, RAMGB: 32}, "✅ Good fit for medium coding models (up to 14B/32B) using unified memory."},
		{Specs{IsAppleSilicon: true, RAMGB: 63}, "✅ Good fit for medium coding models (up to 14B/32B) using unified memory."},
		{Specs{IsAppleSilicon: true, RAMGB: 16}, "⚠️ Good for entry coding models (1.5B/7B), but larger models may slow down."},
		{Specs{IsAppleSilicon: true, RAMGB: 8, NVIDIA: nvidia}, "❌ Low memory. Limit to small models (1.5B) and small contexts."},
		{Specs{RAMGB: 32, GPUVramGB: 4, NVIDIA: nvidia}, "✅ Discrete NVIDIA GPU with 32 GB of system memory: models are placed against the GPU's free memory, and what does not fit stays in system memory."},
		{Specs{RAMGB: 64, GPUVramGB: 16}, "✅ Excellent fit for multi-model coding setup (7B/14B models + local STT)."},
		{Specs{GPUVramGB: 12}, "✅ Good fit for 7B models with plenty of context headroom."},
		{Specs{GPUVramGB: 8}, "⚠️ Tight fit for 7B models (warning: context growth may slow down), or perfect for 3B/1.5B models."},
		{Specs{GPUVramGB: 7, RAMGB: 64}, "❌ Low VRAM. Focus on 1.5B/3B models or CPU offloading."},
	} {
		got := HardwareStatus(tc.s)
		if got != tc.want {
			t.Errorf("%+v:\n got %q\nwant %q", tc.s, got, tc.want)
		}
		if termsafe.Printable(got) != got {
			t.Errorf("printing changes %q", got)
		}
	}
}
