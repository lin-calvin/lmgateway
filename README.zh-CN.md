# lmgateway

**轻量、高度可扩展的 LLM 网关。**

[English](README.md) · [中文](README.zh-CN.md)

lmgateway 是一个单一静态 Go 二进制，置于任意数量的 LLM 后端之前，对外只暴露
一套干净的、OpenAI 兼容的 API。它用一条小巧可组合的流水线做路由，由每个
provider 自己负责鉴权与协议差异，并顺带记录用量。不需要 Python、边车或重型依赖。

## 特性

- **轻量** — 单个静态二进制（关闭 CGO，distroless 镜像）。开发用 SQLite，
  生产用 Postgres。
- **OpenAI 兼容** — `POST /v1/chat/completions`、原生 `POST /v1/responses`，
  以及带运行时 provider 发现的 `GET /v1/models`。
- **高度可扩展** — 请求经过 `from → match → action → to` 调度流水线；provider
  只需实现自己的鉴权、端点与协议转换，新增后端无需改动核心。
- **配置即代码** — YAML 基线 + 数据库覆盖，写入后热加载。规则可做模型别名
  （`agent → gpt-5.6-luna`）或参数默认值。
- **可观测** — 每请求用量计费、Prometheus `/metrics`、OTLP 链路、被动
  logprobs webhook。
- **可管理** — HTTP 管理 API 与 `lmgcli` 命令行客户端。

## 构建

需要 Go 1.26+。

```bash
go build -o gateway ./cmd/gateway
go build -o lmgcli  ./cmd/lmgcli
```

容器镜像：

```bash
docker build -t lmgateway .
```

## 运行

```bash
./gateway --config config/lmgateway.yaml --db sqlite://data/gateway.db --addr :8080
```

命令行参数覆盖对应的环境变量默认值：

| 参数 | 环境变量 | 默认值 |
| --- | --- | --- |
| `--config` | `LMGATEWAY_CONFIG` | `config/lmgateway.yaml` |
| `--db` | `LMGATEWAY_DB_DSN` | `sqlite://data/gateway.db` |
| `--addr` | — | 配置中的 `server.addr`，否则 `:8080` |

`--db` 支持 `sqlite://path` 或 `postgres://user:pass@host/db`。

配置了 master key 即开启鉴权 —— 来自 YAML 的 `server.master_key`，或环境变量
`LMGATEWAY_API_KEY`（YAML 优先）。客户端通过 `Authorization: Bearer <key>` 或
`X-API-Key: <key>` 传入。未配置 key 时关闭鉴权（适合本地开发）。`/healthz` 与
`/metrics` 始终公开。`LMGATEWAY_CORS_ORIGINS` 是给浏览器客户端的逗号分隔白名单。

Docker：

```bash
docker run -p 8080:8080 \
  -e LMGATEWAY_API_KEY=sk-local \
  -v "$PWD/config:/app/config:ro" \
  lmgateway
```

第一个请求：

```bash
curl -s https://llm.example.com/v1/chat/completions \
  -H "Authorization: Bearer $LMGATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "deepseek-v4-flash",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## 配置

网关启动时读取 YAML 基线并 reconcile 进存储。数据库中只保存覆盖项和仅存在于
数据库的对象，因此编辑 YAML 不会静默覆盖它们。完整参考见 wiki：
**[Configuration](https://github.com/lin-calvin/lmgateway/wiki/Configuration)**。

```yaml
server:
  addr: ":8080"
  master_key: "sk-local"          # 可选；留空 = 关闭鉴权

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

## 支持的后端

| `type` | 后端 | 说明 |
| --- | --- | --- |
| [`openai`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai) | 任意 OpenAI 兼容 Chat API | OpenAI、DeepSeek、火山引擎、OpenRouter、GLM/ZAI…；`discover: true` 拉取 `/models` 并路由 `[provider]/model` |
| [`openai_response`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai-response) | OpenAI Responses 协议上游 | |
| [`chatgpt_codex`](https://github.com/lin-calvin/lmgateway/wiki/Provider-chatgpt-codex) | ChatGPT 订阅 | 在 `/ui/codex/` 设备 OAuth 登录 |
| [`commandcode`](https://github.com/lin-calvin/lmgateway/wiki/Provider-commandcode) | commandcode.ai 编程套餐 | `/alpha/generate` 信封 + 指纹/lifecycle |

下游协议：OpenAI Chat Completions 与 Responses。最小 YAML 示例、选项与注意事项见
各 wiki 页面。

## 扩展

Provider、路由与计费是相互独立的层：

- **packet** 承载请求/响应文档；
- **dispatcher** 把 `from`/`match`/`action`/`to` 规则编译成流水线；
- **provider** 为某个后端实现 `Handle`（可选 `Discover`）；
- **handler**（如 `usage`、`logprobs`、`stream`）观察或收尾一个 packet。

设计见 [`docs/DISPATCHER_FLOW_DESIGN.md`](docs/DISPATCHER_FLOW_DESIGN.md)，
HTTP 接口见 [`docs/API.md`](docs/API.md)。

## CLI

`lmgcli` 是管理 API 的薄封装（从不直接读写数据库）：

```bash
lmgcli health
lmgcli provider list
lmgcli model get deepseek-v4-flash
lmgcli rule patch alias-agent --file patch.yaml
lmgcli config reload
```

用 `--server` 或 `LMGATEWAY_SERVER` 指向服务地址。

## 许可证

[MIT](LICENSE)
