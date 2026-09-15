# Video qualification

## Wan2.1 T2V 1.3B Q8_0

`wan2.1-t2v-1.3b-q8_0` is an experimental, Linux x64 single-NVIDIA profile. It is functionally qualified only for text-to-video requests at or below 320x320, 33 frames, and 16 fps. Its fixed generation profile is Euler, 20 steps, CFG 6, and seed 42, with CPU offload and diffusion flash attention enabled. The native backend output used for qualification was AVI.

The initial memory reservation is an estimate, not an authoritative per-process measurement: 8 GiB accelerator and 16 GiB host. The accelerator value rounds the 6.1 GiB whole-GPU peak observed during the bounded 320x320/33-frame Q8 run up by about 30%. The host value conservatively exceeds the observed 8.4 GB host delta and the 7.83 GB artifact bundle. `unifiedBytes` is 24 GiB only to satisfy the shared profile shape; the catalog target gate excludes unified-memory systems.

Runtime admission, pressure monitoring, and emergency eviction remain in force. This estimate is a test starting point, not a performance, capacity, or output-quality guarantee. A real public gateway run using the pinned artifacts is required before merge.
