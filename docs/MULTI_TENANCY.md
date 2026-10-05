# 多租户（Multi-Tenancy）

> 目标读者：二次开发者 + 前端 SPA 开发者
> 实现位置：`internal/tenancy`（领域/鉴权/配额）、`internal/tenancyapi`（管理 API）、
> `internal/handler/authorize.go`（数据面授权 handler）
> 状态：已实现并通过 44 项端到端验证（`devharness/smoke-tenancy.sh`）

---

## 1. 概念模型

```
Tenant（租户，最外层隔离单元）
  └── Project（项目，API key 挂载点）
        └── API Key（数据面凭据，可限模型/限速/限预算）

User（管理面账号，三种角色）
  ├── admin        全局管理员：可管理所有租户
  ├── tenant_admin 租户管理员：只管理本租户下的 project / user / key
  └── member       租户成员：只读本租户
```

`tenant` 与 `project` 的 id 是 **slug**（`^[a-z0-9][a-z0-9-]{1,62}$`）。
刻意禁用下划线：底层 `JSONStore.List` 在 sqlite/pg 上用 `LIKE prefix||'%'` 且不转义 `%`/`_`。

---

## 2. 存储布局

复用既有 `store.JSONStore`（SQLite 开发 / Postgres 生产），键前缀：

| 键 | 内容 |
| --- | --- |
| `tenant/<id>` | Tenant |
| `project/<id>` | Project |
| `user/<id>` | User（内含 `password_hash`） |
| `apikey/<id>` | APIKey（内含 `key_hash`，**绝不存明文**） |
| `session/<sha256(token)>` | Session |

**为什么不走 config 实体框架**：`config.Config` 是「网关配置」，参与路由编译、有 YAML 基线、
有唯一的 `ReconcileBaseline`；而租户/密钥是**运行时数据**，数量大、无基线、与路由无关。
放进去会污染配置语义。因此它们只在 `JSONStore` 里，由 `tenancy.Repository` 独占管理。

**一致性**：`Repository` 在内存维护索引（数据面鉴权是每请求热路径，不能每次扫库）。
写入直接落 store，然后刷新内存；`JSONStore.Watch` 负责多进程收敛，另有 30s 周期兜底刷新。

---

## 3. 凭据与授权矩阵

### 3.1 数据面 / 管理面

| 凭据 | 形式 | 管理面 `/api/tenancy/*` | 数据面 `/v1/*` | 备注 |
| --- | --- | --- | --- | --- |
| master key | `server.master_key` 或 `LMGATEWAY_API_KEY` | ✅ 全局 | ✅ | 运维身份，不受租户配额约束 |
| 会话 token | `sess_…`（登录得到） | ✅ 按角色 | ❌ 403 | 默认 12h 过期 |
| API key | `sk-lm-…` | ❌ 403 | ✅ 绑定 project | 可限模型/RPM/TPM/预算 |

### 3.2 网关配置 API（`/api/*`，provider / model / rule / pool / setting）

网关配置是**所有租户共享**的，不属于任何单个租户，因此单独一道门：

| 凭据 | `/api/*`（配置） | 说明 |
| --- | --- | --- |
| master key | ✅ 读写 | 运维 |
| **全局管理员**会话（role=admin） | ✅ 读写 | 控制台里给"管理员"用 |
| 租户管理员会话（role=tenant_admin） | ❌ 403 | 即使会话合法也不行 |
| 成员会话（role=member） | ❌ 403 | |
| 数据面 API key | ❌ 403 | 数据面凭据不能碰控制面 |
| 无凭据 | ❌ 401 | |

实现是 `tenancy.Authenticator.GuardConfig`（挂在 `main.go` 的 `/api/` 上）。
**这就是"角色判断下沉到接口层"**：前端的导航隐藏与路由守卫只是体验，这里才是边界。

> 边界说明：未配置 master key **且** 库里没有任何用户时是"开放模式"（保留本地开发体验）；
> 一旦有了第一个用户，配置 API 就必须凭据才能访问。

### 3.3 租户角色可读的最小模型清单

租户管理员创建 API key 时要勾选"模型权限"，但它读不到配置 API。
因此单独提供一个**只暴露名字**的端点：

