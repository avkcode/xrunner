# LLM-backed chat backends (DeepSeek / Gemini via CometAPI)

`xrunner ssh chat` and `cmd/xrunner-tui` talk to `SessionService`. By default the server replies with a simple `"ack"`.

To get a Codex-like experience (streamed assistant output + tool calling), enable an LLM backend in `xrunner-remote`
via environment variables. Nothing is hard-coded: provider, base URL, model, and auth are all configured via env.

## Common env vars

- `XR_LLM_ENABLED=1` enable the LLM backend (otherwise server replies `"ack"`).
- `XR_LLM_PROVIDER=openai_compat|deepseek|gemini` backend type.
- `XR_LLM_BASE_URL=...` API base URL.
- `XR_LLM_API_KEY=...` API key/token.
- `XR_LLM_MODEL=...` model id.
- `XR_LLM_TIMEOUT_MS=60000` request timeout.
- `XR_LLM_STREAM=1` enable streaming deltas (default `true`).
- `XR_LLM_MAX_TOOL_ITERS=5` max tool-call iterations per user message.
- `XR_LLM_TOOL_OUTPUT_MAX_BYTES=65536` per tool output capture limit sent back to the model.
- `XR_LLM_MAX_HISTORY_EVENTS=50` how many past session events to include as prompt history.
- `XR_LLM_SYSTEM=...` optional system prompt.

Compatibility:

- If `XR_LLM_BASE_URL` / `XR_LLM_API_KEY` / `XR_LLM_MODEL` are unset, the server falls back to `OPENAI_BASE_URL` / `OPENAI_API_KEY` / `OPENAI_MODEL`.

## DeepSeek / OpenAI-compatible providers

Set:

- `XR_LLM_PROVIDER=openai_compat` (or `deepseek`)
- `XR_LLM_CHAT_PATH=/v1/chat/completions` (default)
- For the default chat path, set `XR_LLM_BASE_URL` **without** a trailing `/v1` (otherwise you end up calling `/v1/v1/...`).

Auth headers:

- Default: `Authorization: Bearer $XR_LLM_API_KEY`
- `xrunner-remote` always uses `Authorization: Bearer ...` for OpenAI-compatible providers (to avoid subtle env misconfigurations).

Tools:

- If the session has a tool bundle (`Session.tool_bundle_path`), the server passes its `tools` into the request and
  executes returned tool calls (`shell`, `write_file_atomic`) while streaming `ToolCallStarted/ToolOutputChunk/ToolCallResult`.

Clients:

- `cmd/xrunner-tui` and `xrunner ssh chat` accept `--tool-bundle` (default `$AGENT_TOOL_SPEC`) to enable tool calling.
  For `xrunner ssh chat`, if the path exists locally it is uploaded to the remote daemon automatically.
- When `xrunner ssh chat` auto-starts `xrunner-remote` (the default `--start-remote=true`), it forwards any locally-set
  `XR_LLM_*` and `OPENAI_*` env vars into the remote daemon process. This avoids needing to configure systemd/env files
  just to try chat.
- For local dev, `scripts/ssh_chat_local.zsh` sources `~/.zprofile` (for `OPENAI_*`) and runs `xrunner ssh chat` via `--proxy`.
- For one-off checks, `xrunner ssh chat --prompt "..."` sends a single prompt and exits. You can also use `--prompt-file`, `--prompt-stdin`, and `--prompt-json`.

## One-command provisioning (bootstrap)

To install `xrunner-api`, `xrunner-worker`, and `xrunner-remote` via systemd and persist an LLM backend config on the host:

```bash
export XR_LLM_ENABLED=1
export XR_LLM_PROVIDER=openai_compat
export XR_LLM_BASE_URL=...
export XR_LLM_API_KEY=...
export XR_LLM_MODEL=...

xrunner ssh bootstrap root@HOST --write-config --llm-from-env
```

This writes the resolved `XR_LLM_*` settings into `/etc/default/xrunner-remote` (mode `0600`) and starts the services.
If you use `--bin-dir`, it must contain **Linux** binaries for the remote architecture; otherwise systemd may fail with `status=203/EXEC`.

To avoid putting API keys in your shell history, use an env file instead:

```bash
xrunner ssh bootstrap root@HOST --write-config --llm-from-env-file ./llm.env
```

Bootstrap also snapshots the current binaries and unit/env files under `/var/lib/xrunner/backup/<epoch>/` and will attempt
an automatic rollback if the upgraded services fail to start.

## Non-root installs (no systemd)

If you don't have sudo/systemd access on the host, you can install into `~/.xrunner` and start the services via `nohup`:

```bash
xrunner ssh bootstrap user@HOST --install-mode user --llm-from-env-file ./llm.env
```

This starts:
- `xrunner-worker` on `127.0.0.1:50052`
- `xrunner-api` on `127.0.0.1:50051`
- `xrunner-remote` on `127.0.0.1:7337`

## Uninstall / purge

To completely remove xrunner services, binaries, config, and state from a host:

```bash
xrunner ssh uninstall root@HOST
```

This removes systemd units/env files (if present), deletes `/var/lib/xrunner`, `/etc/xrunner`, `/root/.xrunner`, and also
purges `~/.xrunner` for the SSH user (user-mode installs).

## Gemini via CometAPI (native Gemini format)

Base URL and auth per your spec:

- `XR_LLM_PROVIDER=gemini`
- `XR_LLM_BASE_URL=https://api.cometapi.com`
- `XR_LLM_API_KEY=$COMETAPI_KEY`
- `XR_LLM_MODEL=gemini-3-pro-preview`

Headers:

- Uses `x-goog-api-key: $XR_LLM_API_KEY` by default (override with `XR_LLM_GEMINI_KEY_HEADER`).

Streaming:

- If `XR_LLM_STREAM=1` and `XR_LLM_GEMINI_OPERATOR=generateContent` (default), the server automatically uses
  `streamGenerateContent?alt=sse` for best-effort SSE streaming.

Tool calling:

- Not implemented for Gemini yet (Gemini tool schema differs from OpenAI-compatible bundles).
