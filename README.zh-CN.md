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

### 后端池

pool 把一个逻辑模型 alias 映射到有序的后端模型 id 列表，仅当 active 后端返回限流时才轮换。
选择是 sticky 的（不做 round-robin），以保留上游 prefix/KV cache。命中限流的那个请求返回
`429`，下一个请求（或客户端自己的重试）使用被提升的后端。

pool 会编译成一条受控的 alias 规则（数据面没有专门的 handler）；被动上报器把失败交给控制面
控制器，控制器轮换目标并在内存中重编译路由。

pool 会周期性 **探针**（合成流式请求）并按测得的 **TTFT** 排序，因此便宜但慢的 plan 会输给更快的
那个。`policy` 决定排序键：`speed_first`（默认，TTFT 优先）或 `price_first`（价格优先；挂 plan 的模型使用折算后的每 token 价格）。

每个后端有 4 态健康：`normal`、`slow`、`ratelimit`、`dead`（服务端错误或余额耗尽）。探针是健康
判定器：失败则冷却/判死，成功则恢复。`fallback` 是可选的最后兜底目标（另一个 pool 或普通 model
id），仅当该 pool 所有后端都不可用时使用；它不参与探针。

```yaml
pools:
  - model: deepseek-v4.1
    backend: [aaa/deepseek-v4.1, bbb/deepseek-v4.1, deepseek-v4.1]
    policy: speed_first        # 或 price_first
    reselect_ttft_ms: 5000     # 探针 ttft 超过即判 slow/重选
    probe_interval_min: 30     # 探针周期
    cooldown_sec: 60
    fallback: deepseek-v4.1-lite   # 全挂时的兜底 pool 或 model
```

pool 与其他实体使用同一套 API/CLI 形态（如 `GET /api/pool/list`、
`PATCH /api/pool/patch/{model}`）。

### Pool 事件（webhook）

pool 的状态迁移可以推送到 webhook（如 node-red、机器人或事件平台）。配置
`events.webhook_url`（也可在运行时通过 `events` setting 修改）；投递是 fire-and-forget，
带有限缓冲，慢的接收端不会阻塞网关。仅当 **active 后端发生变化** 时才触发：

```json
{"event":"pool.downgrade","pool":"deepseek-v4.1","from":"m2","to":"fb",
 "reason":"fault:rate_limit","downgraded":true,"at":"2026-10-13T15:43:24Z",
 "health":{"m1":"ratelimit","m2":"ratelimit"}}
```

`event` 取值：`pool.downgrade`（全部后端不可用，落到底）、`pool.fault`（发生故障后切到其他
后端）、`pool.recover`、`pool.reselect`、`pool.rotate`、`pool.change`。

```yaml
events:
  webhook_url: "https://example.com/hooks/lmgateway"
  timeout_sec: 10
```

### 访问控制（公共命名空间 + key）

网关**只授权一次**：针对显式的*公共命名空间*——客户端可调用的名字集合。每个公共名带自己的
`allowed_groups`（`denied_groups` 优先；空 = 任意已认证主体）。路由图（pool 后端、alias 目标、
provider 绑定）是**内部图，不含 ACL**——它只是 ACL-blind 的名字改写。

公共名包括：model（默认）、pool alias、alias 规则、以及 `[provider]/model` 前缀（provider 默认
public）。把 model 标为 `public: false` 即变成内部路由目标（如 pool 后端），客户端不可直接寻址。

```yaml
models:
  - name: gpt-5
    provider: openai
    allowed_groups: [eng]        # 公共名 + ACL
  - name: openai/gpt-5.6
    provider: openai
    upstream_model: gpt-5.6
    public: false                # 内部后端（经 pool 访问）
pools:
  - model: smart                 # 公共名
    backend: [openai/gpt-5.6]
    allowed_groups: [eng]
rules:
  - id: alias-agent              # alias 也是公共名
    from: http
    match: [{field: model, op: eq, value: agent}]
    set: {model: gpt-5}
    to: http
    allowed_groups: [ops]
keys:
  - { id: ci, key_hash: "<sha256>", groups: [eng] }
```

master key 映射为 **root**（全通过）。key 经哈希查得主体，其 group = key 的 groups + namesake
组（自身 id），因此资源可用 `allowed_groups: [ci]` 单独授权某把 key。用
`lmgcli key create ci --group eng` 创建（明文只返回一次）。存在于路由但不在公共命名空间的名字
返回 `403`；完全未知返回 `404`。数据面审计即 `observe` 的 OTLP trace。

