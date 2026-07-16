export type ModelKind = "llm" | "stt";

export type CommercialStatus = "open" | "conditional" | "prohibited";

export type ModelSpec = {
  modelId: string;
  kind: ModelKind;
  provider: string;
  family: string;
  version: string;
  size: string;
  quant: string;
  codingScore?: number;
  minVramGb: number;
  storageGb: number;
  source: string;
  downloadPath?: string;
  filename?: string;
  inputModalities: string[];
  outputModalities: string[];
  features: string[];
  commercialStatus: CommercialStatus;
  catch: string;
  notes: string;
};

export const CATALOG: readonly ModelSpec[] = [
  {
    modelId: "qwen2.5-coder-1.5b-instruct-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen2.5-Coder",
    version: "2.5",
    size: "1.5B",
    quant: "Q4_K_M",
    codingScore: 6,
    minVramGb: 2,
    storageGb: 1.2,
    source: "https://huggingface.co/Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF",
    downloadPath: "resolve/main/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
    filename: "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Ultra-fast autocomplete baseline and tab completion model."
  },
  {
    modelId: "qwen2.5-coder-3b-instruct-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen2.5-Coder",
    version: "2.5",
    size: "3B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 4,
    storageGb: 2.2,
    source: "https://huggingface.co/Qwen/Qwen2.5-Coder-3B-Instruct-GGUF",
    downloadPath: "resolve/main/qwen2.5-coder-3b-instruct-q4_k_m.gguf",
    filename: "qwen2.5-coder-3b-instruct-q4_k_m.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Fast coding baseline for low-VRAM GPUs."
  },
  {
    modelId: "qwen2.5-coder-7b-instruct-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen2.5-Coder",
    version: "2.5",
    size: "7B",
    quant: "Q4_K_M",
    codingScore: 9,
    minVramGb: 6,
    storageGb: 4.7,
    source: "https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct-GGUF",
    downloadPath: "resolve/main/qwen2.5-coder-7b-instruct-q4_k_m.gguf",
    filename: "qwen2.5-coder-7b-instruct-q4_k_m.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Great coding quality per watt; ideal default for a 12GB GPU."
  },
  {
    modelId: "qwen2.5-coder-14b-instruct-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen2.5-Coder",
    version: "2.5",
    size: "14B",
    quant: "Q4_K_M",
    codingScore: 10,
    minVramGb: 11,
    storageGb: 9.1,
    source: "https://huggingface.co/Qwen/Qwen2.5-Coder-14B-Instruct-GGUF",
    downloadPath: "resolve/main/qwen2.5-coder-14b-instruct-q4_k_m.gguf",
    filename: "qwen2.5-coder-14b-instruct-q4_k_m.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Top-end coding option that still fits on 12GB with careful context settings."
  },
  {
    modelId: "qwen2.5-coder-32b-instruct-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen2.5-Coder",
    version: "2.5",
    size: "32B",
    quant: "Q4_K_M",
    codingScore: 10,
    minVramGb: 20,
    storageGb: 20.3,
    source: "https://huggingface.co/Qwen/Qwen2.5-Coder-32B-Instruct-GGUF",
    downloadPath: "resolve/main/qwen2.5-coder-32b-instruct-q4_k_m.gguf",
    filename: "qwen2.5-coder-32b-instruct-q4_k_m.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "State-of-the-art local coding model. Perfect for unified memory setups."
  },
  {
    modelId: "llama-3.3-70b-instruct-q5_k_m",
    kind: "llm",
    provider: "Meta",
    family: "Llama",
    version: "3.3",
    size: "70B",
    quant: "Q5_K_M",
    codingScore: 8,
    minVramGb: 50,
    storageGb: 48.0,
    source: "https://huggingface.co/bartowski/Llama-3.3-70B-Instruct-GGUF",
    downloadPath: "resolve/main/Llama-3.3-70B-Instruct-Q5_K_M.gguf",
    filename: "Llama-3.3-70B-Instruct-Q5_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "multilingual"],
    commercialStatus: "conditional",
    catch: "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "High quality generalist model at 5-bit quantization. Tight fit on 64GB devices."
  },
  {
    modelId: "llama-3.2-1b-instruct-q4_k_m",
    kind: "llm",
    provider: "Meta",
    family: "Llama",
    version: "3.2",
    size: "1B",
    quant: "Q4_K_M",
    codingScore: 5,
    minVramGb: 2,
    storageGb: 1.0,
    source: "https://huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF",
    downloadPath: "resolve/main/Llama-3.2-1B-Instruct-Q4_K_M.gguf",
    filename: "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "lightweight"],
    commercialStatus: "conditional",
    catch: "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "Ultra-small Llama model for low-resource environments."
  },
  {
    modelId: "llama-3.2-3b-instruct-q4_k_m",
    kind: "llm",
    provider: "Meta",
    family: "Llama",
    version: "3.2",
    size: "3B",
    quant: "Q4_K_M",
    codingScore: 7,
    minVramGb: 4,
    storageGb: 2.0,
    source: "https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF",
    downloadPath: "resolve/main/Llama-3.2-3B-Instruct-Q4_K_M.gguf",
    filename: "Llama-3.2-3B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch: "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "Highly capable 3B generalist model."
  },
  {
    modelId: "llama-3.1-8b-instruct-q4_k_m",
    kind: "llm",
    provider: "Meta",
    family: "Llama",
    version: "3.1",
    size: "8B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 6,
    storageGb: 4.7,
    source: "https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF",
    downloadPath: "resolve/main/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
    filename: "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual", "reasoning"],
    commercialStatus: "conditional",
    catch: "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "One of the most popular open-source 8B models for general tasks and coding."
  },
  {
    modelId: "llama-3.3-70b-instruct-q4_k_m",
    kind: "llm",
    provider: "Meta",
    family: "Llama",
    version: "3.3",
    size: "70B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 40,
    storageGb: 42,
    source: "https://huggingface.co/bartowski/Llama-3.3-70B-Instruct-GGUF",
    downloadPath: "resolve/main/Llama-3.3-70B-Instruct-Q4_K_M.gguf",
    filename: "Llama-3.3-70B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "multilingual"],
    commercialStatus: "conditional",
    catch: "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "High quality generalist model for large GPU servers."
  },
  {
    modelId: "mistral-large-2-instruct-q4_k_m",
    kind: "llm",
    provider: "Mistral",
    family: "Mistral Large",
    version: "2",
    size: "123B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 72,
    storageGb: 72,
    source: "https://huggingface.co/bartowski/Mistral-Large-Instruct-2407-GGUF",
    downloadPath: "resolve/main/Mistral-Large-Instruct-2407-Q4_K_M.gguf",
    filename: "Mistral-Large-Instruct-2407-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "long-context"],
    commercialStatus: "conditional",
    catch: "Mistral Large 2 may require a paid commercial license for for-profit on-prem use.",
    notes: "Enterprise-scale option; not intended for 12GB VRAM nodes."
  },
  {
    modelId: "mixtral-8x22b-instruct-v0.1-q4_k_m",
    kind: "llm",
    provider: "Mistral",
    family: "Mixtral",
    version: "8x22B",
    size: "39B active",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 24,
    storageGb: 25,
    source: "https://huggingface.co/bartowski/Mixtral-8x22B-Instruct-v0.1-GGUF",
    downloadPath: "resolve/main/Mixtral-8x22B-Instruct-v0.1-Q4_K_M.gguf",
    filename: "Mixtral-8x22B-Instruct-v0.1-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["mixture-of-experts", "tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch: "Check Mistral model license terms for commercial use by model variant.",
    notes: "Strong MoE quality with high memory requirements."
  },
  {
    modelId: "deepseek-r1-distill-qwen-14b-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-R1",
    version: "R1",
    size: "14B",
    quant: "Q4_K_M",
    codingScore: 9,
    minVramGb: 11,
    storageGb: 9,
    source: "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Qwen-14B-GGUF",
    downloadPath: "resolve/main/DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf",
    filename: "DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No revenue caps.",
    notes: "Reasoning-focused model with strong coding quality in quantized form."
  },
  {
    modelId: "deepseek-coder-6.7b-instruct-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-Coder",
    version: "1",
    size: "6.7B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 6,
    storageGb: 4.8,
    source: "https://huggingface.co/TheBloke/deepseek-coder-6.7B-Instruct-GGUF",
    downloadPath: "resolve/main/deepseek-coder-6.7b-instruct-Q4_K_M.gguf",
    filename: "deepseek-coder-6.7b-instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes: "Highly capable code model from the first-gen DeepSeek coder series."
  },
  {
    modelId: "deepseek-coder-v2-lite-instruct-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-Coder-V2",
    version: "2",
    size: "16B",
    quant: "Q4_K_M",
    codingScore: 9.5,
    minVramGb: 12,
    storageGb: 11.2,
    source: "https://huggingface.co/bartowski/DeepSeek-Coder-V2-Lite-Instruct-GGUF",
    downloadPath: "resolve/main/DeepSeek-Coder-V2-Lite-Instruct-Q4_K_M.gguf",
    filename: "DeepSeek-Coder-V2-Lite-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["mixture-of-experts", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes: "State-of-the-art MoE coding model with 16B total parameters and 2.4B active."
  },
  {
    modelId: "deepseek-coder-33b-instruct-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-Coder",
    version: "1",
    size: "33B",
    quant: "Q4_K_M",
    codingScore: 9,
    minVramGb: 22,
    storageGb: 21.0,
    source: "https://huggingface.co/TheBloke/deepseek-coder-33B-Instruct-GGUF",
    downloadPath: "resolve/main/deepseek-coder-33b-instruct-Q4_K_M.gguf",
    filename: "deepseek-coder-33b-instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes: "Top-tier 33B coding model, excellent balance between performance and footprint."
  },
  {
    modelId: "gemma-3-1b-it-q4_k_m",
    kind: "llm",
    provider: "Google",
    family: "Gemma",
    version: "3",
    size: "1B",
    quant: "Q4_K_M",
    codingScore: 5,
    minVramGb: 2,
    storageGb: 0.9,
    source: "https://huggingface.co/bartowski/google_gemma-3-1b-it-GGUF",
    downloadPath: "resolve/main/gemma-3-1b-it-Q4_K_M.gguf",
    filename: "gemma-3-1b-it-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "open",
    catch: "Gemma terms allow commercial use; cannot use to improve other models.",
    notes: "Smallest Gemma 3 variant, fast and lightweight."
  },
  {
    modelId: "gemma-3-4b-it-q4_k_m",
    kind: "llm",
    provider: "Google",
    family: "Gemma",
    version: "3",
    size: "4B",
    quant: "Q4_K_M",
    codingScore: 6,
    minVramGb: 4,
    storageGb: 2.8,
    source: "https://huggingface.co/bartowski/google_gemma-3-4b-it-GGUF",
    downloadPath: "resolve/main/gemma-3-4b-it-Q4_K_M.gguf",
    filename: "gemma-3-4b-it-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "open",
    catch: "Gemma terms allow commercial use; cannot use to improve other models.",
    notes: "Highly capable 4B generalist and reasoning model."
  },
  {
    modelId: "gemma-3-12b-it-q4_k_m",
    kind: "llm",
    provider: "Google",
    family: "Gemma",
    version: "3",
    size: "12B",
    quant: "Q4_K_M",
    codingScore: 7,
    minVramGb: 10,
    storageGb: 8,
    source: "https://huggingface.co/bartowski/google_gemma-3-12b-it-GGUF",
    downloadPath: "resolve/main/gemma-3-12b-it-Q4_K_M.gguf",
    filename: "gemma-3-12b-it-Q4_K_M.gguf",
    inputModalities: ["text", "image"],
    outputModalities: ["text"],
    features: ["vision", "tool-calling", "reasoning"],
    commercialStatus: "open",
    catch: "Gemma terms allow commercial use; cannot use to improve other models.",
    notes: "Multimodal-capable family with permissive usage terms."
  },
  {
    modelId: "gemma-3-27b-it-q4_k_m",
    kind: "llm",
    provider: "Google",
    family: "Gemma",
    version: "3",
    size: "27B",
    quant: "Q4_K_M",
    codingScore: 9,
    minVramGb: 18,
    storageGb: 16.5,
    source: "https://huggingface.co/bartowski/google_gemma-3-27b-it-GGUF",
    downloadPath: "resolve/main/gemma-3-27b-it-Q4_K_M.gguf",
    filename: "gemma-3-27b-it-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "multilingual"],
    commercialStatus: "open",
    catch: "Gemma terms allow commercial use; cannot use to improve other models.",
    notes: "Top-tier 27B model; matches larger models in reasoning quality."
  },
  {
    modelId: "phi-4-q4_k_m",
    kind: "llm",
    provider: "Microsoft",
    family: "Phi",
    version: "4",
    size: "14B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 11,
    storageGb: 9,
    source: "https://huggingface.co/bartowski/microsoft_Phi-4-GGUF",
    downloadPath: "resolve/main/phi-4-Q4_K_M.gguf",
    filename: "phi-4-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "Compact reasoning model with strong quality density."
  },
  {
    modelId: "phi-3.5-mini-instruct-q4_k_m",
    kind: "llm",
    provider: "Microsoft",
    family: "Phi",
    version: "3.5",
    size: "3.8B",
    quant: "Q4_K_M",
    codingScore: 7,
    minVramGb: 3,
    storageGb: 2.2,
    source: "https://huggingface.co/bartowski/Phi-3.5-mini-instruct-GGUF",
    downloadPath: "resolve/main/Phi-3.5-mini-instruct-Q4_K_M.gguf",
    filename: "Phi-3.5-mini-instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "Extremely lightweight 3.8B model with high reasoning ability."
  },
  {
    modelId: "phi-3.5-moe-instruct-q4_k_m",
    kind: "llm",
    provider: "Microsoft",
    family: "Phi",
    version: "3.5",
    size: "42B",
    quant: "Q4_K_M",
    codingScore: 8.5,
    minVramGb: 28,
    storageGb: 23.0,
    source: "https://huggingface.co/bartowski/Phi-3.5-MoE-instruct-GGUF",
    downloadPath: "resolve/main/Phi-3.5-MoE-instruct-Q4_K_M.gguf",
    filename: "Phi-3.5-MoE-instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["mixture-of-experts", "reasoning", "multilingual"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "Microsoft MoE release; high performance with 6.6B active parameters."
  },
  {
    modelId: "gpt-oss-20b-q4_k_m",
    kind: "llm",
    provider: "OpenAI",
    family: "GPT-OSS",
    version: "1",
    size: "20B",
    quant: "Q4_K_M",
    codingScore: 9.5,
    minVramGb: 16,
    storageGb: 14.0,
    source: "https://huggingface.co/bartowski/openai_gpt-oss-20b-GGUF",
    downloadPath: "resolve/main/openai_gpt-oss-20b-Q4_K_M.gguf",
    filename: "openai_gpt-oss-20b-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "OpenAI's open-weight reasoning model, optimized for edge devices."
  },
  {
    modelId: "gpt-oss-120b-q4_k_m",
    kind: "llm",
    provider: "OpenAI",
    family: "GPT-OSS",
    version: "1",
    size: "120B",
    quant: "Q4_K_M",
    codingScore: 10,
    minVramGb: 72,
    storageGb: 75.0,
    source: "https://huggingface.co/bartowski/openai_gpt-oss-120b-GGUF",
    downloadPath: "resolve/main/openai_gpt-oss-120b-Q4_K_M.gguf",
    filename: "openai_gpt-oss-120b-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "long-context"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "OpenAI's large open-weight reasoning model, high computational footprint."
  },
  {
    modelId: "falcon-2-11b-instruct-q4_k_m",
    kind: "llm",
    provider: "TII",
    family: "Falcon",
    version: "2",
    size: "11B",
    quant: "Q4_K_M",
    codingScore: 7,
    minVramGb: 9,
    storageGb: 7,
    source: "https://huggingface.co/bartowski/Falcon3-10B-Instruct-GGUF",
    downloadPath: "resolve/main/Falcon3-10B-Instruct-Q4_K_M.gguf",
    filename: "Falcon3-10B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "General-purpose LLM alternative in permissive license family."
  },
  {
    modelId: "grok-1-q4_k_m",
    kind: "llm",
    provider: "xAI",
    family: "Grok",
    version: "1",
    size: "314B",
    quant: "Q4_K_M",
    codingScore: 7,
    minVramGb: 120,
    storageGb: 190,
    source: "https://huggingface.co/bartowski/grok-1-GGUF",
    downloadPath: "resolve/main/grok-1-Q4_K_M.gguf",
    filename: "grok-1-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "long-context"],
    commercialStatus: "open",
    catch: "Apache 2.0 open-weight release.",
    notes: "Research/large-cluster oriented footprint."
  },
  {
    modelId: "deepseek-r1-distill-qwen-32b-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-R1-Distill",
    version: "Qwen-32B",
    size: "32B",
    quant: "Q4_K_M",
    codingScore: 9.5,
    minVramGb: 22,
    storageGb: 20.3,
    source: "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Qwen-32B-GGUF",
    filename: "DeepSeek-R1-Distill-Qwen-32B-Q4_K_M.gguf",
    downloadPath: "resolve/main/DeepSeek-R1-Distill-Qwen-32B-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "DeepSeek-R1 reasoning model distilled on Qwen-32B. Superb coding and logic."
  },
  {
    modelId: "deepseek-r1-distill-llama-8b-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-R1-Distill",
    version: "Llama-8B",
    size: "8B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 6,
    storageGb: 4.7,
    source: "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF",
    filename: "DeepSeek-R1-Distill-Llama-8B-Q4_K_M.gguf",
    downloadPath: "resolve/main/DeepSeek-R1-Distill-Llama-8B-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "DeepSeek-R1 reasoning model distilled on Llama-8B. Great for laptop runs."
  },
  {
    modelId: "deepseek-r1-distill-llama-70b-q4_k_m",
    kind: "llm",
    provider: "DeepSeek",
    family: "DeepSeek-R1-Distill",
    version: "Llama-70B",
    size: "70B",
    quant: "Q4_K_M",
    codingScore: 9.5,
    minVramGb: 40,
    storageGb: 42.0,
    source: "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Llama-70B-GGUF",
    filename: "DeepSeek-R1-Distill-Llama-70B-Q4_K_M.gguf",
    downloadPath: "resolve/main/DeepSeek-R1-Distill-Llama-70B-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "DeepSeek-R1 reasoning model distilled on Llama-3-70B. High reasoning quality."
  },
  {
    modelId: "qwen2.5-72b-instruct-q4_k_m",
    kind: "llm",
    provider: "Alibaba",
    family: "Qwen",
    version: "2.5",
    size: "72B",
    quant: "Q4_K_M",
    codingScore: 10,
    minVramGb: 42,
    storageGb: 47.0,
    source: "https://huggingface.co/bartowski/Qwen2.5-72B-Instruct-GGUF",
    filename: "Qwen2.5-72B-Instruct-Q4_K_M.gguf",
    downloadPath: "resolve/main/Qwen2.5-72B-Instruct-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Top-tier 72B reasoning and coding model. Superb logic, math, and code generation."
  },
  {
    modelId: "codestral-22b-instruct-q4_k_m",
    kind: "llm",
    provider: "Mistral",
    family: "Codestral",
    version: "22B-v0.1",
    size: "22B",
    quant: "Q4_K_M",
    codingScore: 9,
    minVramGb: 18,
    storageGb: 14.0,
    source: "https://huggingface.co/bartowski/Codestral-22B-v0.1-GGUF",
    filename: "Codestral-22B-v0.1-Q4_K_M.gguf",
    downloadPath: "resolve/main/Codestral-22B-v0.1-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["code-generation", "code-editing"],
    commercialStatus: "conditional",
    catch: "Mistral commercial terms apply for non-research use.",
    notes: "Mistral's dedicated coding model. Strong FIM capability."
  },
  {
    modelId: "mistral-nemo-12b-instruct-q4_k_m",
    kind: "llm",
    provider: "Mistral/NVIDIA",
    family: "Mistral",
    version: "Nemo",
    size: "12B",
    quant: "Q4_K_M",
    codingScore: 8,
    minVramGb: 10,
    storageGb: 7.5,
    source: "https://huggingface.co/bartowski/Mistral-Nemo-Instruct-2407-GGUF",
    filename: "Mistral-Nemo-Instruct-2407-Q4_K_M.gguf",
    downloadPath: "resolve/main/Mistral-Nemo-Instruct-2407-Q4_K_M.gguf",
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "Excellent 12B model with large 128k context window."
  },
  {
    modelId: "whisper-large-v3-turbo",
    kind: "stt",
    provider: "OpenAI/ggml",
    family: "Whisper",
    version: "large-v3-turbo",
    size: "large",
    quant: "Q5_0",
    minVramGb: 4,
    storageGb: 1.7,
    source: "https://huggingface.co/ggerganov/whisper.cpp",
    downloadPath: "resolve/main/ggml-large-v3-turbo.bin",
    filename: "ggml-large-v3-turbo.bin",
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text", "translation"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Best quality Whisper family model for local deployment."
  },
  {
    modelId: "whisper-tiny-en-q8_0",
    kind: "stt",
    provider: "OpenAI/ggml",
    family: "Whisper",
    version: "tiny.en",
    size: "tiny.en",
    quant: "Q8_0",
    minVramGb: 0,
    storageGb: 0.08,
    source: "https://huggingface.co/ggerganov/whisper.cpp",
    downloadPath: "resolve/main/ggml-tiny.en-q8_0.bin",
    filename: "ggml-tiny.en-q8_0.bin",
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Ultra-fast English STT with lower accuracy."
  },
  {
    modelId: "whisper-base-q8_0",
    kind: "stt",
    provider: "OpenAI/ggml",
    family: "Whisper",
    version: "base",
    size: "base",
    quant: "Q8_0",
    minVramGb: 0,
    storageGb: 0.15,
    source: "https://huggingface.co/ggerganov/whisper.cpp",
    downloadPath: "resolve/main/ggml-base-q8_0.bin",
    filename: "ggml-base-q8_0.bin",
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Good default STT latency/quality tradeoff."
  }
];

