# Versioning and deprecation

LocalBase follows [Semantic Versioning](https://semver.org/). Before 1.0, a breaking change bumps the minor version (0.2 to 0.3), and patch releases don't break anything.

## What's stable before 1.0

| Stable | Promise |
| --- | --- |
| `/v1` HTTP API | Existing routes, fields and error codes keep working. New optional fields can appear |
| CLI commands and flags | Existing names and `--json` shapes keep working. New ones can appear |
| Data directory and database | A new version opens an older version's data. Any change that breaks **rollback by binary swap** is called out in the changelog |

## What isn't

- `/_localbase/` admin routes, `/app/` routes and page layouts.
- Log event names and diagnostics bundle contents.
- The built-in catalog: models can be added or dropped. A dropped model keeps working like one you imported.
- Pinned runtime versions, which change independently.
- Anything marked experimental.
- Platforms that aren't first-class. See [platforms and hardware](../README.md#platforms-and-hardware).

`GET /openapi.json` on a running gateway marks which routes are stable: `/v1` is, and `/_localbase` isn't.

## Deprecation

When something stable has to change:

1. It's marked deprecated in the changelog and docs, and in CLI output where practical.
2. It keeps working for at least one minor release.
3. It's removed in a later minor release, listed under **Removed** in the changelog.

Security fixes can skip this if keeping the old behaviour would leave people exposed. After 1.0, removals wait for a major version.

## Releases

Only the latest release gets fixes. Tags are `vX.Y.Z`, built by `.github/workflows/release-go.yml`. Runtime packages have their own tags, such as `whisper-v0.0.2`, and their own versions.