```
GET /api/tenancy/models  →  {"items":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},…]}
```

只返回 `id`：provider 端点、上游模型名、定价都属于网关配置。
模型名本身不是秘密——持数据面密钥的客户端本来就能从 `/v1/models` 看到。

### 3.4 密钥学

- 密码用 **PBKDF2-HMAC-SHA256（210k 迭代，随机盐）**——`crypto/pbkdf2` 是 Go 1.24+ 标准库，**没有引入任何新依赖**；
  API key / session token 是 256bit 随机串，用单次 SHA-256 摘要存储（不存在字典攻击面）。
- 常量时间比较；登录失败不区分「用户不存在」与「密码错误」（防账号枚举）。
- 健康检查 `/healthz` 永远公开；`/metrics`、`/ui/codex/` 依旧不鉴权（与上游一致）。

---

## 4. 数据面链路

```
HTTP 请求
  → tenancyAuth.Guard(ScopeData)        解析凭据 → Identity 注入 request context
  → httpapi 建 packet，写入 pkt["identity"]
  → Dispatcher.Serve(pkt, "@ingress")
      @ingress --observe--> authorize --> [ratelimit] --> http --> <provider>
                              ↑ 新增的一环
```

**`authorize` 的位置是刻意的**：它挂在 `http` 之前，也就是**在模型别名改写之前**。
别名规则（`from: http` 的纯变换边）尚未运行，所以租户看到的模型名就是客户端原始请求的模型名——
否则租户只要写一条 `alias-x → gpt-4o` 就能绕过自己 key 的白名单。

`authorize` 做三件事并按顺序短路：

1. **模型白名单**：deny 优先；allow 为空 = 不限；支持 `gpt-*` 通配前缀。不通过 → **403**（`error_kind=policy`）。
2. **配额预扣**：RPM / TPM / 日/月成本，不通过 → **429 + Retry-After**。
3. 放行后 `serve(pkt)`，**结束后按上游真实 usage 结算**（释放预扣、累加实际成本与 token）。

失败即**终态返回**，不调用 Continue → 不会打到上游、不产生费用。

---

## 5. 配额语义

| 维度 | 窗口 | 判定时机 | 说明 |
| --- | --- | --- | --- |
| RPM | 固定 1 分钟（按墙钟对齐） | 请求进入 | 到点自动重置 |
| TPM | 同上 | 进入时粗判 + 结束按实际累加 | 用「已用 + 本次输出上界」保守判定 |
| 日成本 | UTC 自然日 | 进入时**预扣**，结束**结算** | 预扣 = 成本上界 |
| 月成本 | UTC 自然月 | 同上 | |

**成本上界怎么估**：`输入 = 请求体序列化字节数 / 4`，`输出 = max_tokens`（请求未写则用 4096）。
再乘模型定价（`models[].input_per_mtok` / `output_per_mtok`）。

**为什么用预扣**：并发的多个请求如果只看「已结算成本」，会同时通过判定并一起超支。
预扣保证 `已用 + 在途 + 本次上界 ≤ 限额`，**不会超支**；代价是偏保守（可能提前拒绝）。
预扣与结算严格对称：key / project / tenant 三层是三个**独立预算**，每层都记全额。

**三层限额都生效**：限额可以分别配在 API key、project、tenant 上
（`rpm_limit` / `tpm_limit` / `daily_cost_limit` / `monthly_cost_limit`，0 = 不限）。
三层是**互相独立**的预算：任一层超限即拒绝，每层都按本次请求的全额计费；
同一个租户下的多把 key 共享同一个租户预算。拒绝消息会指名是哪一层
（如 `daily cost budget exceeded at tenant acme`），并作为 `scope` 标签进入告警。

**重启不能绕过预算**：计数器是进程内的，但启动时会调用 `tenancy.Calibrate` 从
`spend` + `spend_daily`（rollup 日汇总）合并后的数据，按 `tenant`/`project`/`key` 标签
重建当日与当月成本，之后每 5 分钟再校准一次（有单测覆盖）。

为什么必须合并：超过 `raw_retention_days` 的原始流水会被 rollup 裁剪，只看 `spend`
会让"月中重启"把当月已花成本算漏——等于凭空恢复预算。分界点取 rollup 的 watermark
（`setting/rollup_watermark`），两侧不重叠、不重复。

