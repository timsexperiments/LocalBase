# Whisper native GPU contract

The Linux CI build applies `scripts/whisper-patches/require-gpu-pci.patch` to the checksum-verified Whisper source. `--require-gpu-pci DDDD:BB:DD.F` requires a unique discrete Vulkan backend with that canonical lowercase PCI address. Selection counts all GGML GPU and integrated-GPU entries, not physical Vulkan ordinals. Missing or duplicate matches fail before model loading. GPU initialization failure fails model loading before a CPU-only backend can start. CPU support operations remain allowed.

This contract supports LocalBase's single monitored NVIDIA accelerator policy, including hosts with an additional Intel integrated GPU. It does not enable multi-discrete-GPU admission. macOS builds use the unmodified source.

Release and verify the native artifact before merging a launcher that requires this flag. The launcher must use the PCI identity from the same NVML handle as the admitted UUID and memory measurement, reject missing identity, and diagnose unsupported older binaries. Native runtimes are built only in CI; users do not compile them.
