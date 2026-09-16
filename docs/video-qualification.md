# Video qualification

## FastWan2.2 TI2V 5B Q6_K

`fastwan2.2-ti2v-5b-q6_k` supports text-to-video on Linux x64 with one NVIDIA GPU. The gateway profile is 480x832, 81 frames, 16 fps, Euler/LCM, three steps, CFG 1, flow shift 3, and seed 42. It uses UMT5 Q8_0, the TAew2.2 decoder, CPU offload, diffusion flash attention, and direct VAE convolution. Image-to-video, generated audio, and macOS are not qualified.

With published `sd-server-v0.0.1` and a positive 8 GiB native budget, an RTX 4070 SUPER completed the 81-frame profile in 48.65 seconds. The resulting 5.0625-second AVI decoded fully. Memory sampling observed a 6,624 MiB whole-GPU peak and about 10.3 GiB reduction in host available memory. Contact sheets showed coherent cup geometry and natural colors, with exaggerated steam. Prompt and seed coverage is limited; concurrent inference is not qualified by this test.

Admission reserves estimated demands of 8 GiB accelerator and 16 GiB host memory. The discrete native budget uses the admitted accelerator demand. These estimates and the native budget are not hard limits on driver allocations; pool reserves and memory monitoring remain active. The 24 GiB unified estimate does not enable unsupported targets.

The diffusion model and UMT5 encoder declare Apache-2.0; TAew2.2 uses MIT. All sources are pinned to commits. The tiny VAE's 22,848,048 bytes were verified against Git blob `9d0ef19504c0b918eb6a8efea1ed8596cf666400` at `madebyollin/taehv` commit `fa579a9a726b0a55951998d73e309bfdf0abd342`. Its catalog SHA-256, `b84609b2a133d48434bd9636bfcb44bf05168dc436e2d3cecf26256faa1f5325`, was measured from those verified bytes, not published by the model author.

## Wan2.1 T2V 1.3B Q8_0

`wan2.1-t2v-1.3b-q8_0` is an experimental, Linux x64 single-NVIDIA profile. It is functionally qualified only for text-to-video requests at or below 320x320, 33 frames, and 16 fps. Its fixed generation profile is Euler, 20 steps, CFG 6, FlowShift 3, and seed 42, with CPU offload and diffusion flash attention enabled. Current PCI-bound qualification used stable-diffusion.cpp `07a85c74cb08cda3aa176f688c5d8f522615e2b9` on an RTX 4070 SUPER and exercised AVI and WebM output.

The current PCI-bound run observed a 9,181 MiB whole-GPU peak for the bounded profile. Catalog admission therefore uses an estimated 9 GiB accelerator demand and retains the 16 GiB host demand; neither is an authoritative per-process limit. Live pool availability, the existing accelerator reserve, pressure monitoring, and emergency eviction apply in addition to this demand. The 24 GiB unified-memory value does not enable macOS or other unsupported unified-memory targets.

This estimate is a test starting point, not a performance, capacity, or output-quality guarantee.
