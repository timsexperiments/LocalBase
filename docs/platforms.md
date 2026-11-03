# What runs where

This page says what has actually been run, and on what. "Not run" doesn't mean it fails, only that nobody has checked. Each result is from the real binary, with one model per type, so it says nothing about other models. Dates are UTC.

## Results

| Platform | Run on | Chat | Image | Speech | Transcription | Video |
| --- | --- | --- | --- | --- | --- | --- |
| **macOS, Apple Silicon** | A developer's M4 Max, 64 GB, 2026-11-02 | **Works**, with a small model | Not re-run here | Not re-run here | Not re-run here | Not re-run here |
| **Linux x86-64, no GPU** | GitHub `ubuntu-latest`, 16 GB, no GPU, 2026-11-03 | **Fails** to start | Refused | Kokoro **fails** to start. Others not run | Refused | Not run |
| **Linux x86-64, with a GPU** | Not run | Not run | Not run | Not run | Not run | Not run |
| **Linux ARM64** | GitHub `ubuntu-24.04-arm`, no GPU, 2026-11-03 | Refused | Refused | Not run | Refused | Not run |
| **macOS, Intel** | GitHub `macos-15-intel`, no GPU, 2026-11-03 | Refused | Refused | Not run | Refused | Not run |

"Refused" means `409 backend_not_selected`: LocalBase didn't choose a backend for that model there.

**macOS, Apple Silicon** is the daily-use platform. Earlier project runs covered image, speech, transcription and video on it, recorded in `go/DESIGN.md`. They weren't repeated for this page.

**Video:** from reading the code, it only targets macOS on Apple Silicon. No real video request was run on the other platforms.

## Linux x86-64 without a GPU, in detail

This is under investigation. The runtime can run on it: `llama-server`, started directly with the same small model and a 2,048-token context, answered a short chat in 2.5 to 4 seconds.

- **Chat, default memory reserve:** `503 insufficient_memory`. The model asked for 7.7 GiB, with 14.6 GiB available and 8 GiB reserved. `local-base models show` reports the same model as fitting, using 5.1 GB.
- **Chat, reserve lowered to 5%, at least 1 GiB:** `503 placement_mismatch`, "the backend did not report how many layers it offloaded".
- **Kokoro, chosen by hand with `onnx-cpu`:** `503 backend_startup_failed`, "does not run speech models". `local-base models backend` lists it as supported.
- **Image and transcription:** `409 backend_not_selected`. `local-base models backend` says their built-in backends need Vulkan, which this machine doesn't have.

## Linux ARM64 and macOS Intel

LocalBase has no packaged inference backends for these, only `ffmpeg`. Your own `llama-server` hasn't been tried on them, so nobody knows if chat works that way.

## Not run anywhere yet

- Vulkan on Linux, and CUDA on real NVIDIA hardware.
- Speech on Linux ARM64 or macOS Intel, and any speech model other than Kokoro on Linux x86-64.
- Your own `llama-server` on Linux ARM64 or macOS Intel.
- `local-base start` on a real Linux host.