### Key tag 与 route hint

key 可以带一个**不参与鉴权**的、类 URL query 的后缀：

```http
Authorization: Bearer sk-lm-xxxx#claude-code&pool_policy=price_first
```

- `claude-code`（第一个不含 `=` 的段）是归因用的 `key_tag`。
- `pool_policy=price_first` 是 route hint，写入 `route_hints` map。
- 字面量 `&`/`=` 需要百分号编码（`%26`/`%3D`）；重复 hint key 以最后一个为准。
- body 里的 `key_tag`（字面字符串）和 `route_hint`（query 字符串或 `route_hints` 对象）分别独立覆盖后缀。

Pool 消费 `route_hints.pool_policy`：每个 pool 按各排序策略从 health/price/TTFT 状态预编译一条路由规则，
`pool_policy=price_first|speed_first`（别名 `price`/`speed`）选择对应排序。没有 hint、未知策略或
请求的是普通 model 时，规则不匹配，自然落到默认路由（软匹配）。Pool 的 Status 页会显示每个
policy 选中的后端。

`key_tag` 会写入 spend 记录，并在控制台 Spend 页的 `By key tag` 展示。tag 不参与 key 查询、
`groups` 或 ACL；`key_tag`、`route_hint`、`route_hints` 在发往上游前都会被剥离。tag 由客户端
声明，不允许空白；`key_tag` 最长 128 字节。规则匹配 `route_hint` 或 `route_hints.<key>`
（例如 `route_hints.pool_policy`），而不是 `key_tag`。

### OpenCode session headers

OpenCode 发模型请求时会带 `x-opencode-session`、`x-opencode-session-id`、
`x-opencode-project`、`x-opencode-client`、`x-session-affinity`、`X-Session-Id` 和
`x-parent-session-id`。网关把这组白名单头透传给 OpenAI 类型上游（包括 OpenCode Zen/Go）。
如果都没有 session 值，会依次使用 `x-opencode-session-id`、`X-Session-ID`，最后用 key id +
User-Agent + project 生成一个稳定的 `ses_<sha256>`，保证要求 `x-opencode-session` 的上游总能收到。
任意客户端头和网关的 Authorization 头不会被转发。

### Plan 与速率限制

provider 可以挂一个订阅 **plan**。plan 以 provider 名为 key，没有单独的 plan id。
`plan` 支持 preset 字符串或对象：

```yaml
providers:
  - name: opencode_go
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    api_key_env: OPENCODE_GO_API_KEY
    plan: opencode_go          # $10/月；按请求 usage × 1

  - name: codex
    type: chatgpt_codex
    base_url: https://chatgpt.com/backend-api/codex
    plan:
      type: chatgpt_plus       # $20/月；按 weekly window share
      price: 20
```

内置 preset 在 `internal/config/plan_presets.json`：

| preset | 计费方式 | 含义 |
| --- | --- | --- |
| `opencode_go` | `multiplier` | OpenCode Go（$10/月）；`cost = 列表成本 × (10 / 模型月额度)` |
| `opencode_go_plus` | `multiplier` | OpenCode Go Plus（$40/月）；token 单价相同，`40 / 月额度` |
| `chatgpt_plus` | `window_share` | ChatGPT Plus/Codex（$20/月）；按 weekly 已用比例折算 |
| `chatgpt_pro` | `window_share` | ChatGPT Pro/Codex（$200/月） |

OpenCode Go 的模型价格和倍率从公开模型目录
（`https://opencode.ai/inference/go/v1/models`）生成。生成器读取 `cost.text.*`
和 `go.monthly_usage`，计算 `multiplier = plan 月费 / 模型月额度`；用满某模型的
月额度时，最多折算成 plan 月费。更新后提交 JSON：

```bash
go generate ./internal/config
# 或：go run ./cmd/genplanpresets
go run ./cmd/genplanpresets -check   # CI：检查是否过期
```

生成器**不包含 Zen**：Zen 是 pay-as-you-go，不是订阅 plan。价格取目录里的基础
token 价，不建模上游的分时段 multiplier。plan 月费写死在生成器里，OpenCode 调价时
需要同步修改。

`spend.cost` 是主页/趋势使用的 plan 折算金额；`spend.list_cost` 保留上游列表价。
multiplier plan 的 `plan_cost` 与 `cost` 相同。不做折算的话，$10 的 Go plan 在
主页会显示成几百美元的列表价 usage。

