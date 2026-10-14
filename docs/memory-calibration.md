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
- Image: the catalog `imageLaunchOptions.diffusionFlashAttention` flag (`--diffusion-fa`). Each model otherwise launches one fixed shape.
- STT and TTS: each launches one fixed shape per model.

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
| qwen3-tts-1.7b-base-q4_k_m       |        4.11 |     3.87 |              3.50 | 6         |
| dreamshaper-v8                   |        4.11 |     2.72 |              3.11 | 4 → 6     |
| flux2-klein-4b-q4_0              |       11.77 |     5.53 |              7.96 | 12 → 16   |
| fastwan2.2-ti2v-5b-q6_k          |        6.50 |    12.80 |              6.50 | 8         |

FastWan runs with CPU offload: its host demand is 12.80 GiB and its unified demand is about 17.2 GB, from a system-wide free-memory drop of 15.06 GB.

## Measurements (2026-10-14)

The remaining runnable image profiles at their catalog defaults, and Wan2.1 T2V at its qualification bounds (320x320, 33 frames, 20 steps, CPU offload, diffusion flash attention). Columns are the same as above. Image profiles that set no step count run the sd-server default of 20 steps. The previous estimate is the formula value before this pass.

| Model                 | Accelerator | Peak RSS | Previous estimate | minVramGb |
| --------------------- | ----------: | -------: | ----------------: | --------- |
| dreamshaper-8-lcm     |        4.11 |     2.81 |              4.11 | 6         |
| stable-diffusion-v1-5 |        4.84 |     4.59 |              6.10 | 8 → 6     |
| z-image-turbo-q4_0    |       12.91 |     6.58 |             13.07 | 16        |
| flux2-klein-4b-q8_0   |       13.48 |     7.84 |             13.64 | 16        |
| realvis-xl-v5.0       |       14.47 |     7.37 |             13.46 | 16        |
| sdxl-turbo            |       14.47 |     7.38 |             13.46 | 16        |
| juggernaut-xl-v9      |       14.47 |     7.37 |             13.62 | 16        |
| flux2-klein-9b-q4_0   |       16.59 |    10.73 |             17.23 | 24        |
| flux1-schnell-q4_0    |       13.57 |    16.77 |             13.72 | 16        |
| wan2.1-t2v-1.3b-q8_0  |        6.50 |    11.31 |              6.50 | 8         |

Every image run except flux1-schnell had a system-wide free-memory drop below its RSS, so host demand is peak RSS. SD1.5 is overestimated by the formula because its F32 checkpoint loads as F16 weights. Wan2.1 runs with CPU offload: the T5 stage (5757 MiB of weights and 297 MiB of compute) is the largest on the accelerator, all 7464 MiB of weights stay in host buffers, and its unified demand is 13.70 GiB. The system-wide free-memory drop was 8.22 GB.

flux1-schnell-q4_0 was measured later the same day with the managed sd-server-v0.0.2, at 1024x1024 and 4 steps. `--clip-on-cpu` keeps CLIP-L and T5-XXL in host buffers (9388.50 MiB including their compute), so its host demand is 16.77 GiB peak RSS. Unlike the other image runs, the system-wide free-memory drop (24.9–26.0 GB on warm runs) exceeded params plus host buffers (22.74 GiB), so the record keeps the largest drop and its unified demand is 24.21 GiB. The accelerator holds 6725.62 MiB of diffusion and VAE params plus the 6657 MiB VAE decode. Its `ae.safetensors` now comes from the ungated Comfy-Org/z_image_turbo repository (Apache-2.0), byte-identical to the gated black-forest-labs/FLUX.1-schnell file (same sha256), so it installs without a Hugging Face token.

## Image diffusion flash attention (2026-10-14)

Image profiles declare `imageLaunchOptions.diffusionFlashAttention` in the catalog, and the launcher passes `--diffusion-fa` when it is set. Every image family the managed sd-server (release sd-server-v0.0.1) can run on Metal was compared with the flag off and on: same prompt, seed 42, catalog defaults. The images matched visually in every pair. The image records above were superseded by FA-on records at the same date.

FA shrinks only the diffusion compute buffer. The peak stage is still the VAE decode, so the accelerator planning value and every `minVramGb` tier are unchanged. RSS moved by less than 2% and the records keep the FA-on peak. Sampling time is sd-server's `sampling completed` line; single runs unless a mean is given.

| Model                 | Diffusion compute (MiB) | Sampling off (s) | Sampling on (s) | Accelerator | Peak RSS |
| --------------------- | ----------------------: | ---------------: | --------------: | ----------: | -------: |
| dreamshaper-v8        |         559.90 → 127.68 |      37.46–41.02 |     10.73–12.07 |        4.11 |     2.78 |
| stable-diffusion-v1-5 |         559.90 → 127.68 |            37.46 |           11.25 |        4.84 |     4.65 |
| dreamshaper-8-lcm     |         559.90 → 127.68 |        3.73–4.82 |       1.08–2.40 |        4.11 |     2.80 |
| sdxl-turbo            |         830.86 → 491.99 |   65.7 (mean, 4) |  64.0 (mean, 4) |       14.47 |     7.51 |
| realvis-xl-v5.0       |         830.86 → 491.99 |            60.33 |           61.24 |       14.47 |     7.27 |
| juggernaut-xl-v9      |         830.86 → 491.99 |            71.38 |           61.47 |       14.47 |     7.25 |
| z-image-turbo-q4_0    |        2379.55 → 690.15 |   92.7 (mean, 3) |  89.3 (mean, 3) |       12.91 |     6.60 |
| flux2-klein-4b-q4_0   |       2739.50 → 1078.44 |      30.83–36.46 |     29.16–29.51 |       11.77 |     5.57 |
| flux2-klein-4b-q8_0   |       2739.50 → 1078.44 |            30.76 |           29.54 |       13.48 |     7.83 |
| flux1-schnell-q4_0    |        2361.25 → 793.19 |   77.6 (mean, 2) |  72.8 (mean, 3) |       13.57 |    16.77 |