### 已知限制

1. **多实例部署时计数据不共享**：RPM/TPM/成本计数器在进程内，N 个实例各自计数，
   实际放行量约为 `限额 × N`。要严格全局限制需要引入共享计数器（Redis 或 SQL 原子累加）。
   这是当前最需要知道的限制——**若你要水平扩展，这里必须改**。
2. **固定窗口**不是滑动窗口：窗口边界处允许 2 倍瞬时突发。换成滑动窗口是局部改动。
3. RPM/TPM 的窗口计数**不接受配置变更的历史隔离**：给一把已用过的 key 新设 `rpm_limit`，
   当前分钟窗口内已发生的请求仍计入（这符合「限额作用于窗口」的直觉，但和"改完立刻从零开始"不同）。
4. **限额的粒度是"每层各自固定窗口"**：三层各自按 1 分钟 / 自然日 / 自然月独立判定，
   不做跨层的滑动窗口或配额借用（例如租户没用完的额度不会下发给项目）。

---

## 6. 管理 API 契约（供 SPA）

挂载点 `/api/tenancy/`，全部返回 JSON。列表统一
`{"items":[...],"total":N,"limit":L,"offset":O}`——**不传 `limit` 时返回全量**
（`limit=0`），旧调用行为不变；显式传 `limit`（上限 1000）才截断，`total` 始终是全量计数。
错误统一 `{"error":{"message":"..."}}`。认证用 `Authorization: Bearer <token>` 或 `X-API-Key`。

### 6.1 认证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/tenancy/bootstrap` | `{email,password,name?}` → 创建首个管理员并直接返回会话。**仅当库里无任何用户时可用**，否则 409 |
| POST | `/api/tenancy/login` | `{email,password}` → `{token,expires_at,user}`；失败 401 |
| POST | `/api/tenancy/logout` | 撤销当前会话 |
| GET | `/api/tenancy/me` | 返回 `{identity:{kind,user_id,email,role,tenant_id}}` |
| GET | `/api/tenancy/overview` | `{counts:{tenants,projects,users,keys,sessions}}`（按身份自动收窄） |

### 6.2 资源 CRUD

`{resource}` ∈ `tenants` / `projects` / `users` / `keys`，五个 verb 同构：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/tenancy/{resource}` | 列表；支持 `?tenant=&project=` 过滤（越权查询 403）与 `?limit=&offset=` 分页 |
| POST/PATCH | `/api/tenancy/tenants`、`/api/tenancy/projects` | 可带 `rpm_limit`/`tpm_limit`/`daily_cost_limit`/`monthly_cost_limit`（0 = 不限，负数 422） |
| POST | `/api/tenancy/{resource}` | 新建，成功 201 |
| GET | `/api/tenancy/{resource}/{id}` | 详情 |
| PATCH | `/api/tenancy/{resource}/{id}` | 局部更新（只提交要改的字段） |
| DELETE | `/api/tenancy/{resource}/{id}` | 删除；**有下级资源时 409**（防孤儿数据） |

字段速查：

```jsonc
// POST /tenants
{"id":"acme","name":"Acme Inc","status":"active","note":"..."}

// POST /projects
{"id":"web","tenant_id":"acme","name":"Web App","status":"active"}

// POST /users   password 只在创建/改密时出现
{"email":"a@acme.io","password":"...","role":"tenant_admin|member|admin","tenant_id":"acme","name":"..."}

// POST /keys    → 201 {"key":{...},"secret":"sk-lm-..."}  secret 只返回这一次
{"tenant_id":"acme","project_id":"web","name":"prod",
 "allowed_models":["gpt-4o","gpt-*"],"denied_models":["gpt-4o-legacy"],
 "rpm_limit":600,"tpm_limit":200000,
 "daily_cost_limit":5.0,"monthly_cost_limit":100.0,
 "expires_at":"2026-12-31T00:00:00Z"}
