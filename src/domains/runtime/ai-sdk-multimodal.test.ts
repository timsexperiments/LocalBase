import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createOpenAI } from "@ai-sdk/openai";
import {
  embed,
  embedMany,
  experimental_transcribe,
  generateImage,
  generateText,
} from "ai";
import { join } from "node:path";
import { byId, primaryArtifact } from "../../catalog";
import {
  createLocalBaseAiSdkProvider,
  latestUpstreamMultipartFormData,
  latestUpstreamRequestBody,
} from "../../test/ai-sdk-conformance";
import {
  startGatewayFixture,
  type GatewayFixture,
  type GatewayFixtureOptions,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";
import { minimalWav, tinyPng, tinyPngBase64 } from "../../test/media-fixtures";

const PRIMARY_LLM_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const SWITCHED_LLM_MODEL = "qwen2.5-coder-7b-instruct-q4_k_m";
const PRIMARY_STT_MODEL = "whisper-large-v3-turbo";
const SWITCHED_STT_MODEL = "whisper-tiny-en-q8_0";
const PRIMARY_IMAGE_MODEL = "stable-diffusion-v1-5";
const SWITCHED_IMAGE_MODEL = "dreamshaper-v8";

function artifactPath(modelsDir: string, modelId: string): string {
  const model = byId(modelId);
  if (!model) throw new Error(`Unknown catalog model: ${modelId}`);
  return join(modelsDir, primaryArtifact(model).filename);
}

function runtimeModelPath(args: string[], flag: "-m" | "--model"): string {
  const index = args.indexOf(flag);
  if (index === -1 || !args[index + 1]) {
    throw new Error(`Runtime launch did not include ${flag}: ${args}`);
  }
  return args[index + 1];
}

async function startSwitchingGateway(
  options: Pick<
    GatewayFixtureOptions,
    "sttHealthControlled" | "imageHealthControlled"
  >,
): Promise<GatewayFixture> {
  const gateway = await startGatewayFixture(options);
  const config = gateway.readConfig();
  gateway.saveConfig({
    ...config,
    selectedSttModels: [PRIMARY_STT_MODEL, SWITCHED_STT_MODEL],
    selectedImageModels: [PRIMARY_IMAGE_MODEL, SWITCHED_IMAGE_MODEL],
  });
  await Promise.all([
    writeCompleteCatalogArtifact(config.sttModelsDir, SWITCHED_STT_MODEL),
    writeCompleteCatalogArtifact(config.imageModelsDir, SWITCHED_IMAGE_MODEL),
  ]);
  return gateway;
}

function transcribe(gateway: GatewayFixture, model: string): Promise<Response> {
  const form = new FormData();
  form.append("model", model);
  form.append(
    "file",
    new File([minimalWav], "fixture.wav", { type: "audio/wav" }),
  );
  return fetch(`${gateway.baseUrl}/v1/audio/transcriptions`, {
    method: "POST",
    body: form,
  });
}

function generate(gateway: GatewayFixture, model: string): Promise<Response> {
  return fetch(`${gateway.baseUrl}/v1/images/generations`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ model, prompt: "A one-pixel sunset" }),
  });
}