export function byId(modelId: string): ModelSpec | undefined {
  return CATALOG.find((model) => model.modelId === modelId);
}

export function listModels(kind?: ModelKind): ModelSpec[] {
  return kind ? CATALOG.filter((m) => m.kind === kind) : [...CATALOG];
}

export function recommendedForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter((m) => m.kind === "llm" && m.minVramGb <= vramGb).sort(
    (a, b) => (b.codingScore ?? 0) - (a.codingScore ?? 0) || b.minVramGb - a.minVramGb
  );
}

export function recommendedSttForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter((m) => m.kind === "stt" && m.minVramGb <= vramGb).sort((a, b) => a.storageGb - b.storageGb);
}

export type MemoryFitStatus = "perfect" | "tight" | "insufficient";

export type MemoryFitEvaluation = {
  status: MemoryFitStatus;
  minVramGb: number;
  requiredVramGb: number;
  systemVramGb: number;
  headroomGb: number;
  message: string;
};

export function evaluateModelFit(model: ModelSpec, systemVramGb: number): MemoryFitEvaluation {
  const minVramGb = model.minVramGb;
  let requiredVramGb = minVramGb;
  
  const p = parseFloat(model.size);
  if (!isNaN(p)) {
    let b = 4;
    const q = model.quant.toLowerCase();
    if (q.includes("q4")) b = 4;
    else if (q.includes("q5")) b = 5;
    else if (q.includes("q8")) b = 8;
    else if (q.includes("fp16")) b = 16;
    
    const computed = (p * b / 8) * 1.2;
    requiredVramGb = Math.max(computed, minVramGb);
  }

  const headroomGb = systemVramGb - minVramGb;
  let status: MemoryFitStatus = "perfect";
  let message = "";

  if (systemVramGb < minVramGb) {
    status = "insufficient";
    message = `Requires ${minVramGb}GB, you have ${systemVramGb}GB`;
  } else if (headroomGb < 4 || (systemVramGb > 0 && minVramGb / systemVramGb > 0.75)) {
    status = "tight";
    message = `Leaves ${headroomGb.toFixed(1)}GB headroom`;
  } else {
    status = "perfect";
    message = `Leaves ${headroomGb.toFixed(1)}GB headroom`;
  }

  return {
    status,
    minVramGb,
    requiredVramGb,
    systemVramGb,
    headroomGb,
    message
  };
}

export function calculateMaxSafeContextSize(model: ModelSpec, systemVramGb: number): number {
  const p = parseFloat(model.size);
  if (isNaN(p)) {
    return 8192;
  }

  const w = model.storageGb;

  const availableGb = (systemVramGb - w) * 0.8 - 4.0;
  if (availableGb <= 0) {
    return 4096;
  }

  const calculatedC = availableGb / (p * 8e-6);

  const standardBlocks = [4096, 8192, 16384, 24576, 32768, 49152, 65536, 98304, 131072];
  
  let recommended = 4096;
  for (const block of standardBlocks) {
    if (block <= calculatedC) {
      recommended = block;
    }
  }

  const isOlderModel = model.family.toLowerCase().includes("phi-3") || model.modelId.includes("llama-3.2-") || model.modelId.includes("llama-3-8b");
  const maxModelCtx = isOlderModel ? 8192 : 131072;

  return Math.min(recommended, maxModelCtx);
}

