// Golden download choice and catalog matching from the TypeScript UI.
//
//   bun run go/tools/golden/choice.ts > go/internal/web/testdata/choice.json
//
// Choice sets are the recorded Hub previews (internal/hub/testdata) plus
// edge cases; catalog identities are the built-in catalog plus community
// look-alikes.
import { CATALOG } from "../../../src/catalog";
import { lowQualityQuant, orderedChoices, qualifiedCatalogMatch, recommendedChoice } from "../../../src/ui/model-choice";

const golden = await Bun.file(new URL("../../internal/hub/testdata/golden.json", import.meta.url)).json();
type C = { path: string; sizeBytes: number; quant: string | null; fit: { verdict: "fits" | "tight" | "wont-fit" } | null };
const sets: { name: string; choices: C[] }[] = [];
for (const p of golden.previews) {
  const choices = p.preview?.ok?.choices;
  if (choices?.length) sets.push({ name: p.repo, choices: choices.map(({ path, sizeBytes, quant, fit }: any) => ({ path, sizeBytes, quant, fit: fit && { verdict: fit.verdict } })) });
}
const c = (path: string, sizeBytes: number, quant: string | null, verdict?: "fits" | "tight" | "wont-fit"): C => ({ path, sizeBytes, quant, fit: verdict ? { verdict } : null });
sets.push(
  { name: "only low-bit", choices: [c("m-IQ2_XS.gguf", 1, "IQ2_XS", "fits"), c("m-Q2_K.gguf", 2, "Q2_K", "fits"), c("m-TQ1_0.gguf", 1, "TQ1_0", "fits")] },
  { name: "tight Q4 beats nothing", choices: [c("m-Q4_K_M.gguf", 5, "Q4_K_M", "tight"), c("m-Q8_0.gguf", 9, "Q8_0", "wont-fit")] },
  { name: "fits Q8 beats tight Q4", choices: [c("m-Q4_K_M.gguf", 5, "Q4_K_M", "tight"), c("m-Q8_0.gguf", 9, "Q8_0", "fits")] },
  { name: "UD Q4_K_XL", choices: [c("UD-Q4_K_XL/m.gguf", 6, "UD-Q4_K_XL", "fits"), c("m-Q5_K_M.gguf", 7, "Q5_K_M", "fits"), c("m-q4_k_m.gguf", 5, "q4_k_m")] },
  { name: "projectors and mtp", choices: [c("mmproj-F16.gguf", 1, "F16", "fits"), c("mtp/m-Q4_K_M.gguf", 1, "Q4_K_M", "fits"), c("projectors/p-Q4_K_M.gguf", 1, "Q4_K_M", "fits"), c("m-F16.gguf", 10, "F16", "fits")] },
  { name: "unknown quant and fit", choices: [c("b.gguf", 3, null), c("a.gguf", 3, null), c("m-Q6_K.gguf", 4, "Q6_K")] },
  { name: "empty", choices: [] },
);
const quants = [null, "Q4_K_M", "IQ1_S", "iq2_xxs", "Q2_K", "Q2_K_L", "TQ1_0", "TQ2_0", "UD-IQ2_M", "UD-Q2_K_XL", "Q3_K_M", "IQ3_XS", "Q12", "XQ2", "BF16"];

type Identity = { id: string; catalog: { name: string; displayName?: string; source?: string; qualificationState: string; trust?: string } };
const models: Identity[] = CATALOG.map((m: any) => ({ id: m.modelId, catalog: { name: m.family, displayName: m.displayName, source: m.source, qualificationState: m.qualificationState } }));
models.unshift(
  { id: "community-coder", catalog: { name: "Qwen2.5 Coder 7B Instruct", displayName: "Qwen2.5 Coder 7B Instruct", source: "https://huggingface.co/bartowski/Qwen2.5-Coder-7B-Instruct-GGUF/", qualificationState: "qualified", trust: "community" } },
  { id: "unqualified-next", catalog: { name: "Qwen3 Coder Next", source: "https://huggingface.co/Qwen/Qwen3-Coder-Next-GGUF", qualificationState: "community" } },
);
const repos = [...golden.discovery.ok.hits.map((h: any) => h.repo), ...golden.previews.map((p: any) => p.repo), "someone/Qwen2.5-Coder-1.5B-Instruct-GGUF", "x/qwen2.5-coder-1.5b-instruct-Q4_K_M", "x/Qwen2.5-Coder-1.5B-Instruct-abliterated-GGUF", "noslash"];

console.log(JSON.stringify({
  sets: sets.map(({ name, choices }) => ({ name, choices, recommended: recommendedChoice(choices)?.path ?? null, ordered: orderedChoices(choices).map((x) => x.path) })),
  quants: quants.map((q) => ({ quant: q, low: lowQualityQuant(q) })),
  models,
  matches: repos.map((repo) => ({ repo, match: qualifiedCatalogMatch(repo, models)?.id ?? null })),
}, null, 1));
