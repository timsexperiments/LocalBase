# Video support and limits

Video generation is experimental and restricted to Linux x64 with one NVIDIA GPU. The [catalog](../src/catalog.ts) defines each model's input requirements, fixed output dimensions, frame count, frame rate, generation settings, memory estimates, and job deadline. macOS and multi-GPU video execution are unsupported.

Text-to-video models produce silent video. Combining video with separately generated speech is client-side voiceover composition, not lip synchronization or native audio generation. Output quality and motion vary; concurrent inference and broader hardware support are not qualified.

Memory demand values are admission estimates, not hard allocation limits or performance guarantees. The native GPU budget derives from the admitted accelerator demand; pool reserves, pressure monitoring, and emergency eviction remain active. Unified-memory estimates do not enable unsupported targets.

The experimental Wan2.2 speech-to-video profile uses a portrait and supplied driving audio. Its AVI includes that audio, not a generated voice. Lip-sync quality and longer clips are not qualified; gateway qualification remains pending.
