# Video support and limits

Video generation is experimental. Wan 2.1 T2V 1.3B Q8_0 and FastWan 2.2 TI2V 5B Q6_K support macOS ARM64 unified memory and Linux x64 with one NVIDIA GPU. The [catalog](../src/catalog.ts) defines each model's input requirements, fixed output dimensions, frame count, frame rate, generation settings, memory estimates, and job deadline. macOS x64, multi-GPU video execution, and models without a matching target remain unsupported.

Text-to-video models produce silent video. Combining video with separately generated speech is client-side voiceover composition, not lip synchronization or native audio generation. Output quality and motion vary; concurrent inference and broader hardware support are not qualified.

Memory demand values are admission estimates, not hard allocation limits or performance guarantees. The native GPU budget derives from the admitted accelerator demand; pool reserves, pressure monitoring, and emergency eviction remain active. The unified-memory estimate applies only to catalog targets that explicitly allow it.