describe("Vercel AI SDK multimodal conformance", () => {
  let gateway: GatewayFixture;

  beforeAll(
    async () => {
      gateway = await startGatewayFixture();
      const config = gateway.readConfig();
      gateway.saveConfig({
        ...config,
        selectedLlmModels: [PRIMARY_LLM_MODEL, SWITCHED_LLM_MODEL],
        selectedSttModels: [PRIMARY_STT_MODEL, SWITCHED_STT_MODEL],
        selectedImageModels: [PRIMARY_IMAGE_MODEL, SWITCHED_IMAGE_MODEL],
      });
      await Promise.all([
        writeCompleteCatalogArtifact(config.llmModelsDir, SWITCHED_LLM_MODEL),
        writeCompleteCatalogArtifact(config.sttModelsDir, SWITCHED_STT_MODEL),
        writeCompleteCatalogArtifact(
          config.imageModelsDir,
          SWITCHED_IMAGE_MODEL,
        ),
      ]);
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await gateway?.stop();
    },
    { timeout: 10_000 },
  );

  test("embed forwards float encoding, dimensions, and the selected model", async () => {
    const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;
    const result = await embed({
      model:
        createLocalBaseAiSdkProvider(gateway).embeddingModel(
          SWITCHED_LLM_MODEL,
        ),
      value: "alpha",
      providerOptions: { openaiCompatible: { dimensions: 2 } },
    });

    expect(result.embedding).toEqual([0, 5]);
    expect(result.usage).toEqual({ tokens: 1 });
    expect(latestUpstreamRequestBody(gateway)).toMatchObject({
      model: SWITCHED_LLM_MODEL,
      input: ["alpha"],
      encoding_format: "float",
      dimensions: 2,
    });
    expect(gateway.readConfig().activeLlmModel).toBe(SWITCHED_LLM_MODEL);

    const [launch] = await gateway.waitForLlmRuntimeLaunches(launchOffset, 1);
    expect(runtimeModelPath(launch, "-m")).toBe(
      artifactPath(gateway.readConfig().llmModelsDir, SWITCHED_LLM_MODEL),
    );
  });

  test("embedMany preserves input order in one gateway request", async () => {
    const result = await embedMany({
      model:
        createLocalBaseAiSdkProvider(gateway).embeddingModel(PRIMARY_LLM_MODEL),
      values: ["one", "three"],
    });

    expect(result.values).toEqual(["one", "three"]);
    expect(result.embeddings).toEqual([
      [0, 3],
      [1, 5],
    ]);
    expect(result.usage).toEqual({ tokens: 2 });
    expect(latestUpstreamRequestBody(gateway)).toMatchObject({
      model: PRIMARY_LLM_MODEL,
      input: ["one", "three"],
      encoding_format: "float",
    });
  });

  test("generateImage returns base64 PNGs and selects the requested local image model", async () => {
    const launchOffset = (await gateway.readImageRuntimeLaunches()).length;
    const result = await generateImage({
      model:
        createLocalBaseAiSdkProvider(gateway).imageModel(SWITCHED_IMAGE_MODEL),
      prompt: "A one-pixel sunset",
      n: 2,
      size: "512x512",
      maxRetries: 0,
    });

    expect(result.images.map((image) => image.base64)).toEqual([
      tinyPngBase64,
      tinyPngBase64,
    ]);
    expect(latestUpstreamRequestBody(gateway)).toMatchObject({
      model: SWITCHED_IMAGE_MODEL,
      prompt: "A one-pixel sunset",
      n: 2,
      size: "512x512",
    });
    expect(gateway.readConfig().activeImageModel).toBe(SWITCHED_IMAGE_MODEL);

    const [launch] = await gateway.waitForImageRuntimeLaunches(launchOffset, 1);
    expect(runtimeModelPath(launch, "-m")).toBe(
      artifactPath(gateway.readConfig().imageModelsDir, SWITCHED_IMAGE_MODEL),
    );
  });

  test("generateText transports image bytes as an OpenAI image data URL", async () => {
    const result = await generateText({
      model: createLocalBaseAiSdkProvider(gateway).chatModel(PRIMARY_LLM_MODEL),
      messages: [
        {
          role: "user",
          content: [
            { type: "text", text: "Describe this image." },
            { type: "file", data: tinyPng, mediaType: "image/png" },
          ],
        },
      ],
    });

    expect(result.text).toBe("ok");
    expect(latestUpstreamRequestBody(gateway).messages).toEqual([
      {
        role: "user",
        content: [
          { type: "text", text: "Describe this image." },
          {
            type: "image_url",
            image_url: {
              url: `data:image/png;base64,${tinyPngBase64}`,
            },
          },
        ],
      },
    ]);
  });

  test("experimental_transcribe preserves multipart fields and selects the requested STT model", async () => {
    const launchOffset = (await gateway.readSttRuntimeLaunches()).length;
    const localbase = createOpenAI({
      baseURL: `${gateway.baseUrl}/v1`,
      apiKey: "test-key",
      name: "localbase",
    });
    const result = await experimental_transcribe({
      model: localbase.transcription(SWITCHED_STT_MODEL),
      audio: minimalWav,
      providerOptions: {
        openai: {
          language: "en",
          prompt: "Fixture prompt",
          temperature: 0.25,
          timestampGranularities: ["word", "segment"],
        },
      },
      maxRetries: 0,
    });

    expect(result).toMatchObject({
      text: "fixture transcript",
      language: "en",
      durationInSeconds: 0.5,
      segments: [
        { text: "fixture transcript", startSecond: 0, endSecond: 0.5 },
      ],
    });
    const formData = latestUpstreamMultipartFormData(gateway);
    expect(formData.get("model")).toBe(SWITCHED_STT_MODEL);
    expect(formData.get("language")).toBe("en");
    expect(formData.get("prompt")).toBe("Fixture prompt");
    expect(formData.get("temperature")).toBe("0.25");
    expect(formData.getAll("timestamp_granularities[]")).toEqual([
      "word",
      "segment",
    ]);
    const file = formData.get("file");
    expect(file).toBeInstanceOf(File);
    expect((await (file as File).arrayBuffer()).byteLength).toBe(
      minimalWav.byteLength,
    );
    expect(gateway.readConfig().activeSttModel).toBe(SWITCHED_STT_MODEL);

    const [launch] = await gateway.waitForSttRuntimeLaunches(launchOffset, 1);
    expect(runtimeModelPath(launch, "--model")).toBe(
      artifactPath(gateway.readConfig().sttModelsDir, SWITCHED_STT_MODEL),
    );
  });

  test("switches STT models while the previous startup is waiting for health", async () => {
    const switchingGateway = await startSwitchingGateway({
      sttHealthControlled: true,
    });
    try {
      const first = transcribe(switchingGateway, PRIMARY_STT_MODEL);
      await switchingGateway.waitForSttHealthProbe();
      expect(
        runtimeModelPath(
          await switchingGateway.waitForSttRuntimeStart(),
          "--model",
        ),
      ).toBe(
        artifactPath(
          switchingGateway.readConfig().sttModelsDir,
          PRIMARY_STT_MODEL,
        ),
      );
      const switched = transcribe(switchingGateway, SWITCHED_STT_MODEL);
      expect((await first).status).toBe(503);
      const replacementHealth = await switchingGateway.waitForSttHealthProbe();
      expect(
        runtimeModelPath(
          await switchingGateway.waitForSttRuntimeStart(),
          "--model",
        ),
      ).toBe(
        artifactPath(
          switchingGateway.readConfig().sttModelsDir,
          SWITCHED_STT_MODEL,
        ),
      );
      replacementHealth.release(true);

      expect((await switched).status).toBe(200);
      expect(switchingGateway.readConfig().activeSttModel).toBe(
        SWITCHED_STT_MODEL,
      );
    } finally {
      await switchingGateway.stop();
    }
  });

  test("switches image models while the previous startup is waiting for health", async () => {
    const switchingGateway = await startSwitchingGateway({
      imageHealthControlled: true,
    });
    try {
      const first = generate(switchingGateway, PRIMARY_IMAGE_MODEL);
      await switchingGateway.waitForImageHealthProbe();
      expect(
        runtimeModelPath(
          await switchingGateway.waitForImageRuntimeStart(),
          "-m",
        ),
      ).toBe(
        artifactPath(
          switchingGateway.readConfig().imageModelsDir,
          PRIMARY_IMAGE_MODEL,
        ),
      );
      const switched = generate(switchingGateway, SWITCHED_IMAGE_MODEL);
      expect((await first).status).toBe(503);
      const replacementHealth =
        await switchingGateway.waitForImageHealthProbe();
      expect(
        runtimeModelPath(
          await switchingGateway.waitForImageRuntimeStart(),
          "-m",
        ),
      ).toBe(
        artifactPath(
          switchingGateway.readConfig().imageModelsDir,
          SWITCHED_IMAGE_MODEL,
        ),
      );
      replacementHealth.release(true);

      expect((await switched).status).toBe(200);
      expect(switchingGateway.readConfig().activeImageModel).toBe(
        SWITCHED_IMAGE_MODEL,
      );
    } finally {
      await switchingGateway.stop();
    }
  });
});
