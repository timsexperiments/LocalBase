import { describe, expect, test } from "bun:test";
import { byId, primaryArtifact } from "../../catalog";
import { minimumMemoryGb } from "../runtime/memory-estimate";
import type {
  ObservedMemoryRecord,
  ObservedMemoryStore,
} from "../runtime/observed-memory";
import {
  collectModelMemoryInputs,
  evaluateModelFit,
  fitStatus,
  modelMemoryFigure,
  modelMemoryJson,
  observedMemoryInputs,
  referenceLaunchShape,
  selectObservedPeak,
} from "./memory-fit";

const llm = byId("qwen2.5-coder-1.5b-instruct-q4_k_m")!;
const measured = byId("whisper-tiny-en-q8_0")!;
const gib = 1024 ** 3;

function peak(
  overrides: Partial<ObservedMemoryRecord> = {},
): ObservedMemoryRecord {
  return {
    modelId: llm.modelId,
    artifactSha256: primaryArtifact(llm).sha256!,
    launchShape: referenceLaunchShape(llm),
    peakBytes: 6 * gib,
    method: "footprint",
    observedAt: "2026-10-15T00:00:00.000Z",
    ...overrides,
  };
}

describe("memory figure precedence", () => {
  test("an uninstalled LLM falls back to the estimate", () => {
    const figure = modelMemoryFigure(llm);
    expect(figure.source).toBe("estimate");
    expect(figure.minVramGb).toBe(minimumMemoryGb(figure.demandBytes));
    expect(figure.launchShape).toMatch(/^ctx=\d+;slots=1;kv=/);
  });

  test("a calibration run beats the estimate", () => {
    expect(modelMemoryFigure(measured).source).toBe("measured");
  });

  test("an observed peak beats both", () => {
    const figure = modelMemoryFigure(llm, { observed: [peak()] });
    expect(figure.source).toBe("observed");
    expect(figure.demandBytes).toBe(6 * gib);
    expect(figure.minVramGb).toBe(minimumMemoryGb(6 * gib));
    expect(figure.referenceSource).toBe("estimate");
    expect(figure.observed).toEqual(peak());
  });

  test("the reference shape wins over a newer peak at another shape", () => {
    const other = peak({
      launchShape: "ctx=32768;slots=4;kv=f16/f16",
      peakBytes: 9 * gib,
      observedAt: "2026-10-16T00:00:00.000Z",
    });
    expect(
      selectObservedPeak([other, peak()], referenceLaunchShape(llm)),
    ).toEqual(peak());
    expect(selectObservedPeak([other], referenceLaunchShape(llm))).toEqual(
      other,
    );
  });

  test("Linux VmHWM peaks are kept but never used for fit", () => {
    const hostOnly = peak({ method: "vmhwm" });
    expect(
      selectObservedPeak([hostOnly], hostOnly.launchShape),
    ).toBeUndefined();
    expect(modelMemoryFigure(llm, { observed: [hostOnly] }).source).toBe(
      "estimate",
    );
  });
});

describe("fit verdicts", () => {
  test("fits, tight, and won't fit", () => {
    expect(fitStatus(4, 64)).toBe("perfect");
    expect(fitStatus(62, 64)).toBe("tight");
    expect(fitStatus(50, 64)).toBe("tight");
    expect(fitStatus(65, 64)).toBe("insufficient");
  });

  test("evaluations carry the source and the legacy minimum", () => {
    const fit = evaluateModelFit(llm, 4, { observed: [peak()] });
    expect(fit.verdict).toBe("wont-fit");
    expect(fit.source).toBe("observed");
    expect(fit.message).toContain("observed");
    expect(modelMemoryJson(fit)).toEqual({
      minVramGb: fit.minVramGb,
      minimumVramEstimateGb: fit.minVramGb,
      memorySource: "observed",
      memoryLaunchShape: peak().launchShape,
      observedPeakBytes: 6 * gib,
      fitVerdict: "wont-fit",
      systemMemoryGb: 4,
    });
    expect(modelMemoryJson(evaluateModelFit(llm, 128)).observedPeakBytes).toBe(
      null,
    );
  });
});

describe("memory inputs", () => {
  const store: ObservedMemoryStore = {
    record: () => {},
    list: (modelId, sha) =>
      modelId === llm.modelId && sha === primaryArtifact(llm).sha256
        ? [peak()]
        : [],
  };

  test("observedMemoryInputs reads peaks for the pinned artifact", () => {
    const inputs = observedMemoryInputs([llm, measured], store);
    expect(inputs.get(llm.modelId)?.observed).toEqual([peak()]);
    expect(inputs.get(measured.modelId)?.observed).toEqual([]);
  });

  test("collectModelMemoryInputs skips models that are not installed", async () => {
    const reads: string[] = [];
    const inputs = await collectModelMemoryInputs({
      catalog: [llm, measured],
      installations: new Map([[llm.modelId, true]]),
      directoryFor: () => "/models",
      observed: store,
      readGeometry: async (path) => {
        reads.push(path);
        return null;
      },
    });
    expect(reads).toEqual([`/models/${primaryArtifact(llm).filename}`]);
    expect(inputs.get(llm.modelId)?.observed).toEqual([peak()]);
    expect(inputs.get(measured.modelId)).toEqual({});
  });
});