```

### 6.3 特有能力

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/tenancy/keys/{id}/rotate` | 轮换密钥 → `{key,secret}`；旧密钥立即失效，限额/白名单保留 |
| GET | `/api/tenancy/keys/{id}/quota` | `{key_id,limits:{...},usage:{...,scopes:[{scope,rpm_used,tpm_used,daily_cost,monthly_cost,pending_cost}]},scope_limits:[{scope,label,rpm_limit,...}]}` |
| POST | `/api/tenancy/users/{id}/password` | `{password}`；本人或管理员可改；**改后撤销该用户全部会话** |
| POST | `/api/tenancy/users/{id}/sessions/revoke` | 踢下线 |

### 6.4 用量

`GET /api/tenancy/usage?group_by=tenant|project|key|model|provider&from=&to=&tenant=&project=&key=`

- 默认 `group_by=tenant`，时间默认最近 7 天，上限 90 天；
- 返回 `{group_by,from,to,source,items:[...]}`，`items` 每行为
  `{group,requests,prompt_tokens,completion_tokens,total_tokens,cached_tokens,cost}`，按成本降序
  （同额按 `group` 名排序，保证结果稳定）；
- 非管理员身份自动只统计自己租户的数据。

**数据来源是 raw + rollup 的合并**（`internal/spendagg`）：

- 超过 `raw_retention_days` 的原始 `spend` 流水会被 rollup 裁剪成 `spend_daily` 日汇总，
  只看 raw 会让 7 天以前的历史直接消失。合并以 rollup 的 **watermark** 为分界点：
  `[.., watermark)` 走 daily、`[watermark, ..]` 走 raw，两侧不重叠，既不重复也不漏算；
- `source` ∈ `raw` / `daily` / `mixed` / `empty`，按**实际命中的记录**判定；
- 用到历史汇总时会额外返回 `covered_from`（daily 覆盖到哪一刻）与
  `daily_dimensions`（日汇总实际带有的维度）；
- **维度保证**：`tenant`/`project`/`key`/`provider`/`model`/`stream` 是**强制汇总维度**
  （见 `config.MandatoryRollupDimensions`），不受 `rollup_dimensions` 配置影响——
  否则按租户/项目/密钥查历史、以及重启后的配额校准都会失去依据。
  `rollup_dimensions` 里配的额外维度（如 `meta_team`）会叠加，不是替换；
- **降级是显式而不是静默的**：若历史汇总行缺少当前 `group_by` 维度（例如旧的 rollup 数据
  或该维度未启用），响应会带 `truncated:true` + `degraded_reason`，控制台用量页会显示告警条，
  而不是把缺失的部分当成"真实用量"。

### 6.5 展示设置（汇率）

`GET /api/tenancy/display` → `{"currency":"USD","usd_to_cny":7.2}`

- **所有已登录角色**都能读（控制台要用它换算金额），但它不含任何租户数据或网关配置；
- 写入走配置 API（仅全局管理员）：`PUT /api/config/settings/set/display`，非法币种/汇率返回 422；
- 改完**立即生效，无需重启**（配置管理器热重载 → 本端点直接读运行中的配置）。

### 6.6 审计日志

`GET /api/tenancy/audit?from=&to=&actor=&action=&resource=&tenant=&limit=&offset=`

- 回答"谁在什么时候改了什么"：模型定价、Provider、池、规则、设置、密钥、用户、租户/项目、
  改密、会话撤销、登录（含失败）、bootstrap/logout、运维 action 等写操作都有记录；
- 返回 `{items:[...],total,limit,offset}`，最新在前。每条含
  `{id,at,actor:{kind,user_id,email,role,label},action,resource,tenant_id,method,path,status,outcome,detail}`；
- `detail.changes` 是**字段级 diff**（`{字段:{from,to}}`），所以能直接看出
  `model.patch model/gpt-x → input_per_mtok 3 → 5`；
- **永不落密钥材料**：`api_key`/`password`/`token`/`key_hash` 等字段在写入前统一脱敏成 `***`，
  密钥只留 id 与前缀；
- 权限：全局管理员看全部；其他角色只能看 `tenant_id` 等于自己租户的记录（`?tenant=` 越权 403）；
- 存储：JSONStore 键前缀 `audit/<日期>/<时间戳-随机>`，按天分片，默认保留 90 天
  （写入时顺带清理过期分片）。

