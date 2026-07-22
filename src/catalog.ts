import { existsSync, statSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";

export const ModelKindSchema = z.enum(["llm", "stt", "image"]);
export type ModelKind = z.infer<typeof ModelKindSchema>;

export const CommercialStatusSchema = z.enum([
  "open",
  "conditional",
  "prohibited",
]);
export type CommercialStatus = z.infer<typeof CommercialStatusSchema>;

export const ModelArtifactSchema = z
  .object({
    sourcePath: z.string().min(1),
    filename: z.string().min(1),
    expectedSizeBytes: z.number().positive().optional(),
    sha256: z
      .string()
      .regex(/^[a-fA-F0-9]{64}$/)
      .optional(),
    role: z.enum(["primary", "supplementary"]),
  })
  .superRefine((artifact, ctx) => {
    if (
      (artifact.expectedSizeBytes === undefined) !==
      (artifact.sha256 === undefined)
    ) {
      ctx.addIssue({
        code: "custom",
        message: "expectedSizeBytes and sha256 must be provided together",
      });
    }
  });
export type ModelArtifact = z.infer<typeof ModelArtifactSchema>;

export const ModelSpecSchema = z
  .object({
    modelId: z.string().min(1),
    kind: ModelKindSchema,
    provider: z.string().min(1),
    family: z.string().min(1),
    version: z.string().min(1),
    size: z.string().min(1),
    quant: z.string().min(1),
    codingScore: z.number().optional(),
    minVramGb: z.number().nonnegative(),
    storageGb: z.number().positive(),
    source: z.string().url(),
    repositoryRevision: z.string().min(1),
    artifacts: z.array(ModelArtifactSchema).min(1),
    inputModalities: z.array(z.string().min(1)),
    outputModalities: z.array(z.string().min(1)),
    features: z.array(z.string().min(1)),
    commercialStatus: CommercialStatusSchema,
    catch: z.string(),
    notes: z.string(),
  })
  .superRefine((model, ctx) => {
    const filenames = new Set<string>();
    let primaryCount = 0;

    for (const [index, artifact] of model.artifacts.entries()) {
      if (filenames.has(artifact.filename)) {
        ctx.addIssue({
          code: "custom",
          message: "artifact filenames must be unique",
          path: ["artifacts", index, "filename"],
        });
      }
      filenames.add(artifact.filename);
      if (artifact.role === "primary") primaryCount += 1;
    }

    if (primaryCount !== 1) {
      ctx.addIssue({
        code: "custom",
        message: "models must have exactly one primary artifact",
        path: ["artifacts"],
      });
    }
  });
export type ModelSpec = z.infer<typeof ModelSpecSchema>;

export type CatalogInstallationState = {
  complete: boolean;
  primaryPath: string;
};

export const CatalogSchema = z.array(ModelSpecSchema);

export function validateCatalog(catalog: unknown): ModelSpec[] {
  return CatalogSchema.parse(catalog);
}

export const CATALOG: readonly ModelSpec[] = validateCatalog([
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
        filename: "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Ultra-fast autocomplete baseline and tab completion model.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "qwen2.5-coder-3b-instruct-q4_k_m.gguf",
        filename: "qwen2.5-coder-3b-instruct-q4_k_m.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Fast coding baseline for low-VRAM GPUs.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "qwen2.5-coder-7b-instruct-q4_k_m.gguf",
        filename: "qwen2.5-coder-7b-instruct-q4_k_m.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes: "Great coding quality per watt; ideal default for a 12GB GPU.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "qwen2.5-coder-14b-instruct-q4_k_m.gguf",
        filename: "qwen2.5-coder-14b-instruct-q4_k_m.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes:
      "Top-end coding option that still fits on 12GB with careful context settings.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "qwen2.5-coder-32b-instruct-q4_k_m.gguf",
        filename: "qwen2.5-coder-32b-instruct-q4_k_m.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes:
      "State-of-the-art local coding model. Perfect for unified memory setups.",
  },
  {
    modelId: "qwen3-coder-next-q4_k_m",
    kind: "llm",
    provider: "Qwen",
    family: "Qwen3-Coder",
    version: "Next",
    size: "80B-A3B",
    quant: "Q4_K_M",
    codingScore: 10,
    minVramGb: 64,
    storageGb: 48.4,
    source: "https://huggingface.co/Qwen/Qwen3-Coder-Next-GGUF",
    repositoryRevision: "b82fb7382639d97b38fa7672e526c760c2fb358e",
    artifacts: [
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00001-of-00004.gguf",
        expectedSizeBytes: 15524827040,
        sha256:
          "6bcfc9f9c37901eeb92172e2ab871224dab36a453d263bcb2547f737409534da",
        role: "primary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00002-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00002-of-00004.gguf",
        expectedSizeBytes: 14872168352,
        sha256:
          "817def0691ee9d08bf3dc4444be7aed29c9e52091e8fa9d97901ce7e7f6f01d3",
        role: "supplementary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00003-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00003-of-00004.gguf",
        expectedSizeBytes: 14503294496,
        sha256:
          "23aa634d47dca9b4ca3ea249384e6f01951b24c83cdc076f37f6f43d6c99883f",
        role: "supplementary",
      },
      {
        sourcePath:
          "Qwen3-Coder-Next-Q4_K_M/Qwen3-Coder-Next-Q4_K_M-00004-of-00004.gguf",
        filename: "Qwen3-Coder-Next-Q4_K_M-00004-of-00004.gguf",
        expectedSizeBytes: 3510702144,
        sha256:
          "249c768cc5f130dc731567d6edcbdacc48e14dec9e02c5dbe2b2185d2c5bdb2b",
        role: "supplementary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["agentic-coding", "tool-calling", "long-context"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "Qwen's sparse next-generation coding model for agentic workflows.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
        filename: "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "lightweight"],
    commercialStatus: "conditional",
    catch:
      "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "Ultra-small Llama model for low-resource environments.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Llama-3.2-3B-Instruct-Q4_K_M.gguf",
        filename: "Llama-3.2-3B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch:
      "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "Highly capable 3B generalist model.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
        filename: "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual", "reasoning"],
    commercialStatus: "conditional",
    catch:
      "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes:
      "One of the most popular open-source 8B models for general tasks and coding.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Llama-3.3-70B-Instruct-Q4_K_M.gguf",
        filename: "Llama-3.3-70B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "multilingual"],
    commercialStatus: "conditional",
    catch:
      "Free unless your app has >700M monthly active users; then request a Meta license.",
    notes: "High quality generalist model for large GPU servers.",
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
    source:
      "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Qwen-14B-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf",
        filename: "DeepSeek-R1-Distill-Qwen-14B-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No revenue caps.",
    notes:
      "Reasoning-focused model with strong coding quality in quantized form.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "deepseek-coder-6.7b-instruct.Q4_K_M.gguf",
        filename: "deepseek-coder-6.7b-instruct.Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes:
      "Highly capable code model from the first-gen DeepSeek coder series.",
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
    source:
      "https://huggingface.co/bartowski/DeepSeek-Coder-V2-Lite-Instruct-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DeepSeek-Coder-V2-Lite-Instruct-Q4_K_M.gguf",
        filename: "DeepSeek-Coder-V2-Lite-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["mixture-of-experts", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes:
      "State-of-the-art MoE coding model with 16B total parameters and 2.4B active.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "deepseek-coder-33b-instruct.Q4_K_M.gguf",
        filename: "deepseek-coder-33b-instruct.Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Permissive License (DeepSeek License), allows commercial use.",
    notes:
      "Top-tier 33B coding model, excellent balance between performance and footprint.",
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
    source: "https://huggingface.co/bartowski/gemma-3-1b-it-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "gemma-3-1b-it-Q4_K_M.gguf",
        filename: "gemma-3-1b-it-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch:
      "Gemma terms allow commercial use. Gated model; requires Hugging Face token configuration (local-base configure --hf-token) to install.",
    notes: "Smallest Gemma 3 variant, fast and lightweight.",
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
    source: "https://huggingface.co/bartowski/gemma-3-4b-it-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "gemma-3-4b-it-Q4_K_M.gguf",
        filename: "gemma-3-4b-it-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch:
      "Gemma terms allow commercial use. Gated model; requires Hugging Face token configuration (local-base configure --hf-token) to install.",
    notes: "Highly capable 4B generalist and reasoning model.",
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
    source: "https://huggingface.co/bartowski/gemma-3-12b-it-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "gemma-3-12b-it-Q4_K_M.gguf",
        filename: "gemma-3-12b-it-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text", "image"],
    outputModalities: ["text"],
    features: ["vision", "tool-calling", "reasoning"],
    commercialStatus: "conditional",
    catch:
      "Gemma terms allow commercial use. Gated model; requires Hugging Face token configuration (local-base configure --hf-token) to install.",
    notes: "Multimodal-capable family with permissive usage terms.",
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
    source: "https://huggingface.co/bartowski/gemma-3-27b-it-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "gemma-3-27b-it-Q4_K_M.gguf",
        filename: "gemma-3-27b-it-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "reasoning", "multilingual"],
    commercialStatus: "conditional",
    catch:
      "Gemma terms allow commercial use. Gated model; requires Hugging Face token configuration (local-base configure --hf-token) to install.",
    notes: "Top-tier 27B model; matches larger models in reasoning quality.",
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
    source: "https://huggingface.co/bartowski/phi-4-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "phi-4-Q4_K_M.gguf",
        filename: "phi-4-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "Compact reasoning model with strong quality density.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Phi-3.5-mini-instruct-Q4_K_M.gguf",
        filename: "Phi-3.5-mini-instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes: "Extremely lightweight 3.8B model with high reasoning ability.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Phi-3.5-MoE-instruct-Q4_K_M.gguf",
        filename: "Phi-3.5-MoE-instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["mixture-of-experts", "reasoning", "multilingual"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes:
      "Microsoft MoE release; high performance with 6.6B active parameters.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "openai_gpt-oss-20b-Q4_K_M.gguf",
        filename: "openai_gpt-oss-20b-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "OpenAI's open-weight reasoning model, optimized for edge devices.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Falcon3-10B-Instruct-Q4_K_M.gguf",
        filename: "Falcon3-10B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "General-purpose LLM alternative in permissive license family.",
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
    source:
      "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Qwen-32B-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DeepSeek-R1-Distill-Qwen-32B-Q4_K_M.gguf",
        filename: "DeepSeek-R1-Distill-Qwen-32B-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes:
      "DeepSeek-R1 reasoning model distilled on Qwen-32B. Superb coding and logic.",
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
    source:
      "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Llama-8B-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DeepSeek-R1-Distill-Llama-8B-Q4_K_M.gguf",
        filename: "DeepSeek-R1-Distill-Llama-8B-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes:
      "DeepSeek-R1 reasoning model distilled on Llama-8B. Great for laptop runs.",
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
    source:
      "https://huggingface.co/bartowski/DeepSeek-R1-Distill-Llama-70B-GGUF",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DeepSeek-R1-Distill-Llama-70B-Q4_K_M.gguf",
        filename: "DeepSeek-R1-Distill-Llama-70B-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["reasoning", "tool-calling", "code-generation"],
    commercialStatus: "open",
    catch: "MIT License. No restrictions.",
    notes:
      "DeepSeek-R1 reasoning model distilled on Llama-3-70B. High reasoning quality.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Qwen2.5-72B-Instruct-Q4_K_M.gguf",
        filename: "Qwen2.5-72B-Instruct-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "code-generation", "code-editing"],
    commercialStatus: "open",
    catch: "Alibaba-specific license, generally permissive like Apache 2.0.",
    notes:
      "Top-tier 72B reasoning and coding model. Superb logic, math, and code generation.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Codestral-22B-v0.1-Q4_K_M.gguf",
        filename: "Codestral-22B-v0.1-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["code-generation", "code-editing"],
    commercialStatus: "conditional",
    catch: "Mistral commercial terms apply for non-research use.",
    notes: "Mistral's dedicated coding model. Strong FIM capability.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Mistral-Nemo-Instruct-2407-Q4_K_M.gguf",
        filename: "Mistral-Nemo-Instruct-2407-Q4_K_M.gguf",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["text"],
    features: ["tool-calling", "multilingual"],
    commercialStatus: "open",
    catch: "Apache 2.0.",
    notes: "Excellent 12B model with large 128k context window.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "ggml-large-v3-turbo.bin",
        filename: "ggml-large-v3-turbo.bin",
        role: "primary",
      },
    ],
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text", "translation"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Best quality Whisper family model for local deployment.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "ggml-tiny.en-q8_0.bin",
        filename: "ggml-tiny.en-q8_0.bin",
        role: "primary",
      },
    ],
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Ultra-fast English STT with lower accuracy.",
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
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "ggml-base-q8_0.bin",
        filename: "ggml-base-q8_0.bin",
        role: "primary",
      },
    ],
    inputModalities: ["audio"],
    outputModalities: ["text"],
    features: ["speech-to-text"],
    commercialStatus: "open",
    catch: "MIT License.",
    notes: "Good default STT latency/quality tradeoff.",
  },
  {
    modelId: "stable-diffusion-v1-5",
    kind: "image",
    provider: "RunwayML",
    family: "Stable-Diffusion",
    version: "1.5",
    size: "4.3GB",
    quant: "F16",
    minVramGb: 4,
    storageGb: 4.27,
    source:
      "https://huggingface.co/stable-diffusion-v1-5/stable-diffusion-v1-5",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "v1-5-pruned-emaonly.safetensors",
        filename: "v1-5-pruned-emaonly.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "CreativeML Open RAIL-M license.",
    notes:
      "Extremely lightweight baseline model. Perfect for low VRAM systems (under 8GB) and fast prototyping. Generates 512x512 images.",
  },
  {
    modelId: "dreamshaper-v8",
    kind: "image",
    provider: "Lykon",
    family: "Stable-Diffusion",
    version: "8.0",
    size: "2.0GB",
    quant: "F16",
    minVramGb: 4,
    storageGb: 1.98,
    source: "https://huggingface.co/Lykon/DreamShaper",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "DreamShaper_8_pruned.safetensors",
        filename: "DreamShaper_8_pruned.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "CreativeML Open RAIL-M license.",
    notes:
      "Highly optimized community model for digital art, anime, and portraiture. Extremely fast and lightweight. Requires 4GB+ VRAM.",
  },
  {
    modelId: "sdxl-turbo",
    kind: "image",
    provider: "StabilityAI",
    family: "Stable-Diffusion-XL",
    version: "Turbo-1.0",
    size: "13.9GB",
    quant: "F16",
    minVramGb: 8,
    storageGb: 13.9,
    source: "https://huggingface.co/stabilityai/sdxl-turbo",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "sd_xl_turbo_1.0_fp16.safetensors",
        filename: "sd_xl_turbo_1.0_fp16.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "Stability AI Community License.",
    notes:
      "High-speed 1-step real-time generation model. Excellent for fast interactive feedback cycles. Requires 8GB+ VRAM.",
  },
  {
    modelId: "sdxl-base-1.0",
    kind: "image",
    provider: "StabilityAI",
    family: "Stable-Diffusion-XL",
    version: "1.0",
    size: "6.5GB",
    quant: "F16",
    minVramGb: 12,
    storageGb: 6.46,
    source: "https://huggingface.co/stabilityai/stable-diffusion-xl-base-1.0",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "sd_xl_base_1.0.safetensors",
        filename: "sd_xl_base_1.0.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "Stability AI Community License.",
    notes:
      "Standard high-resolution (1024x1024) image generator. Recommended for standard workstation GPUs with 12GB+ VRAM.",
  },
  {
    modelId: "juggernaut-xl-v9",
    kind: "image",
    provider: "RunDiffusion",
    family: "Stable-Diffusion-XL",
    version: "9.0",
    size: "6.6GB",
    quant: "F16",
    minVramGb: 12,
    storageGb: 6.61,
    source: "https://huggingface.co/RunDiffusion/Juggernaut-XL-v9",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "Juggernaut-XL_v9_RunDiffusionPhoto_v2.safetensors",
        filename: "Juggernaut-XL_v9_RunDiffusionPhoto_v2.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "Stability AI Community License.",
    notes:
      "The gold standard for photorealistic digital photography and cinematic lighting. Exceptional detail and realism. Requires 12GB+ VRAM.",
  },
  {
    modelId: "animagine-xl-3.1",
    kind: "image",
    provider: "CagliostroLab",
    family: "Stable-Diffusion-XL",
    version: "3.1",
    size: "6.5GB",
    quant: "F16",
    minVramGb: 12,
    storageGb: 6.46,
    source: "https://huggingface.co/cagliostrolab/animagine-xl-3.1",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "animagine-xl-3.1.safetensors",
        filename: "animagine-xl-3.1.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "Stability AI Community License.",
    notes:
      "The premier open-source model for high-fidelity anime, manga, and Japanese illustration styles. Requires 12GB+ VRAM.",
  },
  {
    modelId: "realvis-xl-v4.0",
    kind: "image",
    provider: "SG161222",
    family: "Stable-Diffusion-XL",
    version: "4.0",
    size: "6.5GB",
    quant: "F16",
    minVramGb: 12,
    storageGb: 6.46,
    source: "https://huggingface.co/SG161222/RealVisXL_V4.0",
    repositoryRevision: "main",
    artifacts: [
      {
        sourcePath: "RealVisXL_V4.0.safetensors",
        filename: "RealVisXL_V4.0.safetensors",
        role: "primary",
      },
    ],
    inputModalities: ["text"],
    outputModalities: ["image"],
    features: ["text-to-image", "image-to-image"],
    commercialStatus: "open",
    catch: "CreativeML Open RAIL-M license.",
    notes:
      "Top-tier photorealistic alternative to Juggernaut XL. Excellent for high-fidelity human portraits, realistic environments, and natural textures. Requires 12GB+ VRAM.",
  },
]);

