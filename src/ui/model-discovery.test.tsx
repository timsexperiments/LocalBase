import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { DiscoveryCard } from "./model-discovery";
import type { Model } from "./client";

const file = (quant: string) =>
  ({
    path: `a-very-long-model-name-${quant}.gguf`,
    sizeBytes: 16 * 1024 ** 3,
    quant,
    modelId: quant,
    existing: null,
    fit: {
      verdict: "fits",
      minVramGb: 19,
      message: "Leaves 45 GB headroom (estimate)",
    },
  }) as const;
const hit = {
  repo: "publisher/Qwen3.8-27B-GGUF",
  url: "https://huggingface.co/publisher/Qwen3.8-27B-GGUF",
  downloads: 100,
  license: "apache-2.0",
  pipelineTag: "text-generation",
  gated: false,
  likely: { kind: "llm", label: "Chat" },
  preview: {
    kind: "llm",
    choices: [file("IQ1_M"), file("Q4_K_M"), file("Q8_0")],
    defaultPath: file("IQ1_M").path,
  },
  reason: null,
} satisfies Parameters<typeof DiscoveryCard>[0]["hit"];
const props = {
  hit,
  connection: { kind: "session" } as const,
  canManage: true,
  catalogMatch: undefined,
  openCatalog: () => {},
  onAdded: () => {},
};
const catalogMatch: Model = {
  id: "qualified-qwen",
  catalog: {
    name: "qwen",
    displayName: "Qwen3.8 27B",
    kind: "llm",
    qualificationState: "qualified",
    quantization: "Q4_K_M",
    memory: { minimumVramEstimateGb: 19, storageEstimateGb: 16 },
    features: [],
    inputModalities: ["text"],
    outputModalities: ["text"],
    contextWindowTokens: null,
    capabilities: null,
  },
  device: { selected: false, installed: false, runtime: null },
};

test("discovery explains a balanced default and keeps all alternatives readable", () => {
  const markup = renderToStaticMarkup(<DiscoveryCard {...props} />);
  expect(markup).toContain("Suggested download · Q4_K_M");
  expect(markup).toContain("A balance of download size and quality.");
  expect(markup).toContain("Low quality: prefer Q4 or a smaller model");
  expect(markup).toContain(
    'checked="" value="a-very-long-model-name-Q4_K_M.gguf"',
  );
  expect(markup).toContain("Save to catalog");
  expect(markup).toContain("Add and install");
  expect(markup).not.toContain("<select");
});

test("qualified catalog match precedes and collapses the community download path", () => {
  const markup = renderToStaticMarkup(
    <DiscoveryCard {...props} catalogMatch={catalogMatch} />,
  );
  expect(markup).toContain("View catalog model");
  expect(markup.indexOf("Available in the Local catalog")).toBeLessThan(
    markup.indexOf("Use a community file instead"),
  );
  expect(markup).toContain('<details class="discovery-download">');
  const noPreview = renderToStaticMarkup(
    <DiscoveryCard
      {...props}
      catalogMatch={catalogMatch}
      hit={{ ...hit, preview: null }}
    />,
  );
  expect(noPreview).toContain("View catalog model");
});

test("discovery keeps read-only actions disabled and warns when no safe default exists", () => {
  const markup = renderToStaticMarkup(
    <DiscoveryCard
      {...props}
      canManage={false}
      hit={{ ...hit, preview: { ...hit.preview, choices: [file("IQ1_M")] } }}
    />,
  );
  expect(markup).toContain("No balanced download fits");
  expect(markup).toContain('disabled=""');
  expect(markup).not.toContain("Suggested download");
});

test("unknown quantization is a neutral selection with facts inside its file summary", () => {
  const unknown = { ...file("unknown"), quant: null };
  const markup = renderToStaticMarkup(
    <DiscoveryCard
      {...props}
      hit={{ ...hit, preview: { ...hit.preview, choices: [unknown] } }}
    />,
  );
  expect(markup).toContain("Selected download · Quantization unknown");
  expect(markup).not.toContain("Suggested");
  const summary = markup.slice(
    markup.indexOf('class="download-recommendation"'),
    markup.indexOf('class="discovery-alternatives"'),
  );
  expect(summary).toContain(unknown.path);
  expect(summary).toContain("<dt>Download</dt>");
  expect(summary).toContain("<dt>Memory</dt>");
});

test("multi-part options display total size and follow recommended-first size order", () => {
  const large = {
    ...file("BF16"),
    path: "BF16/model-BF16-00001-of-00002.gguf",
    sizeBytes: 100 * 1024 ** 3,
    partCount: 2,
  };
  const small = { ...file("IQ2_XXS"), sizeBytes: 8 * 1024 ** 3 };
  const markup = renderToStaticMarkup(
    <DiscoveryCard
      {...props}
      hit={{
        ...hit,
        preview: { ...hit.preview, choices: [large, small, file("Q4_K_M")] },
      }}
    />,
  );
  const options = markup.slice(markup.indexOf("<fieldset"));
  expect(options.indexOf(file("Q4_K_M").path)).toBeLessThan(
    options.indexOf(small.path),
  );
  expect(options.indexOf(small.path)).toBeLessThan(options.indexOf(large.path));
  expect(options).toContain("100.0 GiB · 2 parts, total");
});
