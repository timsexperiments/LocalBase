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

## Limits: what will not work

- **Anything that needs `/v1/responses`, `/v1/completions` or Anthropic's `/v1/messages`.** LocalBase serves chat completions. As of the last check, Codex CLI only speaks the Responses API.
- **A key used from a web page.** A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key, and there are no CORS headers. Run your code on a server or in Node.
- **A service that calls the address from its own servers.** It cannot reach `127.0.0.1` on your computer, only a public address. Cursor works this way, and its forum also reports that Agent mode needs the Responses API.
- **Open WebUI in Docker using `127.0.0.1`.** That is the container itself: use an address the container can reach.
- **Anything beyond chat, with a key from `--create-key`.** It can chat and list models, and nothing else. For more, choose its permissions with `--scopes`. Give an app only what it needs, and never a `manage` permission, which lets whoever holds the key change models, keys or who can sign in.
- **A web address for an image or other media in a chat request.** A content part carries its content, so send a `data:` URL or base64. An address answers 400 `remote_media_not_allowed`.

If you publish LocalBase through Cloudflare Access, an app also has to get past Access at the edge, with a service token or a bypass rule for `/v1/*`. LocalBase still checks the key.

## Privacy

- **`local-base connect` uses no network,** and changes nothing unless you pass `--create-key`. Without it, the snippets hold the placeholder `YOUR_LOCALBASE_KEY`.
- **A new key is shown once.** LocalBase keeps only its hash, so it can't show it again. A file you send a snippet into holds the key in plain text, so keep that file private and out of version control. Unless you pass `--expires-days`, the key doesn't expire. Revoke it with `local-base keys revoke <keyId>`.
- **Creating a key goes in the audit log,** without its secret.
- **A chat request from an app isn't stored.** LocalBase [counts it](usage.md), and keeps no prompt or reply. What the client does with them, such as keeping a history or also using another service, is up to the client.

## Troubleshooting

| It says, or you see | What to do |
| --- | --- |
| `Unknown client "<name>". Expected one of: …` | Use a name from that list, or see them all with `local-base connect --list` |
| `--name only applies with --create-key` | The same goes for `--scopes` and `--expires-days`: add `--create-key`, or leave them off |
| `model: no chat model is installed yet.` | Run `local-base models install <id>` |
| `model: "<id>" is not an installed chat model. Expected one of: …` | Use an id from that list |
| Connection refused | Check `local-base status`. On another computer or in a container, `127.0.0.1` isn't this computer, so use an address it can reach |
| `401` with `invalid_api_key` | The key is missing or not accepted. A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key, so this is also what a web page gets |
| `403` with `insufficient_permissions` | The key lacks the permission. A key from `--create-key` has only `inference:chat` and `models:read`, unless you chose `--scopes` |
| `404` with `model_not_found` | The app is asking for an id that isn't there. Check `GET /v1/models` |
| `404` with `route_disabled` | The app uses a route LocalBase doesn't serve, such as `/v1/responses`, `/v1/completions` or `/v1/messages`. See [Limits](#limits-what-will-not-work) |

The full list of answers is in the [API reference](api-reference.md).
