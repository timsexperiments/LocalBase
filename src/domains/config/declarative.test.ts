import { expect, test } from "bun:test";
import { defaultConfig } from "../../manager";
import {
  configurationDocument,
  parseConfiguration,
  planConfiguration,
  renderConfiguration,
} from "./declarative";

const current = defaultConfig("/tmp/localbase-config-test");
const document = configurationDocument(current);
const text = renderConfiguration(document);

test("strict versioned TOML round-trips without secrets or implicit host defaults", () => {
  expect(parseConfiguration(text)).toEqual(document);
  expect(renderConfiguration(parseConfiguration(text))).toBe(text);
  const secretConfig = {
    ...current,
    hfToken: "hf-private",
    otelHeaders: "authorization=private",
    otelEndpoint: "https://private@example.test",
  };
  expect(renderConfiguration(configurationDocument(secretConfig))).toBe(text);
  expect(() => parseConfiguration("version = 1")).toThrow("gateway");
});

test.each([
  ["version", text.replace("version = 1", "version = 2")],
  ["unknown table", `${text}\n[identity]\nprovider = 'unknown'`],
  [
    "secret declaration",
    text.replace("version = 1", 'version = 1\nhfToken = "private"'),
  ],
  ["nested unknown", text.replace("[runtime]", "[runtime]\nextra = true")],
  ["duplicate key", `${text}\npercent = 4`],
  ["type coercion", text.replace("port = 2273", 'port = "2273"')],
  ["port range", text.replace("port = 2273", "port = 65536")],
  ["context", text.replace("ctxSize = 131072", "ctxSize = 10")],
  ["memory", text.replace("percent = 15", "percent = 101")],
  [
    "model kind",
    text.replace(
      `activeLlmModel = "${current.activeLlmModel}"`,
      'activeLlmModel = "whisper-base-q8_0"',
    ),
  ],
  [
    "active membership",
    text.replace(
      `activeLlmModel = "${current.activeLlmModel}"`,
      'activeLlmModel = "mistral-nemo-12b-instruct-q4_k_m"',
    ),
  ],
  [
    "duplicate selection",
    text.replace(
      `selectedLlmModels = ["${current.activeLlmModel}"]`,
      `selectedLlmModels = ["${current.activeLlmModel}", "${current.activeLlmModel}"]`,
    ),
  ],
])("rejects %s", (_name, source) => {
  expect(() => parseConfiguration(source)).toThrow();
});

test("malformed TOML errors do not echo source text", () => {
  expect(() => parseConfiguration('token = "SUPER_SECRET')).toThrow(
    "Invalid TOML configuration.",
  );
});

test("semantic diff normalizes model ordering and TOML presentation", () => {
  const config = {
    ...current,
    selectedLlmModels: [
      current.activeLlmModel,
      "mistral-nemo-12b-instruct-q4_k_m",
    ],
  };
  const desired = configurationDocument(config);
  desired.models.selectedLlmModels.reverse();
  expect(planConfiguration(config, desired)).toEqual({
    changed: false,
    restartRequired: false,
    pendingRestart: false,
    changes: [],
  });
  expect(
    planConfiguration(
      current,
      parseConfiguration(text.replaceAll('"auto"', "'auto'")),
    ),
  ).toMatchObject({ changed: false });
});

test("diff orders leaf paths and uses gateway, memory, and runtime reconciliation boundaries", () => {
  const desired = structuredClone(document);
  desired.gateway.port += 1;
  desired.runtime.port += 1;
  desired.runtime.ctxSize = 8192;
  desired.memory.systemReserve.percent = 20;
  desired.models.selectedSttModels = [];
  desired.models.activeSttModel = "";
  const plan = planConfiguration(current, desired);
  expect(plan.restartRequired).toBe(true);
  expect(
    plan.changes.map((change) => [change.path, change.activation]),
  ).toEqual([
    ["gateway.port", "restart-required"],
    ["memory.systemReserve.percent", "restart-required"],
    ["models.activeSttModel", "hot"],
    ["models.selectedSttModels", "hot"],
    ["runtime.ctxSize", "hot"],
    ["runtime.port", "hot"],
  ]);
  expect(
    planConfiguration(undefined, document).changes.every(
      (change) => change.before === null,
    ),
  ).toBe(true);
});