实现分两层：HTTP 中间件对**所有变更方法兜底记录**（新增写接口不会漏审计），
处理器再补语义细节（action/resource/detail）；同一次请求只落一条。

### 6.7 告警

`GET /api/tenancy/alerts?state=firing|resolved&rule=&tenant=&limit=&offset=`

- 返回 `{items,total,limit,offset}`，`firing` 在前、其次按最近时间倒序；
- 规则：
  - `quota_exhausted`——窗口内某 key/project/tenant 被配额拒绝（RPM/TPM/日/月预算）
    的次数 ≥ `quota_denial_min`；≥ `quota_denial_critical` 升为 `critical`；
  - `upstream_mass_failure`——窗口内某 provider 的上游失败次数 ≥ `upstream_error_min`
    **且**失败率 ≥ `upstream_error_rate`（失败率分母 = 失败数 + `spend` 成功数）；
- 数据来源是数据面失败事件流 `request_error`（每次失败一条，带
  `provider/model/kind/class/reason/tenant/project/key`）。**成功请求不写这个流**，
  所以正常时写入量几乎为零；
- 去重与冷却：一个 `(rule, subject)` 最多一条 firing 记录；持续触发时按 `cooldown_sec`
  节流，级别升高立即重发；连续 `resolve_after` 次评估无信号才自动解除并推送 resolved；
- 阈值写在 `setting/alerts`（`GET/PUT /api/config/settings/{get,set}/alerts`，仅全局管理员），
  **改完下次评估生效，无需重启**；`webhook_url` 非空时按事件异步 POST（失败即丢，不影响网关）；
- 状态持久化在 JSONStore 前缀 `alert/`，重启后不会把同一告警当新告警重复推送。

### 6.8 前端集成要点

- **CORS 已就绪**：`LMGATEWAY_CORS_ORIGINS` 未设置时返回 `Access-Control-Allow-Origin: *`，
  且 `Authorization` 已在允许头列表里 → SPA 用 Bearer token 直接调即可（无需 cookie）。
- 若走 cookie 方案，需要把 origin 配进白名单（`*` 与凭据不能共用）。
- **token 存储**：建议内存 + `sessionStorage`（默认 12h 过期）；401 时统一跳登录页。
- **状态码约定**：401 未登录/凭据失效 · 403 权限不足或凭据用错面 · 404 资源不存在 ·
  409 冲突（重名/有下级资源/bootstrap 已完成）· 422 字段校验失败。
- **`key_hash` 永不出现在响应里**；`secret` 只在创建/轮换响应中出现一次——前端必须做「只显示一次」的提示。
- 初始化流程：`GET /api/tenancy/me` 401 → `POST /bootstrap`（若 409 说明已初始化，改走 `/login`）。
- **金额一律以 USD 交付**：后端所有 `cost` / 定价 / 预算都是 USD，控制台按服务端下发的汇率换算显示
  （见 §6.5）。不要把换算后的值写回任何配置字段，否则口径会混。

### 6.7 计费：缓存读取价

模型定价有三个单价（USD / 百万 token）：

| 字段 | 含义 |
| --- | --- |
| `input_per_mtok` | 未命中缓存的输入 |
| `output_per_mtok` | 输出 |
| `cached_input_per_mtok` | **命中 prompt cache 的输入**；留 0 = 未配置 |
| `cached_input_free` | 布尔；**显式声明缓存读取免费** |

关键语义：`prompt_tokens` 是**包含** `cached_tokens` 的（OpenAI 约定），所以计费会把命中部分拆出来：

```
cost = (prompt - cached)/1e6 × input
     + cached/1e6 × cachedUnit              ← 见下表
     + completion/1e6 × output
```

`cachedUnit` 的取值优先级：

| 配置 | `cachedUnit` | 说明 |
| --- | --- | --- |
| `cached_input_free: true` | `0` | 命中部分零成本 |
| `cached_input_per_mtok: 1.25` | `1.25` | 缓存折扣价 |
| 都没配（0 且 false） | `input_per_mtok` | 未配置 → 回退输入价 |

