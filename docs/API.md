# lmgateway API 归档

更新时间：2026-08-26

本文档记录当前 lmgateway 对外 API。示例使用占位符，不包含真实密钥、OAuth token 或上游凭据。

## 部署入口

当前公共入口：

```text
https://llm.svc.calvinlin.cn
```

当前 `llm.svc.calvinlin.cn` 的 Ingress 后端是 lmgateway，不再是 LiteLLM。LiteLLM deployment 已缩容到 0。

## 鉴权

应用优先读取启动参数指定 YAML 中的 `server.master_key`；如果 YAML 未配置，再回退到环境变量 `LMGATEWAY_API_KEY`。配置 master key 后，除 `/healthz` 和公开的 `/metrics` 外的接口都需要鉴权。

支持两种方式：

```http
Authorization: Bearer <LMGATEWAY_API_KEY>
```

或：

```http
X-API-Key: <LMGATEWAY_API_KEY>
```

YAML 和 `LMGATEWAY_API_KEY` 都未配置时关闭鉴权，适合本地开发。生产环境应配置非空 key。homelab Terraform 直接读取 `configs/lmgateway.yaml` 的 `server.master_key`，不再依赖 LiteLLM 配置文件，也不再通过 `LMGATEWAY_API_KEY` 环境变量注入。

鉴权失败响应：

```json
{
  "error": "authentication required"
}
```

HTTP 状态码：`401 Unauthorized`。

## CORS

HTTP 入口默认允许任意 Origin，支持浏览器跨域调用。预检 `OPTIONS` 请求不要求 API key。

可用环境变量限制 Origin，多个值用逗号分隔：

```text
LMGATEWAY_CORS_ORIGINS=https://app.example,https://admin.example
```

未配置或设置为 `*` 时返回：

```http
Access-Control-Allow-Origin: *
```

允许的方法：`GET, POST, PUT, PATCH, DELETE, OPTIONS`。

允许的请求头包括：`Authorization`、`Content-Type`、`X-API-Key`、`X-Session-ID`、`Session-ID`、`Originator`、`Accept-Language`、`X-Request-ID`。

预检成功响应状态码为 `204 No Content`。

## Prometheus Metrics

### `GET /metrics`

该 endpoint 不经过 API key 鉴权，供 Prometheus 抓取。每次抓取会聚合最近 5 分钟的 `spend` 记录，并先刷新待写入的 TS 缓冲。

固定 labels：

```text
provider
model
stream
status
```

输出指标包括：

```text
lmgateway_spend_requests_last_5m
lmgateway_spend_prompt_tokens_last_5m
lmgateway_spend_completion_tokens_last_5m
lmgateway_spend_reasoning_tokens_last_5m
lmgateway_spend_cached_tokens_last_5m
lmgateway_spend_total_tokens_last_5m
lmgateway_spend_cost_usd_last_5m
lmgateway_spend_latency_ms_sum_last_5m
lmgateway_spend_latency_ms_avg_last_5m
lmgateway_spend_ttft_ms_sum_last_5m
lmgateway_spend_ttft_ms_avg_last_5m
lmgateway_spend_generation_ms_sum_last_5m
lmgateway_spend_generation_ms_avg_last_5m
lmgateway_spend_generation_tokens_per_second_last_5m
```

指标使用 gauge，因为窗口会随时间滑动。响应使用 Prometheus text exposition format `0.0.4`。

兼容性：已有的 `lmgateway_spend_*_last_5m` token、cost 和 `latency_ms` 指标继续保留；`latency_ms` 仍表示完整请求耗时。TTFT、generation time 和 token/s 使用新增指标，不替换旧指标。

## 健康检查

### `GET /healthz`

无需鉴权，正常返回 `200 OK`。响应体为空。

## OpenAI-compatible API

### `GET /v1/models`

返回显式配置模型和 provider 实时发现模型。

查询参数：

- `provider`：可选，只查询指定 provider；例如 `?provider=deepseek`。

模型列表规则：

- 显式 `models[]` 配置以裸 ID 返回，例如 `gpt-5.6-luna`。
- provider `/models` 发现结果使用 `[provider]/[upstream-model]` 前缀，例如 `openrouter/openai/gpt-5.6-luna`。
- 每次请求实时查询 OpenAI-compatible provider，不使用缓存。
- 显式配置的同名模型优先，避免重复返回。
- Codex OAuth provider 不通过普通 provider `/models` 自动发现；Codex 模型由显式配置加入。