Through the gateway (scratch `serve`, dreamshaper-v8, 512x512, 20 steps, four sequential requests, first one cold), a generation took 38.4–39.9 s with FA off and 13.3–13.6 s with FA on.

SD1.5 gains about 3.3x. Flux2 Klein and z-image gain a few percent, and SDXL is within run-to-run noise. FA is on for all of these, since it also frees 0.3–1.7 GiB of compute without slowing generation. It is on for the unmeasured SDXL entries (sdxl-base-1.0, animagine-xl-3.1, realvis-xl-v4.0) and flux2-klein-9b-q4_0 by family.

flux2-klein-9b-q4_0 has no FA-on record, because its artifacts exceed the 8 GB download cap for this pass. Its FA-off record (16.59 GiB accelerator, 10.73 GiB RSS) was removed, and the derived estimate (17.23 GiB accelerator and unified, 10.73 GiB host) now applies. That is at or above the FA-off peak, and the tier stays 24.

flux1-schnell-q4_0 was compared afterwards with sd-server-v0.0.2, using the same prompt and seed. FA cut sampling by about 6% and freed 1.53 GiB of diffusion compute. FA-off images were bit-identical across runs, and FA-on images were bit-identical to each other. Off versus on scored SSIM 0.9987 and PSNR 50.9 dB, with no visible difference. A cold first FA-off run took 96.73 s while paging in the weights and is left out of the mean. FA is on for flux1-schnell.

The repository `bin/sd-server` build is older than the managed binary and reports larger buffers, for example 3555 MiB for the z-image text encoder. Measure with the managed runtime under `bin/runtimes/sd-server/<hash>/`.

## Calibrated constants

The measurements also corrected estimate inputs, so unmeasured models of the same family are sized consistently:

- **Image VAE decode**: 13 live full-resolution feature maps instead of 5. This matches the measured sd.cpp VAE compute buffers for SD1.5 and Flux2 Klein. The SDXL VAE takes 15 (`SDXL_VAE_LIVE_FEATURE_MAPS`): realvis-xl-v5.0, sdxl-turbo and juggernaut-xl-v9 each measured a 7680 MiB VAE compute buffer at 1024x1024, about 1 GiB above the 13-map value.
- **Video VAE decode**: 15 live 64-channel maps per 4-frame chunk for TAEHV decoders, measured on FastWan (14.45 maps). 23 live 96-channel maps for the Wan VAE, measured on Wan2.1 at 320x320x33 (3435 MiB compute, 22.9 maps). The catalog-only video profiles use the Wan VAE value.
- **TTS**: llama-tts now launches the talker with `-c 2048` (`SPEECH_CONTEXT_TOKENS`). Without it the talker allocated its full 32768-token trained context: 3584 MiB of f16 KV, 7.42 GiB accelerator and 6.56 GiB peak RSS, tier 12. The prompt fills the context first, then each 12 Hz codec frame takes one cell. The byte-level BPE prompt is at most 4 tokens per code point, so 256 four-byte private-use code points with a reference voice measured about 1050 cells, and 256 English characters about 60. The worst case is about 1310 cells including the 256-frame cap. The prompt is decoded in one batch and llama.cpp caps `n_batch` at `n_ctx`, so 1024 would reject it. The measured record is that worst case: 224 MiB of KV, 632 MiB talker compute and 3.87 GiB peak RSS. The estimate uses the launch context, the published Qwen3-TTS 1.7B talker shape, and two copies of the projector, which the runtime loads twice.

These raised the derived tiers of unmeasured models: the SDXL family, flux1-schnell, z-image-turbo and flux2-klein-4b-q8_0 went from 12 to 16 GiB. SD1.5 went from 6 to 8, dreamshaper-8-lcm from 4 to 6, flux2-klein-9b from 16 to 24, wan2.2-ti2v-5b and hunyuanvideo-1.5 from 16 to 24, and ltx-2.3-22b from 32 to 48.

The 2026-10-14 pass lowered stable-diffusion-v1-5 from 8 to 6 GiB. The Wan VAE recalibration raised the catalog-only wan2.2-ti2v-5b-q6_k and hunyuanvideo-1.5-t2v-480p-q4_k_m from 24 to 32 GiB. It also raised the Wan2.2 S2V unified demand to 32.33 GiB; its accelerator minimum stays 24. The SDXL VAE change pushed juggernaut-xl-v9's derived tier to 24, so it was measured as well. The measured demand keeps it at 16.

## Not measured

Models without a measured record keep estimate confidence:

- flux2-klein-9b-q4_0 with `--diffusion-fa`: it uses the derived estimate (see above).
- wan2.2-s2v-14b-fp8: it targets only Linux x64 with NVIDIA GPUs.
- The catalog-only video profiles, the q8_0 LLM variants and other speech models.
- qwen3.8-27b-q4_k_m: skipped in the first pass because its download was paused.

## Adding a measurement

1. Run the model through a scratch LocalBase root at the shape the gateway launches. For LLMs, that is the reference shape above.
2. Record the backend's GPU and CPU buffer reports from its log. Sample the backend process RSS every 250 ms through a real workload. For offloaded video, also sample free plus inactive system memory.
3. Add a record to `MEMORY_MEASUREMENTS` with the model id, variant, shape, bytes, date, hardware and method.
4. Run `bun test ./src/catalog.test.ts`. If the derived tier changed, update the model's `minVramGb` in `src/catalog.ts`.
