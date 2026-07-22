# Project Guidelines

- **Package Management & Execution**: Only ever use `bun` (never `pnpm`, `npm`, or `yarn`) for package installation, scripting, and compilation in this repository.
- **Contributor-Focused Writing**: Write comments and documentation for future contributors, including engineers and AI agents. Explain only durable context that helps readers understand intent, constraints, or non-obvious behavior. Do not mention prior conversations, implementation narratives, or temporary decision history. Runbooks may document specific incidents and observed errors when operationally useful.
- **Concise Documentation**: Keep comments and documentation succinct. Remove or consolidate text that repeats the code, nearby documentation, or other tests.
- **Intentional Testing**: Test core business behavior and meaningful boundaries. Prefer reusable fixtures and in-memory SQLite databases for stateful behavior. Use focused mocks only when they isolate a specific external boundary.
- **Nonredundant Test Coverage**: Before adding tests, inspect existing coverage and extend or consolidate it where practical. Avoid duplicate assertions and tests that only restate implementation details.
- **Pre-Release Compatibility**: Do not preserve obsolete configuration, database, API, or file formats unless explicitly requested. Prefer the clean current design; reset or uninstall is the supported transition while the project remains pre-release.
