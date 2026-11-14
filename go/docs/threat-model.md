# Threat model

This covers the Go gateway: what it protects, from whom, where the lines are, and what is left. To report a problem, see [SECURITY.md](../../SECURITY.md). Decisions that change behaviour are in [DESIGN](../DESIGN.md) §31 and §36.

## What LocalBase is

A gateway on one computer, usually for one owner. It listens on loopback or behind an authenticated proxy, takes API keys and browser sign-ins, and runs model backends as the account that runs it. Backends aren't sandboxed, and they parse model files in native code.

## What it protects

| What | Why it matters |
| --- | --- |
| The account that runs it | Whoever controls what it runs controls that account |
| Keys, sessions, and the secrets in the root ([§27](../DESIGN.md)) | They grant access here and at other services |
| Prompts and outputs | They're private to whoever sent them |
| Disk, memory and backend slots | A local machine has little to spare, and other work runs on it |

## Who it defends against

| Who | What they should get |
| --- | --- |
| Anyone who can reach the port | Public routes and refusals, at little cost to the gateway |
| Someone who signs in at a provider | Nothing, unless a policy, an invitation or the provider's own permissions give it to them |
| A key or person with `inference:*` | Inference. Not this machine's network or files, and not anyone else's runs |
| A holder of `models:manage` | Models from public sources. Not arbitrary programs, and not this machine's network unless the host allows it |
| A holder of `keys:manage` or `access:manage` | Control over what they hold themselves, and no more |
| Another account on this computer | Nothing in the root. A root another account could have changed is refused |
| A model file or a download source | Bounded parsing. Files checked against pins, where there are pins |

Out of scope, as in `SECURITY.md`: someone with the same account or write access to the root, flaws inside third-party backends, and what a model says.

## The lines

| Line | How it holds | Where |
| --- | --- | --- |
| Network to gateway | Credentials before a body is read, limits on connections and slow clients, no method exempt | §31.1, `route_matrix_test.go` |
| Caller to permission | Every route needs a named permission, and a new route is protected unless it's on an explicit public list | `route_matrix_test.go`, `TestOpenAPIPermissions` |
| Who hands out what | Nobody grants more than they hold, checked under one lock | §13.6 item 5a |
| Provider to identity | An address belongs to the provider that vouches for it, and signing in gives nothing unless someone said so | §13.6 items 5b, 5c |
| Request to network | Downloads connect only to public addresses unless the host allows more, judged when the connection is made. A chat content part carries its content, not an address | §31.2, §13.6 item 13 |
| Request to host | Only the command line registers a backend, and a custom backend runs only for a model someone set it on | §31.4 |
| Request to CPU and disk | Imports, GGUF headers and structured output schemas have limits | §31.3, §31.5 |
| Gateway to backend | A gateway only accepts a backend it started | §29 |
| Root directory | See below | §36 |

## Root permissions

A root decides who can sign in and what gets executed, so before anything in it is used:

- **It must be yours.** A root another account owns is refused, with the command to fix it. Under `sudo`, the message says to run as the owner.
- **It is closed to others.** The directory becomes 0700. If others can still change it afterwards, because the volume ignored the chmod or refused it, the root is refused. A read-only volume is exempt, since nobody can change it.
- **What decides who can sign in or what runs must not be writable by others.** That covers the database, `ui-access.json`, `email-delivery.json`, `backends.d` and the like.

**Group-writable counts as writable by others, unless the group is provably private.** On macOS, local accounts share `staff`, so a mode that looks harmless lets every other account on the computer write. A group is private only if the local account files show it has nobody but you, and those files are all that decides membership. Unknown means refused, and the message gives the fix: `chmod go-w <path>`.

Tightening a mode later doesn't undo a file planted while it was open, which is why these are refused rather than quietly fixed. `serve` still tightens the rest of an older root (§27).

## Powerful permissions

`models:manage`, `access:manage`, `keys:manage` and `system:manage` are for named people and specific automation. Never give them to everyone a provider accepts: for a public issuer that may be anyone with an account. [The authentication guide](../../docs/authentication.md) says so too.

## Shared on purpose: custom voices

