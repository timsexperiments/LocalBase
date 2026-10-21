# Memory calibration

LocalBase admits a runtime only when its memory demand fits the pool. Demand comes from a formula estimate (`src/domains/runtime/memory-estimate.ts`). When a model has been measured at its exact launch shape, the measured record replaces the estimate and the demand carries confidence `measured`. A matching feed peak (`feed`) or a peak observed on this machine (`observed`) can also replace it; see [Admission](#admission). Records live in [`src/domains/runtime/memory-measurements.ts`](../src/domains/runtime/memory-measurements.ts).

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

For a locally installed LLM whose file is larger than the catalog artifact, the launch plan adds the difference.

## Minimum memory and fit

The catalog no longer stores a hand-entered `minVramGb`. Each model's minimum is computed in `src/domains/models/memory-fit.ts`: the accelerator demand of its memory figure plus a margin of max(512 MiB, 10%), rounded up to whole GiB (`minimumMemoryGb`). The figure comes from, most trusted first:

1. **observed**: the peak this machine recorded while serving the model (below).
2. **measured**: a record in `MEMORY_MEASUREMENTS` at the reference shape.
3. **estimate**: the formula estimate from artifact sizes. For an installed GGUF (LLM or TTS talker), the layer, KV-head and embedding geometry is read from the file. Otherwise the shape comes from a size heuristic (`fallbackLlmShape`): P billion parameters from the catalog size, else from the bytes at 4.89 bits per weight; layers = ceil(16 × P^0.4); embedding = sqrt(P × 10^9 / (12 × layers)); 8 KV heads of dimension 128; vocabulary 152064. It assumes full attention in every layer, so it overstates hybrid and sliding-window models until the file is installed.

The API, CLI, diagnostics and UI report the source next to the minimum. `minVramGb` and `minimumVramEstimateGb` remain in the JSON outputs and hold the computed minimum. Despite the field names, it is a planning minimum at one launch shape, not a hardware or VRAM requirement, so the UI and CLI label it "Planning minimum" (`plan_min` in `models catalog`) and state its assumptions: `models show` and the model details view print the reference launch shape and whether the figure is a formula estimate or was measured, with the measuring machine (Apple M-series 64 GiB, macOS 26 for every checked-in record, exposed as `memory.reference.hardware` in model metadata). The minimum comes from the accelerator demand; weight size and peak RSS feed the host and unified demands, never the minimum on their own. The verdict for this machine is `fits`, `tight` (under 4 GiB headroom, or the minimum is over 75% of memory) or `wont-fit`. Admission uses the full launch-plan demand, not the minimum; see [Admission](#admission).

## Observed peaks

Once a backend is ready, the gateway samples its memory every 5 s until it exits and keeps the largest value in the `model_observed_memory` table, keyed by model id, artifact sha256 and launch shape (`ctx=N;slots=N;kv=K/V` for LLMs, `default` otherwise). Only launches of the pinned catalog artifact are recorded. On macOS the sample is the `footprint` TOTAL (dirty plus clean), which includes Metal allocations and is used for fit. On Linux it is `VmHWM`, which omits discrete GPU memory, so it is stored but not used. The figure prefers the peak at the reference shape, else the newest one. A peak reflects only what the workload touched, so a short session can observe less than the reference demand.

## Feed peaks

Entries without checked-in measurements (user and discovered entries) can use a measured peak published in the catalog feed. The figure source is `feed`. A feed peak is used only when the annotation is an exact revision match, the recipe's primary sha256 (if any) matches the pinned artifact, and its launch shape equals the planned one (the reference shape for LLMs, `default` for STT, `ctx=N` for TTS, the flash-attention setting for image, and the qualification shape for video). Precedence is observed, then checked-in measurement, then feed, then estimate. A runtime observation always wins.

## Admission

Launch admission uses the same order as the fit figure, applied to the planned launch (`src/domains/runtime/admission-demand.ts`):

1. **observed**: a macOS `footprint` peak recorded on this machine for the same artifact sha256 and the exact planned launch shape. The demand is the peak plus max(512 MiB, 10%), the same margin as `minimumMemoryGb`, used for unified, host and accelerator alike. Runtime always wins, so an observed peak replaces every other figure, larger or smaller. Unlike the fit figure, admission does not fall back to a peak from another shape. A different context, slot count or KV type plans from the sources below.
2. **measured**: a checked-in record at the planned shape.
3. **feed**: a feed peak for the exact file, revision and planned shape, turned into a demand by `measuredDemand`, as for checked-in records. If several match, the largest is used. A local file larger than the catalog artifact adds the difference.
4. **estimate**: the formula estimate.

Evidence applies only when the launch loads the catalog primary artifact. A launch overridden to another model file plans from its estimate. Diagnostics report the source as `demand_confidence`. Admission only reserves memory. The pressure monitor that runs after load is unchanged and can still evict a runtime whose real use exceeds its reservation.

## Measurements (2026-10-13)

Accelerator is the planning value (backend plus allowance), in GiB. The previous estimate is the formula value before this calibration.

| Model                            | Accelerator | Peak RSS | Previous estimate | minVramGb |
| -------------------------------- | ----------: | -------: | ----------------: | --------- |
| qwen3.5-0.8b-q4_k_m              |        1.56 |     0.77 |              1.59 | 4         |
| qwen3.5-2b-q4_k_m                |        2.28 |     1.48 |              2.48 | 4         |
| qwen3.5-4b-q4_k_m                |        3.84 |     3.09 |              4.09 | 6         |
| qwen3.5-9b-q4_k_m                |        6.67 |     5.95 |              7.22 | 8         |
| qwen3.5-27b-q4_k_m               |       17.73 |    17.72 |             18.58 | 24        |
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

qwen3.5-27b-q4_k_m was re-measured on 2026-10-14 with the same shape and workload, both through a scratch gateway and with llama-server run directly at `-lv 4`. The MTL0 buffers (16710.81 MiB model, 272.00 MiB KV, 149.62 MiB recurrent state, 513.28 MiB compute) and 711.29 MiB of CPU buffers matched the first pass. Peak RSS was 17.31–17.72 GiB, above the first-pass 16.85 GiB, so the record keeps 17.72 GiB (19027968000 B). Its host demand rises to 19.72 GiB with the prompt cache. The tier stays 24.

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
- **TTS**: llama-tts now launches the talker with `-c 2048` (`SPEECH_CONTEXT_TOKENS`). Without it the talker allocated its full 32768-token trained context: 3584 MiB of f16 KV, 7.42 GiB accelerator and 6.56 GiB peak RSS, tier 12. The prompt fills the context first, then each 12 Hz codec frame takes one cell. The byte-level BPE prompt is at most 4 tokens per code point, so 256 four-byte private-use code points with a reference voice measured about 1050 cells, and 256 English characters about 60. The worst case is about 1310 cells including the 256-frame cap. The prompt is decoded in one batch and llama.cpp caps `n_batch` at `n_ctx`, so 1024 would reject it. The measured record is that worst case: 224 MiB of KV, 632 MiB talker compute and 3.87 GiB peak RSS. The estimate uses the launch context, the talker geometry (from the GGUF when installed, else the size heuristic), and two copies of the projector, which the runtime loads twice.
- **Persistent TTS**: Qwen3-TTS now runs in the warm `tts-server` (same talker, projector, codec and `-c 2048`/`-n 256` argv as llama-tts), so the demand is held while the server idles rather than only during a request. A 2026-10-14 recheck with the worst-case input measured 4148838400 B peak RSS and 3.26 GB peak footprint, within the existing record (4160536576 B), so the record and tier are unchanged. llama-tts remains the fallback when no tts-server package is available and has the same demand.

These raised the derived tiers of unmeasured models: the SDXL family, flux1-schnell, z-image-turbo and flux2-klein-4b-q8_0 went from 12 to 16 GiB. SD1.5 went from 6 to 8, dreamshaper-8-lcm from 4 to 6, flux2-klein-9b from 16 to 24, wan2.2-ti2v-5b and hunyuanvideo-1.5 from 16 to 24, and ltx-2.3-22b from 32 to 48.

The 2026-10-14 pass lowered stable-diffusion-v1-5 from 8 to 6 GiB. The Wan VAE recalibration raised the catalog-only wan2.2-ti2v-5b-q6_k and hunyuanvideo-1.5-t2v-480p-q4_k_m from 24 to 32 GiB. It also raised the Wan2.2 S2V unified demand to 32.33 GiB; its accelerator minimum stays 24. The SDXL VAE change pushed juggernaut-xl-v9's derived tier to 24, so it was measured as well. The measured demand keeps it at 16.

## Decision model (2026-10-15)

clef-flash-q4_k_m was measured with its decision launch through a scratch gateway: ctx 8192, 1 slot, batch and ubatch equal to the context, flash attention, q8_0 KV types, `--cache-ram 0` and the Q8_0 projector. The workload was the three model-card decision requests (invoice, checkout, Stripe). Columns are the same as above.

| Model             | Accelerator | Peak RSS | Previous estimate | minVramGb |
| ----------------- | ----------: | -------: | ----------------: | --------- |
| clef-flash-q4_k_m |        8.62 |     7.96 |              9.26 | 11 → 10   |

A decision server keeps no KV or recurrent context (0 MiB). The MTL0 buffers are 5629.85 MiB of model weights, a 1842.53 MiB compute buffer (the 8192-token ubatch), and 595.29 MiB of projector weights plus a 248.10 MiB projector compute buffer. Host buffers are 1211.86 MiB: the 545.64 MiB CPU-mapped embedding, 640.34 MiB of CPU compute, 24.93 MiB of projector CPU compute and the 0.95 MiB output buffer. Peak RSS from `/usr/bin/time -l` was 8351440 KiB. Steady RSS through the gateway after the three requests was 7431296 KiB.

## Not measured

Built-in entries without a measured record keep estimate confidence:

- clef-q4_k_m (Clef 27B, added 2026-10-18): estimate 21.99 GiB accelerator at the 8192-token decision launch, minimum 25 GB. Measure it with the decision method above (ctx 8192, batch = ctx, `--cache-ram 0`, Q8_0 projector).
- qwen3.8-27b-q4_k_m: skipped in the first pass because its download was paused. Estimate 18.38 GiB, minimum 21 GB.
- qwen3.6-35b-a3b-iq4_nl_xl (added 2026-10-21): estimate 20.95 GiB accelerator at the reference launch (8192-token context, 1 request, q8_0 KV cache, F16 projector), minimum 24 GB.
- qwen3-embedding-0.6b-q8_0 and qwen3-embedding-4b-q4_k_m: estimates assume short inputs.
- whisper-base-q8_0: estimate only.
- wan2.2-s2v-14b-fp8: it targets only Linux x64 with NVIDIA GPUs.

## Former built-in entries

Entries that left the built-in catalog (`src/catalog/former/`) keep their records here. An install that kept one as a user entry (see the README's built-in catalog section) still plans from its record while the entry's files and launch shape match what was measured (`usesReferenceMeasurements`). Their unmeasured profiles, such as flux2-klein-9b-q4_0 with `--diffusion-fa`, the catalog-only video profiles and the q8_0 LLM variants, keep their estimates.

## Adding a measurement

1. Run the model through a scratch LocalBase root at the shape the gateway launches. For LLMs, that is the reference shape above.
2. Record the backend's GPU and CPU buffer reports from its log. Sample the backend process RSS every 250 ms through a real workload. For offloaded video, also sample free plus inactive system memory.
3. Add a record to `MEMORY_MEASUREMENTS` with the model id, variant, shape, bytes, date, hardware and method.
4. Run `bun test ./src/catalog.test.ts`. The computed minimum follows the record.
