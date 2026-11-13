package memory

import "fmt"

// HardwareStatus is doctor's one-line verdict on this computer: what size of models it suits, by its
// unified memory on Apple silicon, else by an NVIDIA GPU, else by its GPU memory.
func HardwareStatus(s Specs) string {
	switch {
	case s.IsAppleSilicon && s.RAMGB >= 64:
		return "✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory."
	case s.IsAppleSilicon && s.RAMGB >= 32:
		return "✅ Good fit for medium coding models (up to 14B/32B) using unified memory."
	case s.IsAppleSilicon && s.RAMGB >= 16:
		return "⚠️ Good for entry coding models (1.5B/7B), but larger models may slow down."
	case s.IsAppleSilicon:
		return "❌ Low memory. Limit to small models (1.5B) and small contexts."
	case len(s.NVIDIA) > 0:
		return fmt.Sprintf("✅ Discrete NVIDIA GPU with %d GB of system memory: models are placed against the GPU's free memory, and what does not fit stays in system memory.", s.RAMGB)
	case s.GPUVramGB >= 16:
		return "✅ Excellent fit for multi-model coding setup (7B/14B models + local STT)."
	case s.GPUVramGB >= 12:
		return "✅ Good fit for 7B models with plenty of context headroom."
	case s.GPUVramGB >= 8:
		return "⚠️ Tight fit for 7B models (warning: context growth may slow down), or perfect for 3B/1.5B models."
	default:
		return "❌ Low VRAM. Focus on 1.5B/3B models or CPU offloading."
	}
}
