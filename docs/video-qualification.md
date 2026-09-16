# Video qualification

## FastWan2.2 TI2V 5B Q6_K

`fastwan2.2-ti2v-5b-q6_k` supports text-to-video on Linux x64 with one NVIDIA GPU. The gateway requires exactly 480x832, 81 frames, and 16 fps. Its generation profile is Euler/LCM, three steps, CFG 1, flow shift 3, and seed 42. It uses UMT5 Q8_0, the TAew2.2 decoder, CPU offload, diffusion flash attention, and direct VAE convolution. Image-to-video, generated audio, and macOS are not qualified.

With LocalBase `0658c7d`, published `sd-server-v0.0.1`, and a positive 8 GiB native budget, an RTX 4070 SUPER completed a sequential chat-planning, two-voice TTS, and two-clip video pipeline. All eight used model and voice-reference artifacts matched their pinned sizes and SHA-256 hashes before and after execution. Runtime payloads and library links matched the published archives; embedded tokenizers were included in those checks.

Each native AVI decoded fully at 480x832, 81 frames, 16 fps, and 5.0625 seconds. Client-side assembly produced a fully decoded 162-frame, 10.125-second H.264/AAC MP4. Harbor and Willow narration lasted 1.84 and 2.24 seconds, with no video frame holds. Machine transcripts matched the planned lines. Contact sheets showed coherent mug and fern scenes with exaggerated steam. This is voiceover composition, not lip-synchronized video or native audio generation. Direct listening, naturalness, and subjective voice distinction are not qualified.

Across 757 active-run samples, the whole pipeline peaked at 8,156 MiB GPU use, with at least 3,721 MiB GPU memory free and 79,924,868 KiB host memory available. The maximum sampling gap was 250 ms. These are whole-pipeline observations, not isolated video demand or hard allocation bounds. Both jobs were deleted successfully, temporary speech files were cleared, and all owned processes stopped. Prompt and seed coverage is limited; concurrent inference and broader hardware support are not qualified.

Admission reserves estimated demands of 8 GiB accelerator and 16 GiB host memory. The discrete native budget uses the admitted accelerator demand. These estimates and the native budget are not hard limits on driver allocations; pool reserves and memory monitoring remain active. The 24 GiB unified estimate does not enable unsupported targets.

The diffusion model and UMT5 encoder declare Apache-2.0; TAew2.2 uses MIT. All sources are pinned to commits. The tiny VAE's 22,848,048 bytes were verified against Git blob `9d0ef19504c0b918eb6a8efea1ed8596cf666400` at `madebyollin/taehv` commit `fa579a9a726b0a55951998d73e309bfdf0abd342`. Its catalog SHA-256, `b84609b2a133d48434bd9636bfcb44bf05168dc436e2d3cecf26256faa1f5325`, was measured from those verified bytes, not published by the model author.

## Wan2.2 S2V 14B FP8

`wan2.2-s2v-14b-fp8` is an experimental Linux x64 single-NVIDIA speech-to-video profile. It requires a 480x640 PNG portrait and a supported WAV no longer than 2.0625 seconds, producing exactly 33 frames at 16 fps. The profile uses Euler/discrete, 20 steps, CFG 6, flow shift 3, seed 42, UMT5 Q8, wav2vec2 FP16, and the full 16-channel Wan2.1 VAE. CPU offload and diffusion flash attention are enabled. All four artifacts are pinned and declare Apache-2.0.

Admission reserves estimated demands of 9 GiB accelerator and 32 GiB host memory. The positive native GPU budget derives from the same 9 GiB demand. The 41 GiB unified estimate does not enable macOS or other unsupported targets. The gateway job deadline is 30 minutes, including admission, loading, generation, and artifact handling. Gateway qualification at this budget is pending.

The AVI includes the supplied driving audio, not a generated voice. Lip-sync quality, longer clips, concurrent inference, and broader hardware support are not qualified.

## Wan2.1 T2V 1.3B Q8_0

`wan2.1-t2v-1.3b-q8_0` is an experimental Linux x64 single-NVIDIA catalog profile, not a quality-qualified model. Functional qualification with all used artifacts verified is not established. Its configured text-to-video bounds are exactly 320x320, 33 frames, and 16 fps. Its generation profile is Euler/discrete, 20 steps, CFG 6, flow shift 3, and seed 42, with CPU offload and diffusion flash attention enabled.

Catalog admission uses estimated demands of 9 GiB accelerator and 16 GiB host memory, not verified per-process limits. Live pool availability, accelerator reserves, pressure monitoring, and emergency eviction remain active. The 24 GiB unified estimate does not enable macOS or other unsupported targets.

This estimate is a test starting point, not a performance, capacity, or output-quality guarantee.
