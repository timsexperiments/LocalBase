export type LockPackage = [
  string,
  string,
  {
    dependencies?: Record<string, string>;
    optionalDependencies?: Record<string, string>;
  }?,
  string?,
];

export type Lock = {
  workspaces: Record<string, { dependencies?: Record<string, string> }>;
  packages: Record<string, LockPackage>;
};

export function packageClosure(lock: Lock): LockPackage[] {
  const rootDependencies = lock.workspaces[""]?.dependencies;
  if (!rootDependencies) throw new Error("bun.lock has no root dependencies.");
  const pending = Object.keys(rootDependencies)
    .filter((name) => name !== "esbuild" && name !== "react-refresh")
    .map((name) => ({ parent: "", name }));
  const included = new Map<string, LockPackage>();
  while (pending.length) {
    const { parent, name } = pending.pop()!;
    const parentParts = parent ? parent.split("/") : [];
    let record: LockPackage | undefined;
    let key: string | undefined;
    for (let length = parentParts.length; length >= 0 && !record; length -= 1) {
      const candidate = [...parentParts.slice(0, length), name].join("/");
      const found = lock.packages[candidate];
      if (found?.[0].startsWith(`${name}@`)) {
        record = found;
        key = candidate;
      }
    }
    if (!record || !key)
      throw new Error(
        `Missing package record for ${name} below ${parent || "root"}.`,
      );
    const identity = `${record[0]}\0${record[3] ?? ""}`;
    if (included.has(identity)) continue;
    included.set(identity, record);
    for (const dependency of [
      ...Object.keys(record[2]?.dependencies ?? {}),
      ...Object.keys(record[2]?.optionalDependencies ?? {}),
    ])
      pending.push({ parent: key, name: dependency });
  }
  return [...included.values()].sort((a, b) => a[0].localeCompare(b[0]));
}
