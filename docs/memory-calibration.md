# Memory calibration

LocalBase admits a runtime only when its memory demand fits the pool. Demand comes from a formula estimate (`src/domains/runtime/memory-estimate.ts`). When a model has been measured at its exact launch shape, the measured record replaces the estimate and the demand carries confidence `measured`. Records live in [`src/domains/runtime/memory-measurements.ts`](../src/domains/runtime/memory-measurements.ts).

Measured demand is still an admission reservation, not a hard limit. Pool reserves, pressure monitoring and emergency eviction remain active.

## Method

Each model ran on one machine (Apple M-series, 64 GiB unified memory, macOS 26) through a scratch LocalBase install, using the backend exactly as the gateway launches it. LLMs used their reference shape: ctx 8192, 1 slot, q8_0 K/V cache and flash attention. Each run executed a short real workload:

- LLM: two chats of about 1750 prompt tokens and up to 256 output tokens.
- STT: two transcriptions of a 13 s clip.
- TTS: one synthesis.
- Image: one generation at the catalog defaults.
- Video: one generation at the qualification bounds and launch options.

Three quantities were recorded:

- **Backend accelerator bytes**: the sum of the backend's own GPU buffer report. This is llama.cpp `MTL0` model, context and compute buffers; the whisper.cpp Metal buffers; or sd.cpp params plus the largest per-stage compute buffer. On Apple unified memory, Metal allocations are not part of the process RSS, so RSS alone under-reports the accelerator demand.
- **Host buffer bytes**: the backend's CPU buffer report, or 0 if it prints none.
- **Peak RSS**: the largest RSS of the backend process, sampled every 250 ms for the whole workload. For video, the peak drop in free plus inactive system memory was also sampled.

Units: llama.cpp and sd.cpp print MiB, while whisper.cpp prints MB (10^6 bytes).

## How records are applied

A record applies only when the launch shape matches exactly:

- LLM: ctx, slots and both KV types.
- Video: width, height, frames, CPU offload and diffusion flash attention.
- STT, TTS and image: each launches one fixed shape per model.

Any other shape uses the estimate. `measuredDemand` turns a record into a planning demand:

- accelerator = backend accelerator bytes + 512 MiB allowance for unreported Metal context, pipeline cache and allocator slack. Whisper uses 128 MiB. Video is rounded up to 256 MiB, like its estimate.
- host = max(peak RSS, host buffers, on-disk artifact bytes) + prompt cache. Mmapped pages a run never touched still count.
- unified = max(accelerator + host buffers, host, system delta) + prompt cache.

For a locally installed LLM whose file is larger than the catalog artifact, the launch plan adds the difference. The catalog `minVramGb` tier derives from `catalogModelDemand` (measured when recorded, else the estimate) through `minVramTierGb`. `catalog.test.ts` enforces that the two agree.

## Measurements (2026-10-13)

Accelerator is the planning value (backend plus allowance), in GiB. The previous estimate is the formula value before this calibration.

| Model                            | Accelerator | Peak RSS | Previous estimate | minVramGb |
| -------------------------------- | ----------: | -------: | ----------------: | --------- |
| qwen3.5-0.8b-q4_k_m              |        1.56 |     0.77 |              1.59 | 4         |
| qwen3.5-2b-q4_k_m                |        2.28 |     1.48 |              2.48 | 4         |
| qwen3.5-4b-q4_k_m                |        3.84 |     3.09 |              4.09 | 6         |
| qwen3.5-9b-q4_k_m                |        6.67 |     5.95 |              7.22 | 8         |
| qwen3.5-27b-q4_k_m               |       17.73 |    16.85 |             18.58 | 24        |
| ministral-3-14b-instruct-q4_k_m  |        9.09 |     7.29 |              9.27 | 12        |
| mistral-nemo-12b-instruct-q4_k_m |        8.42 |     7.52 |              8.42 | 12        |
| gpt-oss-20b-q4_k_m               |       11.86 |     9.59 |             11.89 | 16        |
| whisper-tiny-en-q8_0             |        0.32 |     0.16 |              0.26 | 1         |
| whisper-large-v3-turbo           |        1.93 |     1.77 |              1.95 | 4         |
| qwen3-tts-1.7b-base-q4_k_m       |        7.42 |     6.56 |              3.50 | 6 → 12    |
| dreamshaper-v8                   |        4.11 |     2.72 |              3.11 | 4 → 6     |
| flux2-klein-4b-q4_0              |       11.77 |     5.53 |              7.96 | 12 → 16   |
| fastwan2.2-ti2v-5b-q6_k          |        6.50 |    12.80 |              6.50 | 8         |

FastWan runs with CPU offload: its host demand is 12.80 GiB and its unified demand is about 17.2 GB, from a system-wide free-memory drop of 15.06 GB.

## Calibrated constants

The measurements also corrected estimate inputs, so unmeasured models of the same family are sized consistently:

- **Image VAE decode**: 13 live full-resolution feature maps instead of 5. This matches the measured sd.cpp VAE compute buffers for SD1.5 and Flux2 Klein.
- **Video VAE decode**: 15 live feature maps. This is an assumption extrapolated from the image measurement, because the measured FastWan run decodes with TAEHV.
- **TTS**: llama-tts runs without `-c`, so the talker allocates its full 32768-token f16 KV cache, measured at exactly 3584 MiB. The estimate uses that context, the published Qwen3-TTS 1.7B talker shape, and two copies of the projector, which the runtime loads twice.

These raised the derived tiers of unmeasured models: the SDXL family, flux1-schnell, z-image-turbo and flux2-klein-4b-q8_0 went from 12 to 16 GiB. SD1.5 went from 6 to 8, dreamshaper-8-lcm from 4 to 6, flux2-klein-9b from 16 to 24, wan2.2-ti2v-5b and hunyuanvideo-1.5 from 16 to 24, and ltx-2.3-22b from 32 to 48.

## Not measured

Models that were not installed on the measurement machine keep estimate confidence. These include the q8_0 LLM variants, the remaining image and video profiles and other speech models. qwen3.8-27b-q4_k_m was skipped because its download was paused.

## Adding a measurement

1. Run the model through a scratch LocalBase root at the shape the gateway launches. For LLMs, that is the reference shape above.
2. Record the backend's GPU and CPU buffer reports from its log. Sample the backend process RSS every 250 ms through a real workload. For offloaded video, also sample free plus inactive system memory.
3. Add a record to `MEMORY_MEASUREMENTS` with the model id, variant, shape, bytes, date, hardware and method.
4. Run `bun test ./src/catalog.test.ts`. If the derived tier changed, update the model's `minVramGb` in `src/catalog.ts`.
