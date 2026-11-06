// The checked-in memory measurement records, for embedding in the Go binary.
//
//   bun run go/tools/golden/measurements.ts > go/internal/memory/measurements.json
import { MEMORY_MEASUREMENTS } from "../../../src/domains/runtime/memory-measurements";

console.log(
  JSON.stringify(
    MEMORY_MEASUREMENTS.map(({ date: _d, hardware: _h, method: _m, variant: _v, ...record }) => record),
    null,
    1,
  ),
);
