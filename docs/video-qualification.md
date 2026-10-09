# Video support and limits

Video generation is experimental. Accepted runtime targets are declared per profile in the [catalog](../src/catalog.ts): the Wan T2V and FastWan profiles support macOS ARM64 as well as Linux x64 with one NVIDIA GPU. Wan 2.2 S2V is restricted to Linux x64 with one NVIDIA GPU. A supported runtime target does not mean that a profile has been hardware qualified.

Text-to-video models produce silent video. Combining video with separately generated speech is client-side voiceover composition, not lip synchronization or native audio generation. Output quality and motion vary; concurrent inference and broader hardware support are not qualified.

Memory demand values are admission estimates, not hard allocation limits or performance guarantees. The native GPU budget derives from the admitted accelerator demand; pool reserves, pressure monitoring, and emergency eviction remain active. Unified-memory estimates do not enable unsupported targets.

The Wan2.2 speech-to-video profile is experimental and unqualified. It uses a portrait and supplied driving audio; its AVI includes that audio, not a generated voice. Its 9 GiB accelerator and 32 GiB host admission reservations are estimates. Re-measure both in a hardware run using the pinned runtime before changing the profile to qualified. Linux x64 with one NVIDIA GPU remains required to qualify request completion within the 30-minute deadline, AVI decode and audio correspondence, post-run artifact/runtime hashes, job deletion, and owned-process cleanup. Lip-sync quality, longer clips, concurrent inference, and broader hardware support are not qualified.

The profile is hidden by default. Enable it in declarative configuration with `[models] allowExperimental = true`; configuration, installation, and activation still enforce the supported runtime target.