响应形状：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5.6-luna",
      "object": "model",
      "created": 0,
      "owned_by": "chatgpt_codex"
    }
  ]
}
```

### `POST /v1/chat/completions`

保留的 Chat Completions 兼容接口。普通 OpenAI-compatible provider 和 Codex provider 都可以通过该接口使用。

最小请求：

```json
{
  "model": "agent-max",
  "messages": [
    {
      "role": "user",
      "content": "只回答 OK"
    }
  ],
  "stream": false
}
```

常用请求字段：

- `model`
- `messages`
- `stream`
- `tools`
- `tool_choice`
- `temperature`
- `max_tokens`
- `reasoning_effort`
- `reasoning_summary`
- `metadata`
- `previous_response_id`

Codex Chat 适配行为：

- 外部接收 Chat Completions。
- 内部转换到 Codex Responses。
- `reasoning_effort` 转为 Responses 的 `reasoning.effort`。
- `reasoning_summary` 转为 Responses 的 `reasoning.summary`。
- Codex 不接受 `max_output_tokens`，Chat 路径不会把 `max_tokens` 转发为该字段。
- Responses reasoning summary 会投影为 Chat 的 `reasoning_content`。
- 工具调用参数增量按 `item_id -> call_id` 关联，并合并为标准 Chat `tool_calls`。

流式响应：

```text
Content-Type: text/event-stream
```

返回标准 Chat completion chunks，结束事件为：

```text
data: [DONE]
```

reasoning chunk 示例：

```json
{
  "choices": [
    {
      "index": 0,
      "delta": {
        "reasoning_content": "简短摘要"
      },
      "finish_reason": null
    }
  ]
}
```

### `POST /v1/responses`

原生 OpenAI Responses 兼容接口。该接口用于保留 OpenCode 原生 Responses 的 reasoning block、summary part、`item_id`、`summary_index`、工具事件和换行语义。

OpenCode 的 native provider 使用：

```text
POST /v1/responses
```

最小请求：

```json
{
  "model": "agent-max",
  "input": [
    {
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "只回答 OK"
        }
      ]
    }
  ],
  "stream": true,
  "store": false,
  "reasoning": {
    "effort": "max",
    "summary": "concise"
  },
  "include": [
    "reasoning.encrypted_content"
  ]
}
```

支持透传的 Responses 字段包括：

- `model`
- `input`
- `instructions`
- `tools`
- `tool_choice`
- `stream`
- `store`
- `reasoning`
- `include`
- `previous_response_id`
- `text`
- `max_output_tokens`
- `temperature`
- `top_p`
- 其他上游支持的字段

原生流式事件会保留 `event:` 名称和 JSON data，不追加 Chat 的 `[DONE]`。常见事件包括：

```text
response.created
response.in_progress
response.output_item.added
response.reasoning_summary_part.added
response.reasoning_summary_text.delta
response.reasoning_summary_text.done
response.reasoning_summary_part.done
response.output_item.done
response.content_part.added
response.output_text.delta
response.output_text.done
response.content_part.done
response.completed
response.failed
```

OpenCode 使用这些事件恢复独立 reasoning part，因此比 Chat 兼容路径更适合原生 COT 渲染和多轮 reasoning replay。

### 当前模型 alias

当前 live store 中的主要 alias：

```text
agent      -> gpt-5.6-luna
agent-max  -> gpt-5.6-luna + reasoning.effort=max + reasoning.summary=concise
```

Codex 上游 provider：

```text
chatgpt_codex
```

实际 Codex 模型 ID：

```text
gpt-5.6-luna
```

## 管理 API

所有 `/api/*` 接口都需要鉴权，除非 `LMGATEWAY_API_KEY` 为空。

成功响应通常为 JSON。配置写入会先进行 JSON Schema 和语义校验，成功入库后通过 store watch 触发热重载；校验失败不会写入。

### Provider 实体

Provider 字段：

```json
{
  "type": "openai",
  "base_url": "https://provider.example/v1",
  "discover": false,
  "api_key": "<upstream-api-key>",
  "api_key_env": "UPSTREAM_API_KEY",
  "timeout_sec": 120,
  "options": {},
  "originator": "lmgateway",
  "forward_client_metadata": false
}
```

支持的 provider type：

- `openai`
- `chatgpt_codex`
- `commandcode`

`[provider]/model` 前缀路由对所有 provider 生效（provider handler 会剥离前缀），因此 alias/后端 id 可直接用 `provider/model` 寻址，不依赖 discovery。`discover` 默认 `false`，仅控制是否实时请求 `/models` 并出现在发现模型列表；显式配置的 model 优先级更高。`chatgpt_codex` 不支持普通 `/models` 自动发现。

`commandcode` 对接 commandcode.ai 的 `/alpha/generate` 私有协议：handler 把任意客户端请求归一化为 Chat、组装 CC 信封（含设备指纹、lifecycle 预请求、`threadId` 会话），再把 CC NDJSON 事件流投影回客户端协议。`options` 支持：

```json
{
  "device_project_dir": "C:\\Users\\dev\\projects\\app",
  "fingerprint_salt": "",
  "cli_mode": "agent",
  "cli_session_mode": "interactive",
  "zdr": false,
  "empty_system_placeholder": true,
  "stream_idle_ms": 30000,
  "nonstream_idle_ms": 90000,
  "upstream_retry_max": 2,
  "upstream_retry_base_ms": 400
}
```

`stream_idle_ms` / `nonstream_idle_ms` 是上游读空闲看门狗，只计两次读取之间没有字节的时间，超时不重试并返回超时错误。`upstream_retry_max` 为在**尚未向下游写出任何字节**前允许的上游闪断/空流重试次数（`0` 关闭）；一旦已提交首个事件就不再重试。

接口：

```text
GET    /api/provider/list
GET    /api/provider/get/{name}
PUT    /api/provider/set/{name}
PATCH  /api/provider/patch/{name}
DELETE /api/provider/delete/{name}
POST   /api/provider/reset/{name}
```

`GET` 和 `list` 会隐藏 `api_key`，返回：

```json
{
  "api_key": "****",
  "has_api_key": true
}
```

写入成功示例：

```json
{
  "ok": true,
  "name": "openrouter",
  "version": 2
}
```

可使用 `If-Match: <version>` 做乐观锁更新。版本冲突返回 `409 Conflict`。

### Model 实体

Model 字段：

```json
{
  "provider": "chatgpt_codex",
  "upstream_model": "gpt-5.6-luna",
  "extra_body": {},
  "default": false,
  "input_per_mtok": 0,
  "output_per_mtok": 0
}
```

接口：

```text
GET    /api/model/list
GET    /api/model/get/{name}
PUT    /api/model/set/{name}
PATCH  /api/model/patch/{name}
DELETE /api/model/delete/{name}
POST   /api/model/reset/{name}
```

写入成功后会保存为完整配置文档，并标记来源：YAML 中已有同名对象时为 `db_override`，否则为 `db`。读取接口返回当前 effective 配置，并附带 `source` 和持久化 `version`。

### Pool 实体

pool 把一个逻辑模型 alias 映射到有序的后端模型 id 列表，仅当 active 后端返回限流时轮换；
选择是 sticky 的（不做 round-robin），保留上游 prefix/KV cache。

```json
{
  "model": "deepseek-v4.1",
  "backend": ["aaa/deepseek-v4.1", "bbb/deepseek-v4.1"],
  "cooldown_sec": 60,
  "on_all_limited": "fail",
  "policy": "speed_first",
  "reselect_ttft_ms": 5000,
  "probe_interval_min": 30
}
```

`backend` 的成员由现有路由解析（显式 model 边或 `provider/model` 前缀路由）。`on_all_limited`
为 `fail`（默认）或 `force-least-recent`；`cooldown_sec` 是上游未给 `retry-after` 时的冷却。

`policy` 选择排序键：`speed_first`（默认，探针 TTFT 优先，价格次之）或 `price_first`（价格优先；
订阅 plan 无 `*_per_mtok` 视为免费）。网关每 `probe_interval_min`（默认 30）分钟向后端派发一次
合成流式探针，measurement 由 spend tracker 记录（TTFT）。仅当 active 冷却或其探针 TTFT 超过
`reselect_ttft_ms`（默认 5000）时才切换；无 round-robin。`probe_max_tokens` 可在 model 元数据上
覆盖探针输出长度（默认 1024）。

```text
GET    /api/pool/list
GET    /api/pool/get/{model}
PUT    /api/pool/set/{model}
PATCH  /api/pool/patch/{model}
DELETE /api/pool/delete/{model}
POST   /api/pool/reset/{model}
GET    /api/pool/status/{model}    # 运行时：active 后端 + 冷却
POST   /api/pool/rotate/{model}    # 强制切到下一个健康后端（内存重编译）
POST   /api/pool/clear/{model}     # 清除冷却（不改变 active）
```

`status`/`rotate`/`clear` 需要网关启用了 pool controller（provider 存在时 `d.Pools != nil`）。
`rotate` 在所有后端冷却时保持 active 不变；若 `on_all_limited: force-least-recent` 则切到冷却最早
到期的后端。

pool 会编译成一条受控的 alias 规则（数据面没有专门的 pool handler）。一个被动的上报器观察到
限流失败后交给控制面控制器，控制器轮换 active 后端并在内存中重编译路由（不写配置库）。
命中的请求返回 `429` 并带 `Retry-After`；下一个请求（或客户端重试）使用被提升的后端。

### Rules

接口：

```text
GET    /api/config/rules/meta
GET    /api/config/rules/list
GET    /api/config/rules/get/{id}
PUT    /api/config/rules/set/{id}
PATCH  /api/config/rules/patch/{id}
DELETE /api/config/rules/delete/{id}
POST   /api/config/rules/reset/{id}
```

规则字段：

```json
{
  "id": "alias-agent-max",
  "from": "http",
  "match": [
    {
      "field": "model",
      "op": "eq",
      "value": "agent-max"
    }
  ],
  "set": {
    "model": "gpt-5.6-luna"
  },
  "default": {
    "reasoning.effort": "max",
    "reasoning.summary": "concise"
  },
  "action": "",
  "to": "http"
}
```

`set` 与 `default` 语义：

- `set`：硬覆盖。无条件写入，客户端显式值会被替换；用于 model 别名、条件降级等路由/强制场景。
- `default`：软默认。仅当字段缺失、`null` 或空字符串时写入，客户端显式值优先；用于 reasoning 等参数默认。

应用顺序（低到高）：

```text
客户端请求
  → rule.default（仅缺失时写入）
  → rule.set（硬覆盖）
  → 协议转换
  → model.default_extra_body（软）
  → model.extra_body（硬）
  → handler 协议强制字段（stream/store/include 等）
```

多条规则命中时，`default` 按规则声明顺序先到先得。

Model 实体也支持软默认：

```json
{
  "name": "agent-max",
  "provider": "chatgpt_codex",
  "upstream_model": "gpt-5.6-luna",
  "default_extra_body": {
    "reasoning": { "effort": "max", "summary": "concise" }
  },
  "extra_body": {}
}
```

可匹配字段：

```text
type
model
stream
metadata.*
raw.*
provider
error
error_kind
```

可用操作：

```text
eq
neq
in
prefix
regex
exists
empty
```

`set + to` 可构造纯变换规则：修改请求后回到指定位置继续路由。

### Settings

接口：

```text
GET   /api/config/settings/list
GET   /api/config/settings/get/{name}
PUT   /api/config/settings/set/{name}
PATCH /api/config/settings/patch/{name}
POST  /api/config/settings/reset/{name}
```

允许的 setting：

- `server`
- `spend`
- `logprobs`

`logprobs` 是被动 webhook 导出设置。网关不会添加或修改客户端的 `logprobs`、`top_logprobs` 等请求字段；仅当上游实际返回 logprobs 时，才将完整数据、响应 content、reasoning、tool calls 和 usage 异步 POST 到 `webhook_url`。投递超时、失败、非 2xx 或并发队列满都会直接丢弃，不影响客户端响应。

```json
{
  "webhook_url": "http://collector.example:8080/logprobs",
  "timeout_sec": 10
}
```

```json
{
  "raw_retention_days": 7,
  "rollup_dimensions": ["provider", "model", "stream"],
  "timezone": "Asia/Shanghai"
}
```

### Actions

通用接口：

```text
POST /api/action/{name}
```

当前 action：

```text
POST /api/action/rollup/run
POST /api/action/spend/flush
POST /api/action/config/reload
POST /api/action/config/seed
```

常见成功响应：

```json
{
  "ok": true
}
```

配置对象的来源字段有三种：`yaml` 表示通过数据库 marker 懒加载 YAML baseline，`db_override` 表示数据库中覆盖 YAML 同名对象的完整配置，`db` 表示数据库独有对象。`reset` 会把有 YAML baseline 的对象恢复为 `{"source":"yaml"}`；没有 YAML baseline 的数据库对象应使用 `delete`。

## Token 与 Spend API

### `GET /api/spend/token`

查询参数：

- `time=today`，默认值
- `time=yesterday`
- `time=7d`
- `time=30d`
- `time=YYYY-MM-DD`

响应：

```json
{
  "time": "today",
  "timezone": "Asia/Shanghai",
  "from": "2026-08-26T00:00:00Z",
  "to": "2026-08-26T01:00:00Z",
  "source": "raw",
  "requests": 1,
  "tokens": {
    "prompt_tokens": 100,
    "completion_tokens": 25,
    "reasoning_tokens": 10,
    "cached_tokens": 80,
    "total_tokens": 125
  },
  "cost": 0,
  "latency_ms": {
    "sum": 1000,
    "avg": 1000
  },
  "timing_ms": {
    "ttft_ms": {
      "sum": 250,
      "avg": 250
    },
    "generation_ms": {
      "sum": 1750,
      "avg": 1750
    },
    "generation_tokens_per_second": 14.2857142857
  }
}
```

`source`：

- `raw`
- `daily`
- `mixed`
- `empty`

统计兼容 Chat 与 Responses usage 字段：

```text
Chat:       prompt_tokens / completion_tokens
Responses:  input_tokens / output_tokens
```

reasoning 和 cache 字段也会从两种 usage 结构中提取。

### `GET /api/ts/{stream}/query`

查询原始时间序列记录。

常用 stream：

```text
spend
spend_daily
```

查询参数：

- `from=<RFC3339>`
- `to=<RFC3339>`
- `limit=<number>`
- `provider=<provider>`
- `model=<model>`
- `status=<status>`
- `stream=<true|false>`

响应：

```json
{
  "records": [
    {
      "Ts": "2026-08-26T01:00:00Z",
      "Stream": "spend",
      "Tags": {
        "provider": "chatgpt_codex",
        "model": "gpt-5.6-luna",
        "stream": "true",
        "status": "ok"
      },
      "Fields": {
        "prompt_tokens": 100,
        "completion_tokens": 25,
        "reasoning_tokens": 10,
        "cached_tokens": 80,
        "total_tokens": 125,
        "latency_ms": 1000,
        "ttft_ms": 250,
        "ttft_samples": 1,
        "generation_ms": 750,
        "generation_samples": 1,
        "generation_tokens": 25
      }
    }
  ]
}
```

### `GET /api/ts/{stream}/agg`

聚合时间序列数据。

查询参数：

- `from=<RFC3339>`
- `to=<RFC3339>`
- `group=model` 或逗号分隔的 tag 名
- `bucket=1h` 等 Go duration
- `model=<model>`：按 model 过滤

响应：

```json
{
  "rows": [
    {
      "Tags": {
        "model": "gpt-5.6-luna"
      },
      "Bucket": "0001-01-01T00:00:00Z",
      "Sums": {
        "prompt_tokens": 100,
        "completion_tokens": 25,
        "reasoning_tokens": 10,
        "cached_tokens": 80,
        "total_tokens": 125,
        "latency_ms": 1000,
        "cost": 0
      },
      "Count": 1
    }
  ]
}
```

## Codex OAuth API

Codex token 存储在 gateway store 中，status 接口不会返回 access/refresh token。

### `POST /api/action/codex/login/device/start`

启动 Device Code 登录。

成功响应包含：

```json
{
  "request_id": "<request-id>",
  "verification_url": "https://auth.openai.com/codex/device",
  "user_code": "<user-code>",
  "expires_at": "2026-08-26T00:00:00Z"
}
```

### `POST /api/action/codex/login/device/poll`

请求：

```json
{
  "request_id": "<request-id>"
}
```

登录等待中：

```json
{
  "status": "pending",
  "request_id": "<request-id>",
  "verification_url": "https://auth.openai.com/codex/device",
  "user_code": "<user-code>",
  "expires_at": "2026-08-26T00:00:00Z"
}
```

登录成功：

```json
{
  "status": "authenticated",
  "request_id": "<request-id>",
  "account_id": "<account-id>"
}
```

### `GET /api/action/codex/status`

响应：

```json
{
  "authenticated": true,
  "expires_at": "2026-09-04T16:24:31Z",
  "account_id": "<account-id>",
  "residency": "<region>"
}
```

### `POST /api/action/codex/logout`

删除当前 Codex token。

响应：

```json
{
  "ok": true
}
```

## Codex UI

```text
GET /ui/codex/
```

提供 Device Code 登录页面。该页面用于调用上面的 Codex device start/poll/status/logout API。

## 错误格式

管理 API 错误统一为：

```json
{
  "error": "error message"
}
```

OpenAI 兼容 API 错误通常为：

```json
{
  "error": {
    "message": "error message",
    "type": "gateway_error"
  }
}
```

常见状态码：

- `400`：请求 JSON、参数或 If-Match 格式错误
- `401`：鉴权失败
- `404`：路由、资源或模型不存在
- `409`：If-Match 版本冲突
- `422`：Schema 或配置语义校验失败
- `502`：上游 provider/Codex 调用失败

## 当前实现要点

- `/v1/chat/completions`：兼容旧 OpenAI Chat 客户端，Codex 请求内部转 Responses。
- `/v1/responses`：保留 OpenCode 原生 Responses 事件，用于正确渲染 reasoning/COT 和工具调用。
- `/v1/models`：显式模型与 provider 实时发现模型合并，发现模型加 provider 前缀。
- 配置写入后自动热加载，旧配置在新配置校验失败时继续运行。
- LiteLLM 已缩容到 0；`llm.svc.calvinlin.cn` 当前由 lmgateway 提供。

## lmgcli

`lmgcli` 是管理 API 的命令行薄封装，不直接读写数据库。默认服务地址为 `http://127.0.0.1:8080`，也可通过 `LMGATEWAY_SERVER` 或 `--server` 指定。

```text
lmgcli health
lmgcli provider list
lmgcli model get gpt-5.6-luna
lmgcli rule get alias-agent
lmgcli rule patch alias-agent --file patch.yaml
lmgcli setting get spend
lmgcli config reload
lmgcli action spend/flush
```

### alias

alias 是遵循 `alias-<name>` 约定的 rule（`from: http`、`match model==<name>`、`set.model=<target>`、可选 `default`），`lmgcli alias` 用位置参数语法直接编辑它。注意全局 flag 解析在第一个位置参数处停止，因此 `--if-match`/`--rule-id` 可放在任意位置。

```text
lmgcli alias list
lmgcli alias get agent-max
lmgcli alias set agent model gpt-5.6-luna-fast
lmgcli alias set agent-max model gpt-5.6-luna-fast effort max summary concise
lmgcli alias set agent-max effort low          # 只改软默认，需 alias 已存在
lmgcli alias unset agent-max summary
lmgcli alias reset agent-max                    # 丢弃 DB override，回到 YAML baseline
lmgcli alias delete agent-max
```

键词汇：`model` 写入硬 `set.model`（路由目标）；`effort`、`summary`、`temperature`、`top_p`、`max_tokens` 是短名，映射到软 `default`（分别为 `reasoning.effort`、`reasoning.summary`、`temperature`、`top_p`、`max_tokens`）；含点的键按请求路径原样写入 `default`。未知的裸键会报错，避免拼错静默不生效。`set` 会与现有 rule 合并，改一个默认不会丢掉其他字段。

### 补全

```text
lmgcli completion bash
lmgcli completion zsh
```

补全脚本调用隐藏命令 `lmgcli __complete <cword> <words...>`，动态提示命令、flag、运行时名称（provider/model/setting/rule/alias）与 alias 键；`model` 值会合并 `/api/model/list` 与 `/v1/models`（含发现的 `[provider]/model`）。API 查询失败时静默返回空，不影响 shell。

```bash
source <(lmgcli completion bash)
# zsh
lmgcli completion zsh > "${fpath[1]}/_lmgcli"
```

`lmgcli` 支持持久客户端配置文件 `$XDG_CONFIG_HOME/lmgcli.toml`，未设置 `XDG_CONFIG_HOME` 时默认为 `~/.config/lmgcli.toml`。可使用 `lmgcli config init` 创建模板。

```toml
server = "https://llm.example.com"
api_key_env = "LMGATEWAY_API_KEY"
auth_header = "Authorization"
output = "json"
timeout = "30s"
```

配置优先级为：命令行参数 > 环境变量 > TOML 文件 > 默认值。API key 只通过 `api_key_env` 或 `api_key_file` 引用，不写入 TOML。
