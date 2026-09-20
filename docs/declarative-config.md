# Declarative configuration

`local-base config` manages persisted gateway, runtime, model, and memory settings without prompts. All commands accept `--root <directory>` and `--json`. They ignore runtime environment overrides when reading or writing desired state. `--root` and `LOCALBASE_ROOT` still select the data directory.

Export an initialized installation, edit the document, and review the changes:

```sh
local-base config show > local-base.toml
local-base config validate --file local-base.toml
local-base config plan --file local-base.toml --detailed-exit-code
local-base config apply --file local-base.toml --restart auto --wait
```

`validate`, `plan`, and `apply` require `--file`. Use `--file -` to read stdin. Validation and planning do not initialize the data directory. Apply can initialize an empty directory; a nonempty directory without a LocalBase root marker is rejected.

On a database created by an older binary, `apply` runs the generated schema migrations under the operation lock after validating the document. Read-only `plan` and `show` require the current schema. Upgrade it first through the normal gateway startup or `local-base configure --defaults --no-create-key`.

The version 1 document is complete for the supported settings. Every field below is required, unknown fields are rejected at every level, and values are not coerced. Model IDs must belong to the matching catalog modality, selected lists cannot contain duplicates, and a nonempty active model must be selected. Empty lists disable optional modalities. LLM selection must remain nonempty.

```toml
version = 1

[gateway]
host = "127.0.0.1"
port = 2273

[runtime]
host = "0.0.0.0"
port = 18000
ctxSize = 131072
parallel = "auto"
sttHost = "0.0.0.0"
sttPort = 18080

[models]
selectedLlmModels = ["qwen2.5-coder-7b-instruct-q4_k_m"]
selectedSttModels = ["whisper-base-q8_0"]
selectedTtsModels = []
selectedImageModels = []
selectedVideoModels = []
activeLlmModel = "qwen2.5-coder-7b-instruct-q4_k_m"
activeSttModel = "whisper-base-q8_0"
activeTtsModel = ""
activeImageModel = ""
activeVideoModel = ""

[memory.systemReserve]
percent = 15
minimumGb = 8

[memory.acceleratorReserve]
percent = 10
minimumGb = 2
```

`gateway` controls the public listener. Explicit `serve --host` and `serve --port` flags override its persisted defaults for that process. `runtime.host` and `runtime.port` control the LLM backend; the STT fields control the STT backend. `parallel` accepts `"auto"` or integers 1 through 4. `ctxSize` accepts integers from 2048 through 2147483647. Memory percentages range from 0 through 100; minimum reserves are nonnegative GiB values.

`show` emits canonical TOML with selected model lists sorted. Secrets, API keys, telemetry configuration, root paths, and process-only flags are omitted. Applying the export preserves those existing values. There are no secret placeholders that could overwrite credentials. `show --json` returns the TOML string as `data.document` and activation status as `data.pendingRestart` inside the usual `{ "ok": true, "data": ... }` envelope. Human output reports pending activation on stderr so redirected TOML remains re-applicable.

Plans compare semantic values with persisted state, ignoring TOML formatting, table order, and selected-model ordering. Changes are sorted by field path and include `before`, `after`, and `activation`. For a new installation, `before` is `null`. Plan exits 0 on success unless `--detailed-exit-code` is set; with that flag it exits 0 for no changes and no pending restart, and 2 for configuration changes or pending activation. Config command errors exit 1 and use the standard JSON error envelope when requested.

| Settings | Activation |
| --- | --- |
| Runtime launch settings and model selection | Hot, through existing runtime reconciliation |
| Gateway listener and memory policy | Gateway restart required |

Apply validates before mutation, reads the current state under the root operation lock, and commits the configuration row in one SQLite transaction. A repeated apply skips the settings write when semantic values match. It does not install model artifacts; use `models install` separately. A hot change may drain and replace an affected runtime. A pending static change blocks runtime reconciliation until the gateway restarts.

| Restart policy | Behavior |
| --- | --- |
| `auto`, the default | Start or restart the managed service when static configuration changes or a durable pending record require it, including unchanged re-applies. Refuse automatic restarts when ownership is foreground, unknown, or stopping, or when the service manager is unavailable. |
| `always` | Start or restart through the service manager, including when configuration is unchanged. Refuse unsafe ownership or an unavailable manager. |
| `never` | Save settings without restarting. Report `restart-required` when static settings changed. |

`--wait` checks gateway identity, health, and public `/health/ready` admission readiness for up to 30 seconds. It requires a running gateway with no restart changes in this apply, or a restart that this apply performs. For pending activation, it also waits for the managed gateway to acknowledge the saved static settings. Readiness does not mean every model is loaded or every inference request will succeed. Process overrides continue to take precedence.

Static changes store a pending-restart fingerprint in the same transaction as the settings. This metadata is separate from the desired-state document. Plan and apply expose `pendingRestart` even when `changed` is false. A later `apply --restart auto --wait` starts or restarts the managed service; `local-base start` also acknowledges pending settings when it starts a stopped service. An unchanged `never` apply preserves the record.

Only a managed gateway that has bound its listener and can admit requests acknowledges its startup settings. The acknowledgement checks both the pending fingerprint and the current saved static settings, so an older process cannot clear a newer change. Explicit listener overrides that differ from saved settings leave activation pending. If restart or readiness fails, apply exits 1, reports that configuration was saved, and retains pending activation for retry. It does not roll back persisted state behind a potentially running service.

The interactive `configure` command shares validation and transactional persistence for these settings. Its existing prompts, secret handling, and optional API-key creation remain separate from the declarative document.
