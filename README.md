# lmgateway

**A lightweight, highly extensible LLM gateway.**

[English](README.md) · [中文](README.zh-CN.md)

lmgateway is a single static Go binary that sits in front of any number of LLM
backends and exposes one clean, OpenAI-compatible API. It routes requests with a
small composable pipeline, owns each provider's authentication and protocol
quirks, and records usage along the way. No Python, no sidecars, no heavy
dependencies.

## Features

- **Lightweight** — one static binary (CGO disabled, distroless image). SQLite
  for development, Postgres for production.
- **OpenAI-compatible** — `POST /v1/chat/completions`, native
  `POST /v1/responses`, and `GET /v1/models` with live provider discovery.
- **Highly extensible** — requests flow through a `from → match → action → to`
  dispatcher; a provider only implements its own auth, endpoint and protocol
  translation. Add a backend without touching the core.
- **Config as code** — a YAML baseline plus database overrides, hot-reloaded on
  write. Rules can alias models (`agent → gpt-5.6-luna`) or default parameters.
- **Observable** — per-request spend accounting, Prometheus `/metrics`, OTLP
  traces, and a passive logprobs webhook.
- **Managed** — an HTTP management API and the `lmgcli` command line client.

## Build

Requires Go 1.26+.

```bash
go build -o gateway ./cmd/gateway
go build -o lmgcli  ./cmd/lmgcli
```

Container image:

```bash
docker build -t lmgateway .
```

## Run

```bash
./gateway --config config/lmgateway.yaml --db sqlite://data/gateway.db --addr :8080
```

Flags override the matching environment defaults:

| Flag | Environment | Default |
| --- | --- | --- |
| `--config` | `LMGATEWAY_CONFIG` | `config/lmgateway.yaml` |
| `--db` | `LMGATEWAY_DB_DSN` | `sqlite://data/gateway.db` |
| `--addr` | — | `server.addr` from config, else `:8080` |

`--db` accepts `sqlite://path` or `postgres://user:pass@host/db`.

Authentication is enabled when a master key is configured — either
`server.master_key` in the YAML or the `LMGATEWAY_API_KEY` environment variable
(YAML wins). Clients send `Authorization: Bearer <key>` or `X-API-Key: <key>`.
With no key configured, authentication is disabled (fine for local development).
`/healthz` and `/metrics` are always public. `LMGATEWAY_CORS_ORIGINS` is a
comma-separated allow-list for browser clients.

Docker:

```bash
docker run -p 8080:8080 \
  -e LMGATEWAY_API_KEY=sk-local \
  -v "$PWD/config:/app/config:ro" \
  lmgateway
```

First request:

```bash
curl -s https://llm.example.com/v1/chat/completions \
  -H "Authorization: Bearer $LMGATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## Configuration

The gateway reads a YAML baseline at startup and reconciles it into the store.
The database holds only overrides and database-only entities, so editing the
YAML never silently overwrites them. The full reference lives on the wiki:
**[Configuration](https://github.com/lin-calvin/lmgateway/wiki/Configuration)**.

```yaml
server:
  addr: ":8080"
  master_key: "sk-local"          # optional; empty = auth disabled

providers:
  - name: deepseek
    type: openai
    base_url: https://api.deepseek.com/v1
    api_key_env: DEEPSEEK_API_KEY
    discover: true

models:
  - name: deepseek-v4-flash
    provider: deepseek
    upstream_model: deepseek-chat

rules:
  - id: alias-agent
    from: http
    match: [{ field: model, op: eq, value: agent }]
    set: { model: deepseek-v4-flash }

spend:
  raw_retention_days: 7
  rollup_dimensions: [provider, model, stream]
  timezone: Asia/Shanghai
```

### Backend pools

A pool maps one logical model alias to an ordered list of backend model ids and
rotates only when the active backend returns a rate limit. Selection is sticky
(no round-robin), so upstream prefix/KV cache is preserved. The request that hit
the limit returns `429`; the next request (or the client's own retry) uses the
promoted backend.

A pool compiles to a managed alias rule (no dedicated data-path handler); a
passive reporter feeds a control-plane controller, which rotates the target and
recompiles routing in memory.

```yaml
pools:
  - model: deepseek-v4.1
    backend: [aaa/deepseek-v4.1, bbb/deepseek-v4.1]
    cooldown_sec: 60
    on_all_limited: fail        # fail | force-least-recent
```

Manage pools with the same API/CLI shape as other entities (e.g.
`GET /api/pool/list`, `PATCH /api/pool/patch/{model}`).

## Supported backends

| `type` | Backend | Notes |
| --- | --- | --- |
| [`openai`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai) | Any OpenAI-compatible Chat API | OpenAI, DeepSeek, Volcengine, OpenRouter, GLM/ZAI, …; `discover: true` pulls `/models` and routes `[provider]/model` |
| [`openai_response`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai-response) | OpenAI Responses-protocol upstream | |
| [`chatgpt_codex`](https://github.com/lin-calvin/lmgateway/wiki/Provider-chatgpt-codex) | ChatGPT subscription | Device OAuth login at `/ui/codex/` |
| [`commandcode`](https://github.com/lin-calvin/lmgateway/wiki/Provider-commandcode) | commandcode.ai coding plan | `/alpha/generate` envelope + fingerprint/lifecycle |

Downstream protocols: OpenAI Chat Completions and Responses. Minimal YAML
examples, options and gotchas are on each wiki page.

## Extending

Providers, routing and accounting are separate layers:

- the **packet** carries request/response documents;
- the **dispatcher** compiles `from`/`match`/`action`/`to` rules into a pipeline;
- a **provider** implements `Handle` (and optionally `Discover`) for one backend;
- **handlers** such as `usage`, `logprobs` and `stream` observe or finalize a
  packet.

See [`docs/DISPATCHER_FLOW_DESIGN.md`](docs/DISPATCHER_FLOW_DESIGN.md) for the
design and [`docs/API.md`](docs/API.md) for the HTTP surface.

## CLI

`lmgcli` is a thin wrapper over the management API (it never touches the
database):

```bash
lmgcli health
lmgcli provider list
lmgcli model get deepseek-v4-flash
lmgcli rule patch alias-agent --file patch.yaml
lmgcli config reload
```

Point it at a server with `--server` or `LMGATEWAY_SERVER`.

Aliases have a dedicated, positional command (an alias is a rule with the
`alias-<name>` convention):

```bash
lmgcli alias list
lmgcli alias set agent model gpt-5.6-luna-fast
lmgcli alias set agent-max model gpt-5.6-luna-fast effort max summary concise
lmgcli alias unset agent-max summary
```

Shell completion (bash/zsh) suggests commands, flags, live names and the alias
keys, including models discovered from `/v1/models`:

```bash
source <(lmgcli completion bash)
# zsh
lmgcli completion zsh > "${fpath[1]}/_lmgcli"
```

## License

[MIT](LICENSE)