Pool 的 `price_first` 也使用折算后的每 token 价格（列表价 × plan multiplier）。
没有 per-token 价格的 plan（例如 Codex window-share）在 pool 排序里仍为 0。

价格优先级：显式 `model.input_per_mtok` / `model.output_per_mtok` → plan preset
模型目录 → 无价格。preset 只填充已有 model 的空价格，不会自动创建 model 实体；
发现出来的模型则直接查 plan 目录，因此 OpenCode Go 的模型不需要 `model` 条目也能
得到真实 `plan_cost`。

每个 provider 响应（包括 429/402）都会产生 rate-limit 事件：

- OpenAI 兼容 provider 把所有上游响应头原样放进 packet；plan manager 解析
  `ratelimit-*` / `x-ratelimit-*`。
- Codex 解析 `x-codex-*` 响应头和 `codex.rate_limits` SSE 事件，得到
  primary/secondary window、reset、credits、plan type 的类型化快照。
- OpenCode Go **不返回** rate-limit 响应头。网关每分钟用 provider key 请求
  `GET https://opencode.ai/zen/go/v1/usage`，从响应里取 `rolling`（5 小时）、
  `weekly`、`monthly` 的百分比和 reset 时间。

usage collector 同时写普通 `spend` 记录和 rate-limit 事件。multiplier plan 的
`spend` 记录带 `plan`、`multiplier`、`list_cost`、`cost`、`plan_cost` 字段。
`GET /api/plans/status` 返回最新窗口和成本；OpenCode 的 rolling/weekly/monthly
窗口来自 usage 轮询，Codex 来自响应头/SSE。console 的 **Plans / limits** 页面
展示这些数据。当前只做
可见性和记账，不做硬配额拦截。

## 支持的后端

| `type` | 后端 | 说明 |
| --- | --- | --- |
| [`openai`](https://github.com/lin-calvin/lmgateway/wiki/Provider-openai) | 任意 OpenAI 兼容 Chat API | OpenAI、DeepSeek、火山引擎、OpenRouter、GLM/ZAI…；`[provider]/model` 始终可路由；`discover: true` 额外拉取 `/models` |
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
lmgcli pool clear deepseek-v4.1    # 清除全部故障
lmgcli pool recover deepseek-v4.1 aaa/deepseek-v4.1   # 把某个后端重置为 normal
```

Shell 补全（bash/zsh）会提示命令、参数、运行时名称和 alias 键，并包含从
`/v1/models` 发现的模型：

```bash
source <(lmgcli completion bash)
# zsh
lmgcli completion zsh > "${fpath[1]}/_lmgcli"
```

## Web 控制台

管理控制台内嵌在二进制中，通过 `/ui/` 提供——无需构建步骤、不依赖 CDN。它与 CLI 一一对应：相同的资源组（Provider、Model、Pool、Key、Rule、Alias、Setting）、相同的动词（`list/get/set/patch/delete/reset`、pool 的 `status/rotate/clear/recover`、`key create`），以及相同的 Codex 登录流程。左侧为资源分组，中间为资源列表，点击某一行打开详情/操作抽屉（表单 + 原始 JSON）。

它调用与 `lmgcli` 相同的 JSON 管理 API。打开 `http://<addr>/ui/`，在顶栏粘贴网关 key（保存在浏览器 `localStorage`）；鉴权关闭时无需 key 也能使用。原 `/ui/codex/` 地址会重定向到控制台。

Pool 详情默认打开 **Status** 标签页（运行时 active 后端 + 每个后端的 health/TTFT/cost/samples/cooldown），旁边是 **Fields**、**ACL** 与 **JSON**。每个资源的抽屉都有 **ACL** 标签页，只编辑它共享的 ACL 字段（`public`、`allowed_groups`、`denied_groups`；key 用 `groups`），不会误改其它配置。Pool 列表的每个 model 行上直接提供 **Rotate / Clear / Recover** 按钮，Status 表里每个后端还有单独的 Recover。Spend 的 token 总量以 K/M/B 紧凑显示，悬停可看精确值；**By key tag** 表按客户端/用途拆分用量；**Trend** 折线图在单日按小时、7d/30d/90d/1y 按天展示 tokens / cost / requests。

规则（Rules）提供 **表格 / 图** 切换：图视图把编译后的规则表渲染为有向图（位置节点、handler 边、终态），自环与回边以曲线表示；点击边即可打开对应规则。

## 许可证

[MIT](LICENSE)