**为什么"免费"要单独一个开关**：`0` 已经表示"未配置（回退输入价）"。
再让 `0` 兼任"免费"就是两种相反口径压在一个值上——迟早算错账。
两者同时设置是**矛盾配置**，`config.Build` 直接报错（写入时 422、启动/重载时构建失败），不静默取其一。

几个刻意的选择：

- **未配置缓存价时回退输入价**，而不是按 0 计费：漏配不会让成本被静默低估。
- **`cached` 夹到 `[0, prompt]`**：上游 usage 偶有不一致时，成本不能变成负数。
- **预留（预扣）用上界**：请求入口还不知道会命中多少缓存，而缓存价有可能高于输入价
  （少见但合法），所以入口按两者较大者估算，保证预扣不低于实际。缓存免费时上界仍是输入价。
- 缓存命中量已经支持多 provider 归一化：`prompt_tokens_details.cached_tokens`（OpenAI）、
  `prompt_cache_hit_tokens`（DeepSeek）、`cache_read_input_tokens` 等。

端到端验证（`devharness/smoke-tenancy.sh` 第 18 节，同一 prompt=1000 / cached=400 / completion=100）：

| 模型口径 | 记录成本 | 若按全价 |
| --- | --- | --- |
| `cached_input_per_mtok: 1.25` | **0.003** | 0.0035 |
| `cached_input_free: true` | **0.0025** | 0.0035（若把 0 当成"未配置"） |
| 未配置 | 0.0035 | 0.0035 |

---

## 7. 运维

```bash
# 首次初始化（也可用 API）
curl -sX POST http://127.0.0.1:8080/api/tenancy/bootstrap \
  -H 'Content-Type: application/json' \
  -d '{"email":"ops@example.com","password":"supersecret1"}'

# 建租户 → 项目 → 密钥
curl -sX POST .../api/tenancy/tenants -H "Authorization: Bearer $SESSION" -d '{"id":"acme","name":"Acme"}'
curl -sX POST .../api/tenancy/projects -H "Authorization: Bearer $SESSION" -d '{"id":"web","tenant_id":"acme","name":"Web"}'
curl -sX POST .../api/tenancy/keys -H "Authorization: Bearer $SESSION" \
  -d '{"tenant_id":"acme","project_id":"web","name":"prod","rpm_limit":600}'
# → 响应里的 secret 就是客户端要用的 sk-lm-...
```

- 数据全在既有库里，**备份策略不变**（sqlite 文件 / pg 库）。
- 租户数据写到 store 会触发 `JSONStore.Watch`；我们对 `Manager.reloadLocked` 加了
  「有效配置未变则跳过重建」的短路，因此**建 key 不会重建 provider 连接池**（E2E 断言了 reload 次数）。

---

## 8. 代码地图

| 文件 | 职责 |
| --- | --- |
| `internal/tenancy/model.go` | 领域类型、状态/角色常量、校验、模型白名单匹配 |
| `internal/tenancy/secret.go` | PBKDF2 密码哈希、API key/session 生成与摘要、展示前缀 |
| `internal/tenancy/store.go` | Repository：JSONStore CRUD + 内存索引 + Watch/周期刷新 |
| `internal/tenancy/auth.go` | Identity、凭据解析、`Scope`、`Guard`/`GuardSoft` 中间件 |
| `internal/tenancy/quota.go` | 配额引擎：分钟窗口 + 日/月成本预扣结算 |
| `internal/tenancy/calibrate.go` | 从 spend 记录重建成本计数器（重启/定期） |
| `internal/tenancy/ids.go` | 随机后缀与邮箱派生 id |
| `internal/tenancyapi/api.go` | 管理 API（登录/CRUD/轮换/配额/用量） |
| `internal/handler/authorize.go` | 数据面授权 handler（白名单 + 配额） |

对既有代码的改动（全部为小改动）：

| 文件 | 改动 |
| --- | --- |
| `cmd/gateway/main.go` | 装配 repo/limiter/auth，挂载 `/api/tenancy/`，启动+周期校准 |
| `internal/config/config.go` | `BuildDeps` 增 `Quota`；注册 `authorize`；`buildTable` 插入边 |
| `internal/config/manager.go` | `SetQuota`；有效配置未变时跳过重建（避免租户写入触发 reload 风暴） |
| `internal/packet/packet.go` | 新增 `KeyIdentity`、`ErrPolicy` |
| `internal/httpapi/httpapi.go` | 把身份写进 packet；策略错误透传状态码 |
| `internal/handler/spend.go` | spend 记录加 `tenant`/`project`/`key` 标签 |