export function byId(modelId: string): ModelSpec | undefined {
  return CATALOG.find((model) => model.modelId === modelId);
}

export function primaryArtifact(model: ModelSpec): ModelArtifact {
  const artifact = model.artifacts.find(({ role }) => role === "primary");
  if (!artifact) {
    throw new Error(`Model "${model.modelId}" has no primary artifact`);
  }
  return artifact;
}

/**
 * Checks whether every file declared by a catalog model is ready to use.
 *
 * Expected sizes catch interrupted or stale downloads without hashing large
 * artifacts during routine startup. Full checksums remain installer work.
 */
export function resolveCatalogInstallation(
  model: ModelSpec,
  kindDirectory: string,
): CatalogInstallationState {
  const primary = primaryArtifact(model);
  const complete = model.artifacts.every((artifact) => {
    const path = join(kindDirectory, artifact.filename);
    if (!existsSync(path)) return false;
    if (artifact.expectedSizeBytes === undefined) return true;

    try {
      return statSync(path).size === artifact.expectedSizeBytes;
    } catch {
      return false;
    }
  });

  return {
    complete,
    primaryPath: join(kindDirectory, primary.filename),
  };
}

export function artifactDownloadUrl(
  model: ModelSpec,
  artifact: ModelArtifact,
): string {
  const base = model.source.replace(/\/$/, "");
  const sourcePath = artifact.sourcePath.replace(/^\/+/, "");
  return `${base}/resolve/${model.repositoryRevision}/${sourcePath}`;
}

