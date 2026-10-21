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

Pools periodically **probe** each backend (a synthetic streaming request) and rank
them by measured **TTFT**, so a cheap-but-slow plan loses to a faster one.
`policy` picks the key order: `speed_first` (default, TTFT primary) or
`price_first` (price primary; subscription plans count as free).

Each backend has a health state — `normal`, `slow`, `ratelimit`, or `dead`
(server error *or* ran out of balance). The probe is the health oracle: a failure
cools/deadens the backend, a success revives it. `fallback` is an optional
last-resort target (another pool or a plain model id), used only when every
backend of the pool is unavailable; it is never probed.

```yaml
pools:
  - model: deepseek-v4.1
    backend: [aaa/deepseek-v4.1, bbb/deepseek-v4.1, deepseek-v4.1]
    policy: speed_first        # or price_first
    reselect_ttft_ms: 5000     # probe ttft above this → slow/reselect
    probe_interval_min: 30     # synthetic probe cadence
    cooldown_sec: 60
    fallback: deepseek-v4.1-lite   # pool or model, when all backends are down
```

Manage pools with the same API/CLI shape as other entities (e.g.
`GET /api/pool/list`, `PATCH /api/pool/patch/{model}`).

### Pool events (webhook)

Pool transitions can be pushed to a webhook (e.g. node-red, a bot, or an
incident tool). Set `events.webhook_url` (also editable at runtime as the
`events` setting); delivery is fire-and-forget with a bounded buffer, so a slow
receiver never blocks the gateway. Events fire only when the **active backend
changes**:

```json
{"event":"pool.downgrade","pool":"deepseek-v4.1","from":"m2","to":"fb",
 "reason":"fault:rate_limit","downgraded":true,"at":"2026-10-13T15:43:24Z",
 "health":{"m1":"ratelimit","m2":"ratelimit"}}
```

`event` is one of `pool.downgrade` (active fell back because all backends are
unavailable), `pool.fault` (active moved to another backend after a fault),
`pool.recover`, `pool.reselect`, `pool.rotate`, or `pool.change`.

```yaml
events:
  webhook_url: "https://example.com/hooks/lmgateway"
  timeout_sec: 10
```

### Access control (public namespace + keys)

The gateway authorizes **once**, against an explicit *public namespace*: the set
of names a client may call. Each public name carries its own `allowed_groups`
(`denied_groups` wins; empty `allowed_groups` = any authenticated caller). The
routing graph (pool backends, alias targets, provider bindings) is **internal and
carries no ACL** — it is a pure, ACL-blind name rewriter.

A public name is: a model (default), a pool alias, an alias rule, or a
`[provider]/model` prefix (providers are public by default). Mark a model
`public: false` to make it an internal routing target (e.g. a pool backend) that
clients cannot address directly.

```yaml
models:
  - name: gpt-5
    provider: openai
    allowed_groups: [eng]        # public name + ACL
  - name: openai/gpt-5.6
    provider: openai
    upstream_model: gpt-5.6
    public: false                # internal backend (reachable via a pool)

pools:
  - model: smart                 # public name
    backend: [openai/gpt-5.6]
    allowed_groups: [eng]

rules:
  - id: alias-agent              # aliases are public names too
    from: http
    match: [{field: model, op: eq, value: agent}]
    set: {model: gpt-5}
    to: http
    allowed_groups: [ops]

keys:
  - { id: ci, key_hash: "<sha256>", groups: [eng] }
```

The master key maps to **root** (passes everything). Keys hash-lookup to a
principal whose groups are the key's groups plus a namesake group (the key id),
so a resource can grant to a single key with `allowed_groups: [ci]`. Create keys
with `lmgcli key create ci --group eng` (plaintext returned once). Names not in
the public namespace but present in routing return `403`; unknown names `404`.
Data-plane audit is the OTLP trace from `observe`.

## Supported backends

| `type` | Backend | Notes |
| --- | --- | --- |
| [`openai`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai) | Any OpenAI-compatible Chat API | OpenAI, DeepSeek, Volcengine, OpenRouter, GLM/ZAI, …; `[provider]/model` always routes; `discover: true` also pulls `/models` |
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

Backend pools (config CRUD plus runtime status/rotate/clear):

```bash
lmgcli pool list
lmgcli pool set deepseek-v4.1 --json '{"model":"deepseek-v4.1","backend":["aaa/x","bbb/x"],"cooldown_sec":60}'
lmgcli pool status deepseek-v4.1   # active backend + cooling members
lmgcli pool rotate deepseek-v4.1   # force the next healthy backend
lmgcli pool clear deepseek-v4.1    # drop all faults
lmgcli pool recover deepseek-v4.1 aaa/deepseek-v4.1   # reset one backend to normal
```

Shell completion (bash/zsh) suggests commands, flags, live names and the alias
keys, including models discovered from `/v1/models`:

```bash
source <(lmgcli completion bash)
# zsh
lmgcli completion zsh > "${fpath[1]}/_lmgcli"
```

## Web console

An admin console is embedded in the binary and served at `/ui/` — no build step
and no CDN. It mirrors the CLI one-to-one: the same resource groups (Provider,
Model, Pool, Key, Rule, Alias, Setting), the same verbs (`list/get/set/patch/
delete/reset`, pool `status/rotate/clear/recover`, `key create`), and the same
Codex login flow. The left panel lists resources, the middle is the resource
list, and clicking a row opens a detail/actions drawer (fields + raw JSON).

It talks to the same JSON management API as `lmgcli`. Open
`http://<addr>/ui/`, paste a gateway key into the top bar (kept in the
browser's `localStorage`), and it also works with no key when auth is disabled.
The old `/ui/codex/` URL redirects into the console.

Pools open on a **Status** tab (runtime active backend plus per-backend
health/TTFT/cost/samples/cooldown), with **Fields**, **ACL** and **JSON** beside
it. Every resource drawer also has an **ACL** tab for the fields it shares
(`public`, `allowed_groups`, `denied_groups`; keys use `groups`), so an ACL edit
never touches the rest of the config. Pools expose quick **Rotate / Clear /
Recover** buttons on each model row, plus a per-backend Recover in the Status
table. Spend totals use compact token counts (K/M/B) with the exact value on
hover.

Rules have a **Table / Graph** toggle: the graph view renders the compiled rule
table as a directed graph (positions, handler edges, terminals), with self-loops
and back edges drawn as curves. Clicking an edge opens that rule.

## License

[MIT](LICENSE)