Custom voices belong to the gateway, not to a key or a person. Anyone with `models:read` can list them and anyone with `inference:speech` can speak in them, and that includes a default key. Anyone with `models:manage` can play back, rename or delete any of them, whoever made them. Treat a voice as visible to everyone with a key.

## Residual risks

These are accepted, and each has a reason.

- **A flood through a tunnel fills the global connection cap,** since everything arrives from one address. The cap protects the process, not availability. Rate limit at the edge.
- **Secrets at rest are plain text** (§27), and a backup holds the same ones (§34.7). File modes are the only protection.
- **A writable parent directory can swap the whole root,** or the backups beside it. Owner and mode checks can't see that.
- **Backends have no credentials,** so any process on this computer can talk to their loopback ports directly.
- **OTLP, OIDC discovery, SMTP and runtime downloads are outside the download guard** (§31.2). They're set from the host or need the highest permissions, and a local collector or a LAN provider is normal.
- **A configured proxy resolves names,** so a public name pointing at a private address gets through it (§31.2).
- **With `--allow-private-imports` on,** a `models:manage` holder can tell open ports from closed ones (§31.2).
- **Callers can only hand down,** so nobody can regrow a permission through the API once its last holder is gone. The host commands are the way back (§13.6 item 5a).
- **A default role isn't tied to a provider:** whoever signs in through any configured provider gets it. That's why adding a provider needs every permission (§13.6 item 5b).
- **A process that wins the moment between the port check and a backend's start** could answer for that backend, where `lsof` is missing. Otherwise it's refused (§29).
- **After a crash between moving an import's files and recording them,** the next import of that id downloads again (§31.3).
- **Backend output at debug level** (`runtime.output`) reaches `events.jsonl`, stderr, diagnostics bundles and OTLP. It's off by default. Since [#499](https://github.com/timsexperiments/LocalBase/pull/499), credentials, query values and fragments in URLs are removed. Marker tests on llama-server showed prompts, system text, tool arguments, embeddings input and image-with-text don't appear; other backends weren't tested.
- **Backends share a prompt cache across keys.** Timing might show that someone already sent a long prefix. This is a possibility from reading the code, not something shown.
- **One key can fill the inference queue.** Denial of service by a holder of inference access is out of scope in `SECURITY.md`.

## What was proven, and what was only read

| Area | How far |
| --- | --- |
| Route and method coverage | Proven: a matrix built from the source, against a real gateway |
| Slow clients and connection limits | Proven on a real binary |
| Download guard | Proven through a recording proxy. Addresses by table test |
| GGUF and schema limits | Proven with crafted files and schemas, and fuzzed. Schemas also against a real llama-server |
| Chat content parts | Proven on a real binary with a real llama-server and a small test model |
| Who hands out what, issuer binding | Proven on real binaries and through the real sign-in flow, with a fake provider. Forced races included |
| Email delivery changes | Proven end to end in a no-network container, with a test certificate |
| Root ownership and group rules | Proven as a different user in Linux containers. On macOS the owner checks use a test hook |
| Backend registration | Proven on a real binary |
| Standard library | Scanned with `govulncheck`, in CI too. Each advisory wasn't assessed for reachability by hand |
| Ownership of runs, video jobs and usage | Read, and covered by each area's tests. Not attacked separately |
| Secrets in logs and diagnostics, file modes | Read, and covered by tests with planted values in some paths |
| Archive extraction, process launch, service units | Read |
| Release workflows, installer, pins | Read. Nothing was run |

## What could not be tested

- A real sign-in provider. Tests use a fake one.
- A real tunnel or proxy in front of the gateway.
- A disk that's actually full. Tests fake the free-space call.
- Another real account on macOS.
- Pages in a real browser, for the security-related messages.
- A projector that takes audio, and custom backends with a real engine.
- A real release, and the installer against real assets.
- The insides of third-party backends.

## How it stays this way

- **Route matrix and OpenAPI permission tests** fail on a route nobody classified.
- **Fuzz targets** run their seeds in `go test`. `scripts/fuzz.sh` searches on demand; a failing input goes into the target's `testdata/fuzz` and is committed with the fix.
- **`govulncheck`** runs in the lint job, and weekly.
- **Mutation checks** by hand on each security fix: revert each rule and see a test fail.
