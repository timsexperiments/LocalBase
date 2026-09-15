# Video qualification

## Wan2.1 T2V 1.3B Q8_0

`wan2.1-t2v-1.3b-q8_0` is an experimental, Linux x64 single-NVIDIA profile. It is functionally qualified only for text-to-video requests at or below 320x320, 33 frames, and 16 fps. Its fixed generation profile is Euler, 20 steps, CFG 6, FlowShift 3, and seed 42, with CPU offload and diffusion flash attention enabled. The current PCI-bound Linux runtime qualification is recorded at `/private/tmp/tim-9-video-artifact-qualification/PR162-LINUX-QUALIFICATION.md`.

The current PCI-bound run observed a 9,181 MiB whole-GPU peak for the bounded profile. Catalog admission therefore uses an estimated 9 GiB accelerator demand and retains the 16 GiB host demand; neither is an authoritative per-process limit. Live pool availability, the existing accelerator reserve, pressure monitoring, and emergency eviction apply in addition to this demand. The 24 GiB unified-memory value does not enable macOS or other unsupported unified-memory targets.

This estimate is a test starting point, not a performance, capacity, or output-quality guarantee.
