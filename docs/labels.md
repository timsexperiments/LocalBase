# Issue and PR labels

**Exist today:** `bug`, `documentation`, `duplicate`, `enhancement`, `good first issue`, `help wanted`, `invalid`, `question`, `wontfix`, `dependencies`, `github_actions`, `dual` (runs the differential job).

**Proposed, for a maintainer to create:**

| Label | Use |
| --- | --- |
| `model-request` | Requests to add a model to the catalog. The issue template applies it |
| `area:api`, `area:ui`, `area:runtime`, `area:memory`, `area:auth`, `area:install` | Which part is affected |
| `platform:linux`, `platform:macos`, `platform:nvidia` | Platform-specific reports |
| `needs-info` | Waiting on the reporter |
| `legacy-ts` | Only affects the TypeScript version |

GitHub ignores a label in an issue template if the label doesn't exist, so templates keep working until these are created.
