// Golden catalog memory figures (estimate, measured, minimum) from TS for
// every built-in model at its reference shape, without local geometry.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/catalog-memory.ts \
//     > go/internal/memory/testdata/catalog-memory.json
import { CATALOG } from "../../../src/catalog";
import {
  estimateCatalogModelMemory,
  measuredCatalogModelMemory,
} from "../../../src/domains/runtime/memory-estimate";
import { modelMemoryFigure } from "../../../src/domains/models/memory-fit";

console.log(
  JSON.stringify(
    CATALOG.map((spec) => {
      const figure = modelMemoryFigure(spec);
      return {
        modelId: spec.modelId,
        estimate: estimateCatalogModelMemory(spec).demand,
        measured: measuredCatalogModelMemory(spec) ?? null,
        minVramGb: figure.minVramGb,
        source: figure.source,
        launchShape: figure.launchShape,
      };
    }),
    null,
    1,
  ),
);
