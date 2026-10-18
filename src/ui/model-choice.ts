import type { Model } from "./client";

type DownloadChoice = {
  path: string;
  quant: string | null;
  fit: { verdict: "fits" | "tight" | "wont-fit" } | null;
};

export function lowQualityQuant(quant: string | null) {
  return /(?:^|[-_])(?:IQ[12]|Q[12]|TQ[12])(?:_|$)/i.test(quant ?? "");
}

/** Prefer balanced Q4 files with known headroom, never aggressive low-bit defaults. */
export function recommendedChoice<T extends DownloadChoice>(
  choices: readonly T[],
) {
  const rank = (choice: T) => {
    const quant = choice.quant?.toUpperCase().replace(/^UD-/, "");
    const quality =
      quant === "Q4_K_M"
        ? 0
        : quant === "Q4_K_XL"
          ? 1
          : /^Q[4568]/.test(quant ?? "")
            ? 2
            : 3;
    const fit =
      choice.fit?.verdict === "fits"
        ? 0
        : choice.fit?.verdict === "tight"
          ? 1
          : 2;
    return fit * 10 + quality;
  };
  return choices
    .filter(
      (choice) =>
        !lowQualityQuant(choice.quant ?? choice.path) &&
        !/(?:^|\/)(?:mtp[-_.]|mmproj[-_.]|projectors?\/)/i.test(choice.path) &&
        !/(?:^|\/)mtp\//i.test(choice.path) &&
        choice.fit?.verdict !== "wont-fit",
    )
    .sort((a, b) => rank(a) - rank(b))[0];
}

/** Exact normalized family names only; never match a derivative by substring. */
type CatalogIdentity = {
  catalog: Pick<
    Model["catalog"],
    "name" | "displayName" | "source" | "qualificationState" | "trust"
  >;
};

export function qualifiedCatalogMatch<T extends CatalogIdentity>(
  repo: string,
  models: readonly T[],
) {
  const identity = (name: string) =>
    name
      .toLowerCase()
      .replace(/[-_](?:ud[-_])?(?:i?q\d.*|bf16|f16)$/i, "")
      .replace(/[-_]gguf$/i, "")
      .replace(/[^a-z0-9]/g, "");
  const name = repo.split("/").at(-1) ?? "";
  return models.find((model) => {
    if (
      model.catalog.qualificationState !== "qualified" ||
      (model.catalog.trust && model.catalog.trust !== "qualified")
    )
      return false;
    const source = model.catalog.source?.replace(/\/$/, "");
    const sameRepo = source === `https://huggingface.co/${repo}`;
    return (
      sameRepo ||
      identity(model.catalog.displayName ?? model.catalog.name) ===
        identity(name)
    );
  });
}
