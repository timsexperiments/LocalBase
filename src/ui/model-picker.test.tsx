import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { ModelPickerList } from "./model-picker";
import type { Model } from "./client";

const model: Model = {
  id: "test-q4_k_m",
  catalog: {
    name: "test",
    displayName: "Test model",
    kind: "llm",
    qualificationState: "qualified",
    quantization: "Q4_K_M",
    memory: { minimumVramEstimateGb: 3, storageEstimateGb: 1 },
    features: [],
    inputModalities: ["text"],
    outputModalities: ["text"],
    contextWindowTokens: 8192,
    capabilities: null,
  },
  device: { installed: true, selected: true, runtime: null },
};
const props = {
  selectedId: model.id,
  search: "",
  busy: false,
  hostMemory: null,
  onChoose: () => {},
};

test("picker leads with usable models and explains setup separately", () => {
  const markup = renderToStaticMarkup(
    <ModelPickerList
      {...props}
      models={[
        {
          ...model,
          id: "missing",
          device: { ...model.device, installed: false },
        },
        model,
        {
          ...model,
          id: "disabled",
          device: { ...model.device, selected: false },
        },
      ]}
    />,
  );
  expect(markup).toContain('role="status">1 ready to use');
  expect(markup).toContain('aria-pressed="true"');
  expect(markup.indexOf("Ready to use")).toBeLessThan(
    markup.indexOf("Available to set up"),
  );
  expect(markup).toContain("Install in Manage models");
  expect(markup).toContain("Enable in Manage models");
  expect(markup.match(/disabled=""/g)).toHaveLength(2);
  expect(markup).not.toContain('<details class="picker-unavailable" open');
});

test("picker search includes quantization and exposes a meaningful empty state", () => {
  expect(
    renderToStaticMarkup(
      <ModelPickerList {...props} models={[model]} search="q4_K_M" />,
    ),
  ).toContain("Ready to use");
  const empty = renderToStaticMarkup(
    <ModelPickerList {...props} models={[model]} search="missing" />,
  );
  expect(empty).toContain("No models match your search.");
  expect(empty).not.toContain("model-card");
  expect(
    renderToStaticMarkup(<ModelPickerList {...props} models={[model]} busy />),
  ).toContain('disabled=""');
});
