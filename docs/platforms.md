# What runs where

This page says what has actually been run, and on what. "Not run" doesn't mean it fails, only that nobody has checked. Results below come from the real binary with real model downloads, on 2026-11-02 and 2026-11-03.

## Results

| Platform | Run on | Chat | Image | Speech | Transcription | Video |
| --- | --- | --- | --- | --- | --- | --- |
| **macOS, Apple Silicon** | A developer's M4 Max, 64 GB | **Works** | Not re-run here | Not re-run here | Not re-run here | Not re-run here |
| **Linux x86-64, no GPU** | GitHub `ubuntu-latest`, 16 GB, no GPU | **Fails** to start | Refused | **Fails** to start | Refused | Not supported |
| **Linux x86-64, with a GPU** | Not run | Not run | Not run | Not run | Not run | Not supported |
| **Linux ARM64** | GitHub `ubuntu-24.04-arm`, no GPU | Refused | Refused | Not reached | Refused | Not supported |
| **macOS, Intel** | GitHub `macos-15-intel`, no GPU | Refused | Refused | Not reached | Refused | Not supported |

"Refused" means `409 backend_not_selected`: no backend can run that model there, so LocalBase didn't choose one.

**macOS, Apple Silicon** is the daily-use platform. Earlier project runs covered image, speech, transcription and video on it, recorded in `go/DESIGN.md`. They weren't repeated for this page.

**Video** only targets macOS on Apple Silicon, from reading the code, so "Not supported" wasn't reached by a run.

## Linux x86-64 without a GPU, in detail

This is under investigation. The runtime itself works: `llama-server` run directly on that machine answered a chat in about 3 seconds.

- **Chat, default memory reserve:** `503 insufficient_memory`. The model asked for 7.7 GiB, with 14.6 GiB available and 8 GiB reserved. `local-base models show` reports the same model as fitting, using 5.1 GB.
- **Chat, reserve lowered to 5%, at least 1 GiB:** `503 placement_mismatch`, "the backend did not report how many layers it offloaded".
- **Kokoro, chosen by hand with `onnx-cpu`:** `503 backend_startup_failed`, "does not run speech models". `local-base models backend` lists it as supported.
- **Image and transcription:** `409 backend_not_selected`. Their built-in backends need Vulkan.

## Linux ARM64 and macOS Intel

LocalBase has no packaged inference backends for these, only `ffmpeg`. Chat may work with your own `llama-server`, but that hasn't been run.

## Not run anywhere yet

- Vulkan on Linux, and CUDA on real NVIDIA hardware.
- Speech with the default speech model on Linux, or on the two platforms above.
- Your own `llama-server` on Linux ARM64 or macOS Intel.
- `local-base start` on a real Linux host.