---

## 9. 实现注意：索引一致性（一个已修复的真实竞态）

`Repository` 在内存里维护索引（鉴权是每请求热路径，不能每次扫库），写入后重建索引。
早期实现有一个**真实竞态**，症状非常隐蔽：

> 创建一个 API key → 服务端返回 201 和明文 → 客户端立刻用它调用 → **401**。

根因：每次写入都会重建全量索引，而**写后重建**、**store 变更回调**、**定期兜底刷新**
三处都会触发重建。如果有两次重建交叠，"先启动但后完成"的那次会把**旧快照**换入，
于是出现"库里已经写成功、索引里却没有这把新密钥"。概率低、但一旦发生，
用户拿到的是一把**永远用不了的密钥**。

三层修复：

1. **重建串行化**：`Refresh` 用 `refreshMu` 保证同一时刻只有一个装载在跑。
   每次装载都在上一次换入之后才开始读，快照因此**单调不回退**。
2. **未命中回源**：`LookupAPIKey` 索引未命中时直接扫一次 store，并把结果补进索引。
   鉴权是安全路径，宁可慢一点也不能因为索引滞后把合法密钥判成无效。
3. **写后校验**：`CreateAPIKey` / `RotateAPIKey` 写入后回读文档比对哈希；
   不一致就返回错误（而不是把一把用不了的密钥发给用户）。
   把"静默地给出一把坏密钥"变成"响亮地失败"。

回归测试：`TestConcurrentCreateWithRefreshKeepsIndexFresh`（24 个并发创建 + 持续重建，
白盒断言索引里必须有刚创建的密钥）。**把第 1 层的 `refreshMu` 摘掉后该测试立刻失败**
（`已创建但不在内存索引里（旧快照赢得了换入）`），修复后稳定通过——测试确实能抓到这个 bug。

---

## 10. 实现注意：配置写入不接受打码值（`****`）

`GET /api/provider/get/{name}` 与 `list` 会把 `api_key` 打码成 `"****"`（并附 `has_api_key`），
这是对的——密钥不该回显。但由此带来一个**静默毁数据**的风险：

> 客户端"读-改-写"：GET 拿到文档 → 改个 `discover` → PUT 整条回去。
> 打码值 `"****"` 就这样被当成真密钥写进存储，而且 JSON Schema 拦不住（它是合法字符串）。

因此写入路径（`applyAndWrite`，PUT 与 PATCH 共用）会**显式拒绝**这个字面量：

```
422 api_key must not be the masked placeholder "****":
    omit it (PATCH preserves the stored key) or send the real key
```

正确的编辑姿势是 **PATCH**：它合并的是**未打码的真实文档**，不提交 `api_key` 就天然保留原值；
PUT 是整条替换，必须提交真实密钥。控制台按这个约定实现（新建 PUT / 编辑 PATCH），
并端到端验证过：只改 `discover` 时 `has_api_key` 仍为 true，提交 `****` 得到 422。

---

## 11. 后续可做（未实现）

1. **多实例共享计数器**（见 §5 限制 1）——水平扩展前必须做。
2. 滑动窗口限流、按模型的差异化限额。
3. ~~租户级配额~~——**已实现**：key / project / tenant 三层可各自设限并独立判定（§5）。
4. 审计日志（谁改了哪个 key）——**已实现**（§6.6）。
5. ~~告警~~——**已实现**（§6.7）。尚可增强：把告警通过 SSE 推给控制台（目前是列表 + 轮询刷新）、
   告警静默期/维护窗口、按模型的失败率规则。
6. 用量接口在超大数据量下的进一步优化：把聚合下推到 SQL（现在三个后端都是"取回后在内存聚合"），
   或给 `store_ts` 的 tags 加索引（sqlite 目前无 tags 索引）。
7. 前端 SPA（独立仓库，通过 `/api` 访问）——API 已按此设计。
