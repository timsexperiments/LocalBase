# Contributing

New work goes in [`go/`](go/); [`src/`](src/README.md) is the legacy TypeScript version. See [`go/README.md`](go/README.md) for build and test commands.

Before opening a pull request, from `go/`:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test ./internal/... ./cmd/...
```

Use a focused branch named `type/short-description` and a pull request title in `type: concise summary` form. Follow the repository guidance in [AGENTS.md](.agents/AGENTS.md).

## Licensing of contributions

Contributions are accepted under AGPL-3.0-or-later: inbound contributions are licensed on the same terms as the project (inbound = outbound).
