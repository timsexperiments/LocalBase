# Connect an app

LocalBase speaks the OpenAI chat API, so most tools that let you change the base URL can use it. Open **Connect an app** in the browser, or run:

```bash
local-base connect                       # the address, the model and the clients
local-base connect python                # one client's snippet
local-base connect python --create-key   # the same, with a new key filled in
```

An app needs three things:

| Field | What to use |
| --- | --- |
| Base URL | `http://127.0.0.1:2273/v1` on this computer, or your public address plus `/v1` |
| API key | A scoped key. `--create-key` and the page's **Create a key for chat** grant `inference:chat` and `models:read`, and nothing else |
| Model | An id from `GET /v1/models`. It is required: there is no default for API callers |

With a client named, `local-base connect` prints the snippet that does the job on stdout and everything else on stderr, so a config can go straight into a file. It does not merge: if you already have that file, add to it by hand.

```bash
local-base connect opencode > opencode.json
```

`--model`, `--base-url`, `--name`, `--scopes` and `--expires-days` do what they say, and `--json` returns everything as data. The whole API is described at `/openapi.json`.

## Clients

**Run** means someone ran that exact snippet against a LocalBase gateway. **Follows its docs, not run** means the fields were compared with the client's own documentation, on the date in `internal/connect/clients.go`, and nobody has run it yet. The page and the CLI say the same thing.

| Client | Status |
| --- | --- |
| curl | Run |
| OpenAI Python SDK | Run |
| OpenAI JavaScript SDK | Run |
| opencode | Follows its docs, not run |
| Continue | Follows its docs, not run |
| Cline | Follows its docs, not run |
| Aider | Follows its docs, not run |
| Zed | Follows its docs, not run |
| goose | Follows its docs, not run |
| Open WebUI | Follows its docs, not run |

### Help check one

Each client marked "not run" is a good first contribution if you already use it. You need no Go knowledge to report the result, and none of this needs a real model on your side beyond the one you already run:

1. Run `local-base connect <client> --create-key` and follow what it prints.
2. Send a chat message from the client, with streaming on if the client streams.
3. Open an issue or a pull request saying which client and version you used, what worked, and what did not.

If it works, the change is one line: set `Run: true` for that client in `internal/connect/clients.go`, and add it to the list in `TestClientsSayWhetherTheyWereRun`. If a field is wrong, fix the snippet there too.

## What will not work

- **Anything that needs `/v1/responses`, `/v1/completions` or Anthropic's `/v1/messages`.** LocalBase serves chat completions. As of the last check, Codex CLI only speaks the Responses API.
- **A key used from a web page.** A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key, and there are no CORS headers. Run your code on a server or in Node.
- **A service that calls the address from its own servers.** It cannot reach `127.0.0.1` on your computer, only a public address. Cursor works this way, and its forum also reports that Agent mode needs the Responses API.
- **Open WebUI in Docker using `127.0.0.1`.** That is the container itself: use an address the container can reach.

If you publish LocalBase through Cloudflare Access, an app also has to get past Access at the edge, with a service token or a bypass rule for `/v1/*`. LocalBase still checks the key.
