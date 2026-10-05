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
  # 额外汇总维度；tenant/project/key/provider/model/stream 始终参与日汇总
  # （否则按租户/密钥查历史、以及重启后的配额校准会失去依据）
  rollup_dimensions: [provider, model, stream]
  timezone: Asia/Shanghai
```

### 后端池

pool 把一个逻辑模型 alias 映射到有序的后端模型 id 列表，仅当 active 后端返回限流时才轮换。
选择是 sticky 的（不做 round-robin），以保留上游 prefix/KV cache。命中限流的那个请求返回
`429`，下一个请求（或客户端自己的重试）使用被提升的后端。

pool 会编译成一条受控的 alias 规则（数据面没有专门的 handler）；被动上报器把失败交给控制面
控制器，控制器轮换目标并在内存中重编译路由。

```yaml
pools:
  - model: deepseek-v4.1
    backend: [aaa/deepseek-v4.1, bbb/deepseek-v4.1]
    cooldown_sec: 60
    on_all_limited: fail        # fail | force-least-recent
```

pool 与其他实体使用同一套 API/CLI 形态（如 `GET /api/pool/list`、
`PATCH /api/pool/patch/{model}`）。

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

alias 有专门的位置参数命令（alias 就是遵循 `alias-<name>` 约定的 rule）：

```bash
lmgcli alias list
lmgcli alias set agent model gpt-5.6-luna-fast
lmgcli alias set agent-max model gpt-5.6-luna-fast effort max summary concise
lmgcli alias unset agent-max summary
```

后端池（配置 CRUD + 运行时 status/rotate/clear）：

```bash
lmgcli pool list
lmgcli pool set deepseek-v4.1 --json '{"model":"deepseek-v4.1","backend":["aaa/x","bbb/x"],"cooldown_sec":60}'
lmgcli pool status deepseek-v4.1   # active 后端 + 冷却成员
lmgcli pool rotate deepseek-v4.1   # 强制切到下一个健康后端
lmgcli pool clear deepseek-v4.1    # 清除冷却
```

Shell 补全（bash/zsh）会提示命令、参数、运行时名称和 alias 键，并包含从
`/v1/models` 发现的模型：

```bash
source <(lmgcli completion bash)
# zsh
lmgcli completion zsh > "${fpath[1]}/_lmgcli"
```

## 许可证

[MIT](LICENSE)
