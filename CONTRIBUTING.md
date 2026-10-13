# Contributing

## Development

Use Bun `1.3.14` (the version in `packageManager` and CI). Install with `bun install`.

Before opening a pull request, run:

```sh
bun run check
bun test path/to/relevant.test.ts
bun run build
```

Use a focused branch named `type/short-description` and a pull request title in `type: concise summary` form. Follow the repository guidance in [AGENTS.md](.agents/AGENTS.md).

## Developer Certificate of Origin

Contributions are accepted under AGPL-3.0-or-later: inbound contributions are licensed on the same terms as the project (inbound = outbound). Every commit must include a DCO sign-off; use `git commit -s`. The sign-off certifies the [Developer Certificate of Origin](https://developercertificate.org/).