export function modelDownloadUrl(model: ModelSpec): string {
  return artifactDownloadUrl(model, primaryArtifact(model));
}

export function listModels(kind?: ModelKind): ModelSpec[] {
  return kind ? CATALOG.filter((m) => m.kind === kind) : [...CATALOG];
}

export function recommendedForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter((m) => m.kind === "llm" && m.minVramGb <= vramGb).sort(
    (a, b) =>
      (b.codingScore ?? 0) - (a.codingScore ?? 0) || b.minVramGb - a.minVramGb,
  );
}

export function recommendedSttForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter((m) => m.kind === "stt" && m.minVramGb <= vramGb).sort(
    (a, b) => a.storageGb - b.storageGb,
  );
}

export function recommendedImageForVram(vramGb: number): ModelSpec[] {
  return CATALOG.filter(
    (m) => m.kind === "image" && m.minVramGb <= vramGb,
  ).sort((a, b) => a.storageGb - b.storageGb);
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

export function evaluateModelFit(
  model: ModelSpec,
  systemVramGb: number,
): MemoryFitEvaluation {
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

    const computed = ((p * b) / 8) * 1.2;
    requiredVramGb = Math.max(computed, minVramGb);
  }

  const headroomGb = systemVramGb - minVramGb;
  let status: MemoryFitStatus = "perfect";
  let message = "";

  if (systemVramGb < minVramGb) {
    status = "insufficient";
    message = `Requires ${minVramGb}GB, you have ${systemVramGb}GB`;
  } else if (
    headroomGb < 4 ||
    (systemVramGb > 0 && minVramGb / systemVramGb > 0.75)
  ) {
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
    message,
  };
}

export function calculateMaxSafeContextSize(
  model: ModelSpec,
  systemVramGb: number,
): number {
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

  const standardBlocks = [
    4096, 8192, 16384, 24576, 32768, 49152, 65536, 98304, 131072,
  ];

  let recommended = 4096;
  for (const block of standardBlocks) {
    if (block <= calculatedC) {
      recommended = block;
    }
  }

  const isOlderModel =
    model.family.toLowerCase().includes("phi-3") ||
    model.modelId.includes("llama-3.2-") ||
    model.modelId.includes("llama-3-8b");
  const maxModelCtx = isOlderModel ? 8192 : 131072;

  return Math.min(recommended, maxModelCtx);
}
