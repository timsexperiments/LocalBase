# What runs where

This page says what has actually been run, and on what. "Not run" doesn't mean it fails, only that nobody has checked. Each result is from the real binary, with one model per type, so it says nothing about other models. Dates are UTC.

## Results

| Platform | Run on | Chat | Image | Speech | Transcription | Video |
| --- | --- | --- | --- | --- | --- | --- |
| **macOS, Apple Silicon** | A developer's M4 Max, 64 GB, 2026-11-02 | **Works**, with a small model | Not re-run here | Not re-run here | Not re-run here | Not re-run here |
| **Linux x86-64, no GPU** | GitHub `ubuntu-latest`, 16 GB, no GPU, 2026-11-03 | **Works with a lowered memory reserve**, slowly. Refused at the default reserve | Refused | Kokoro **fails** to start. Others not run | Refused | Not run |
| **Linux x86-64, with a GPU** | Not run | Not run | Not run | Not run | Not run | Not run |
| **Linux ARM64** | GitHub `ubuntu-24.04-arm`, no GPU, 2026-11-03 | Refused | Refused | Not run | Refused | Not run |
| **macOS, Intel** | GitHub `macos-15-intel`, no GPU, 2026-11-03 | Refused | Refused | Not run | Refused | Not run |

"Refused" means `409 backend_not_selected`: LocalBase didn't choose a backend for that model there.

**macOS, Apple Silicon** is the daily-use platform. Earlier project runs covered image, speech, transcription and video on it, recorded in `go/DESIGN.md`. They weren't repeated for this page.

**Video:** from reading the code, it only targets macOS on Apple Silicon. No real video request was run on the other platforms.

## Linux x86-64 without a GPU, in detail

**These results are from one machine, one small model and six chats, at earlier code than the current integration tree.** Run 41560648170, on 2026-11-03. Head `d02ec1f1` wasn't itself run on a real runner.

- **Chat, reserve lowered to 5%, at least 1 GiB:** complete answers, on automatic, CPU-only and 0-GPU-layer placement, at about 7 tokens a second. A short reply took 28 to 73 seconds, mostly on reasoning before the answer. That setting isn't the default, and nobody has judged whether it's safe to keep on a machine in daily use.
- **Chat, default reserve (15%, at least 8 GiB):** refused on 16 GB. The model needed 7.7 GB, with 6.5 GB available after keeping 8 GB. That run answered `503 placement_does_not_fit`. Later code answers `422 model_too_large`, which has only run in tests.
- **Kokoro, chosen by hand with `onnx-cpu`:** `503 backend_startup_failed`. `local-base models backend` now rates it incompatible, because this version can't start it.
- **Image and transcription:** `409 backend_not_selected`. `local-base models backend` says their built-in backends need Vulkan, which this machine doesn't have.

Later code also starts smaller on CPU-only machines, which should need less memory. That hasn't run on a real runner, so none of the figures above reflect it.

## Linux ARM64 and macOS Intel

LocalBase has no packaged inference backends for these, only `ffmpeg`. Your own `llama-server` hasn't been tried on them, so nobody knows if chat works that way.

## Not run anywhere yet

- Vulkan on Linux, and CUDA on real NVIDIA hardware.
- Speech on Linux ARM64 or macOS Intel, and any speech model other than Kokoro on Linux x86-64.
- Your own `llama-server` on Linux ARM64 or macOS Intel.
- `local-base start` on a real Linux host.
