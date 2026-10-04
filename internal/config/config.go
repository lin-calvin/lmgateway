package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"lmgateway/internal/codingplan/codex"
	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/codingplan/commandcode"
	"lmgateway/internal/codingplan/openai"
	"lmgateway/internal/dispatch"
	"lmgateway/internal/handler"
	"lmgateway/internal/lm"
	"lmgateway/internal/match"
	"lmgateway/internal/packet"
	"lmgateway/internal/pool"
	"lmgateway/internal/store"
	"lmgateway/internal/table"
	"lmgateway/internal/tenancy"
)

// ConditionCfg 一条匹配条件
type ConditionCfg struct {
	Field  string `yaml:"field" json:"field"`
	Op     string `yaml:"op" json:"op"`
	Value  any    `yaml:"value" json:"value"`
	Values []any  `yaml:"values" json:"values"` // in 用
}

// RuleCfg 一条规则：无 priority/phase —— 位置(From)隐含相位，用户边先于自动边（分层），层内声明序。
// Set+To（无 Action）= 纯变换规则：改写请求后回走。
type RuleCfg struct {
	ID      string         `yaml:"id" json:"id,omitempty"`
	From    string         `yaml:"from" json:"from"`                 // 起始位置（包上 source），必填
	Action  string         `yaml:"action" json:"action,omitempty"`   // 运行的 handler 或终态；纯变换规则可空
	To      string         `yaml:"to" json:"to,omitempty"`           // 下一位置名；默认 = Action，无 Action 时必填
	Set     map[string]any `yaml:"set" json:"set,omitempty"`         // 硬覆盖：action 前无条件改写包字段
	Default map[string]any `yaml:"default" json:"default,omitempty"` // 软默认：仅当字段缺失/为空时写入
	Match   []ConditionCfg `yaml:"match" json:"match,omitempty"`
}

// ProviderCfg 纯端点连接器（不含 models）
type ProviderCfg struct {
	Name                  string         `yaml:"name" json:"name"`
	Type                  string         `yaml:"type" json:"type"` // 目前仅 openai
	BaseURL               string         `yaml:"base_url" json:"base_url"`
	Discover              bool           `yaml:"discover" json:"discover,omitempty"`
	APIKey                string         `yaml:"api_key" json:"api_key,omitempty"`
	APIKeyEnv             string         `yaml:"api_key_env" json:"api_key_env,omitempty"`
	TimeoutSec            int            `yaml:"timeout_sec" json:"timeout_sec,omitempty"`
	Options               map[string]any `yaml:"options" json:"options,omitempty"`
	Originator            string         `yaml:"originator" json:"originator,omitempty"`
	ForwardClientMetadata bool           `yaml:"forward_client_metadata" json:"forward_client_metadata,omitempty"`
}

// ModelCfg 模型实体 → 自动生成路由边 + pricing + upstream 改写
type ModelCfg struct {
	Name             string         `yaml:"name" json:"name"`                                       // 客户端看到的 model 名
	Provider         string         `yaml:"provider" json:"provider"`                               // 路由到哪个 provider
	UpstreamModel    string         `yaml:"upstream_model" json:"upstream_model,omitempty"`         // 实际发给上游的 model，默认 = Name
	ExtraBody        map[string]any `yaml:"extra_body" json:"extra_body,omitempty"`                 // 硬覆盖：上游请求体附加字段（thinking/reasoning 等）
	DefaultExtraBody map[string]any `yaml:"default_extra_body" json:"default_extra_body,omitempty"` // 软默认：仅当客户端未提供该字段时写入
	Default          bool           `yaml:"default" json:"default,omitempty"`                       // 请求无 model 时兜底
	InputPerMtok     float64        `yaml:"input_per_mtok" json:"input_per_mtok,omitempty"`
	OutputPerMtok    float64        `yaml:"output_per_mtok" json:"output_per_mtok,omitempty"`
	// CachedInputPerMtok 命中 prompt cache 的那部分输入的单价。
	// 留 0 表示"未配置"，此时缓存命中部分按 InputPerMtok 计费（保守，不会低估成本）。
	CachedInputPerMtok float64 `yaml:"cached_input_per_mtok" json:"cached_input_per_mtok,omitempty"`
	// CachedInputFree 显式声明"缓存读取免费"。
	//
	// 为什么需要这个开关而不是让 0 兼任两种含义：0 已经表示"未配置（回退输入价）"，
	// 用它同时表示"免费"会让两种完全相反的口径压在一个值上——迟早算错账。
	// 与 CachedInputPerMtok 同时设置是矛盾配置，Build 会直接报错（不静默取其一）。
	CachedInputFree bool `yaml:"cached_input_free" json:"cached_input_free,omitempty"`
}

// SpendCfg 热/冷分层设置
type SpendCfg struct {
	RawRetentionDays int      `yaml:"raw_retention_days" json:"raw_retention_days"`
	RollupDimensions []string `yaml:"rollup_dimensions" json:"rollup_dimensions"`
	Timezone         string   `yaml:"timezone" json:"timezone,omitempty"`
}

// DefaultSpendCfg 默认值
func DefaultSpendCfg() SpendCfg {
	return SpendCfg{RawRetentionDays: 7, RollupDimensions: []string{"provider", "model", "stream"}, Timezone: "UTC"}
}

// DisplayCfg 控制台展示设置。
//
// 金额在后端一律以 USD 存储与计算，这里只决定**怎么显示**：
// 汇率写死在前端会导致多端不一致、改汇率要重新发版，所以由服务端下发。
type DisplayCfg struct {
	Currency string  `yaml:"currency" json:"currency"`     // 默认显示币种：USD | CNY
	USDToCNY float64 `yaml:"usd_to_cny" json:"usd_to_cny"` // 1 USD = ? CNY
}

// SupportedCurrencies 控制台已实现符号与换算的币种。
// 新增币种需要同时在前端补符号表，所以这里用白名单而不是放任意字符串过去。
var SupportedCurrencies = []string{"USD", "CNY"}

// DefaultDisplayCfg 默认值
func DefaultDisplayCfg() DisplayCfg {
	return DisplayCfg{Currency: "USD", USDToCNY: 7.2}
}

// NormalizeDisplayCfg 补默认值；校验与读取共用，避免各处各写一份分支。
func NormalizeDisplayCfg(d DisplayCfg) DisplayCfg {
	out := d
	out.Currency = strings.ToUpper(strings.TrimSpace(out.Currency))
	if out.Currency == "" {
		out.Currency = DefaultDisplayCfg().Currency
	}
	if out.USDToCNY <= 0 {
		out.USDToCNY = DefaultDisplayCfg().USDToCNY
	}
	return out
}

// ValidateDisplayCfg 显式拒绝不支持的币种/非法汇率，而不是静默回落默认值——
// 静默回落会让"我明明改了汇率"变成排查半天的问题。
func ValidateDisplayCfg(d DisplayCfg) error {
	currency := strings.ToUpper(strings.TrimSpace(d.Currency))
	if currency != "" {
		ok := false
		for _, supported := range SupportedCurrencies {
			if currency == supported {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unsupported currency %q (supported: %s)", d.Currency, strings.Join(SupportedCurrencies, ", "))
		}
	}
	if d.USDToCNY < 0 {
		return fmt.Errorf("usd_to_cny must be > 0")
	}
	return nil
}

type LogprobsCfg struct {
	WebhookURL string `yaml:"webhook_url" json:"webhook_url,omitempty"`
	TimeoutSec int    `yaml:"timeout_sec" json:"timeout_sec,omitempty"`
}

// ServerCfg 网关监听设置
type ServerCfg struct {
	Addr      string `yaml:"addr" json:"addr"`
	MasterKey string `yaml:"master_key" json:"-"`
}

// PoolCfg 一个 backend pool：把逻辑模型 alias 映射到有序的后端模型 id 列表。
// 选择是 sticky 的：active 后端持续服务，直到它上报可重试的上游失败后被冷却，
// 下一个健康后端被提升。刻意不做 round-robin（会破坏上游 prefix/KV cache）。
type PoolCfg struct {
	Model        string   `yaml:"model" json:"model"`                             // 客户端调用的 alias
	Backend      []string `yaml:"backend" json:"backend"`                         // 有序的后端模型 id
	CooldownSec  int      `yaml:"cooldown_sec" json:"cooldown_sec,omitempty"`     // 无 retry-after 时的冷却
	OnAllLimited string   `yaml:"on_all_limited" json:"on_all_limited,omitempty"` // fail | force-least-recent
}

// Config 顶层配置
type Config struct {
	Server    ServerCfg     `yaml:"server" json:"server"`
	Providers []ProviderCfg `yaml:"providers" json:"providers"`
	Models    []ModelCfg    `yaml:"models" json:"models"`
	Pools     []PoolCfg     `yaml:"pools" json:"pools"`
	Rules     []RuleCfg     `yaml:"rules" json:"rules"`
	Spend     SpendCfg      `yaml:"spend" json:"spend"`
	Logprobs  LogprobsCfg   `yaml:"logprobs" json:"logprobs"`
	Display   DisplayCfg    `yaml:"display" json:"display"`
}

// Runtime ConfigManager 编译产物
type Runtime struct {
	Config     Config
	Dispatcher *dispatch.Dispatcher
	Registry   *dispatch.Registry
	Table      *table.Table
	providers  map[string]bool // provider names, kept for routing recompiles
}

// BuildDeps Build 的可选依赖（middleware 注入点）
type BuildDeps struct {
	Storage *store.Storage
	Codex   *codexauth.Service
	Pools   *pool.Controller
	// Quota 多租户配额限流器。非 nil 时注册 authorize handler 并插入路由边。
	Quota *tenancy.Limiter
}

// Load 从 YAML 编译（无持久化环境的便捷路径）
func Load(path string) (*Runtime, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return Build(cfg)
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s failed: %w", path, err)
	}
	return cfg, nil
}

func LoadServerCfg(path string) (ServerCfg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServerCfg{}, err
	}
	var cfg struct {
		Server ServerCfg `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ServerCfg{}, fmt.Errorf("parse config %s failed: %w", path, err)
	}
	return cfg.Server, nil
}

// Build 把 Config 编译成 Runtime：
//  1. models → pricing/upstream 映射
//  2. provider → handler 注册
//  3. 自动接线：model 路由边（含默认）+ provider→usage 边
//  4. 用户规则：override 自动（同 from+match 替换）+ 分层（用户先/自动后/层内声明序）
//  5. 全局 respond 兜底
func Build(cfg Config, deps ...BuildDeps) (*Runtime, error) {
	built, err := buildRegistry(cfg, deps)
	if err != nil {
		return nil, err
	}
	tbl, err := buildTable(cfg, built.registry, built.providers, built.pools)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		Config:     cfg,
		Registry:   built.registry,
		Table:      tbl,
		Dispatcher: dispatch.New(tbl, built.registry),
		providers:  built.providers,
	}, nil
}

// registryResult is the provider/handler layer of a compiled config. It is reused
// across in-memory routing recompiles so provider runtime state (HTTP clients,
// Codex/CommandCode sessions and device fingerprints) is preserved.
type registryResult struct {
	registry  *dispatch.Registry
	providers map[string]bool
	pools     *pool.Controller
}

// buildRegistry constructs the handler registry: builtins plus one handler per
// provider. It does not build routing.
func buildRegistry(cfg Config, deps []BuildDeps) (*registryResult, error) {
	var storage *store.Storage
	var poolsCtrl *pool.Controller
	var quota *tenancy.Limiter
	if len(deps) > 0 {
		storage = deps[0].Storage
		poolsCtrl = deps[0].Pools
		quota = deps[0].Quota
	}
	if len(cfg.Pools) > 0 && poolsCtrl == nil {
		return nil, fmt.Errorf("pools configured but no pool controller was provided")
	}

	reg := dispatch.NewRegistry()

	// models → pricing / upstream / extraBody
	pricing := map[string]handler.Pricing{}
	upstream := map[string]string{}
	extraBody := map[string]map[string]any{}
	defaultExtraBody := map[string]map[string]any{}
	for _, m := range cfg.Models {
		if m.Name == "" || m.Provider == "" {
			continue
		}
		// 矛盾配置直接报错，不静默取其一：两种口径压在一个模型上必然算错账。
		// 写入配置时表现为 422，启动/重载时表现为构建失败（都是"响亮地失败"）。
		if m.CachedInputFree && m.CachedInputPerMtok > 0 {
			return nil, fmt.Errorf(
				"model %q: cached_input_free and cached_input_per_mtok are mutually exclusive "+
					"(set cached_input_free for free cache reads, or a price, not both)", m.Name)
		}
		if m.UpstreamModel != "" {
			upstream[m.Name] = m.UpstreamModel
		}
		if len(m.ExtraBody) > 0 {
			extraBody[m.Name] = m.ExtraBody
		}
		if len(m.DefaultExtraBody) > 0 {
			defaultExtraBody[m.Name] = m.DefaultExtraBody
		}
		if m.InputPerMtok > 0 || m.OutputPerMtok > 0 || m.CachedInputPerMtok > 0 || m.CachedInputFree {
			pricing[m.Name] = handler.Pricing{
				InputPerMtok:       m.InputPerMtok,
				OutputPerMtok:      m.OutputPerMtok,
				CachedInputPerMtok: m.CachedInputPerMtok,
				CachedInputFree:    m.CachedInputFree,
			}
		}
	}

	if storage != nil && storage.TS != nil {
		reg.Register("usage", handler.NewSpendRecorder(storage.TS, pricing).Handle)
	} else {
		reg.Register("usage", handler.Usage)
	}
	reg.Register("stream", handler.Stream{}.Handle)
	reg.Register("observe", handler.Observe{}.Handle)
	if poolsCtrl != nil && len(cfg.Pools) > 0 {
		reg.Register("ratelimit", handler.NewRatelimit(poolsCtrl).Handle)
	}
	reg.Register("logprobs", handler.NewLogprobsExporter(handler.LogprobsConfig{
		WebhookURL: cfg.Logprobs.WebhookURL,
		TimeoutSec: cfg.Logprobs.TimeoutSec,
	}).Handle)
	// 多租户授权(模型白名单 + RPM/TPM/成本配额)。未注入限流器时不注册,
	// 路由表也不会插入 authorize 边 —— 单租户行为完全不变。
	if quota != nil {
		reg.Register("authorize", handler.NewAuthorizer(quota, pricing, 0).Handle)
	}

	providers := map[string]bool{}
	for _, p := range cfg.Providers {
		if p.Name == "" {
			continue
		}
		switch p.Type {
		case "openai", "openai_response":
			key := p.APIKey
			if key == "" && p.APIKeyEnv != "" {
				key = os.Getenv(p.APIKeyEnv)
			}
			documentType, ok := ProviderDocumentType(p.Type)
			if !ok {
				return nil, fmt.Errorf("provider %s: no document type for %q", p.Name, p.Type)
			}
			o := openai.NewOpenAI(openai.OpenAIConfig{
				Name:               p.Name,
				BaseURL:            p.BaseURL,
				APIKey:             key,
				DocumentType:       documentType,
				TimeoutSec:         p.TimeoutSec,
				Upstream:           upstream,
				ExtraBody:          extraBody,
				DefaultExtraBody:   defaultExtraBody,
				DisableStreamUsage: streamUsageDisabled(p.Options),
			})
			if p.Discover && p.Type == "openai" {
				reg.RegisterProvider(p.Name, o.Handle, o.Discover)
			} else {
				reg.Register(p.Name, o.Handle)
			}
		case "chatgpt_codex":
			if len(deps) == 0 || deps[0].Codex == nil {
				return nil, fmt.Errorf("provider %s: chatgpt_codex requires codex auth service", p.Name)
			}
			c := codex.NewChatGPTCodex(codex.ChatGPTCodexConfig{
				Name:                  p.Name,
				BaseURL:               p.BaseURL,
				Originator:            p.Originator,
				ForwardClientMetadata: p.ForwardClientMetadata,
				TimeoutSec:            p.TimeoutSec,
				Auth:                  deps[0].Codex,
				Upstream:              upstream,
				ExtraBody:             extraBody,
				DefaultExtraBody:      defaultExtraBody,
			})
			if p.Discover {
				reg.RegisterProvider(p.Name, c.Handle, c.Discover)
			} else {
				reg.Register(p.Name, c.Handle)
			}
		case "commandcode":
			key := p.APIKey
			if key == "" && p.APIKeyEnv != "" {
				key = os.Getenv(p.APIKeyEnv)
			}
			cc := commandcode.New(commandcode.Config{
				Name:                   p.Name,
				BaseURL:                p.BaseURL,
				APIKey:                 key,
				ProjectSlug:            optionString(p.Options, "project_slug"),
				DeviceProjectDir:       optionString(p.Options, "device_project_dir"),
				FingerprintSalt:        optionString(p.Options, "fingerprint_salt"),
				CLIMode:                optionString(p.Options, "cli_mode"),
				CLISessionMode:         optionString(p.Options, "cli_session_mode"),
				ZDR:                    optionBool(p.Options, "zdr", false),
				EmptySystemPlaceholder: optionBool(p.Options, "empty_system_placeholder", true),
				StreamIdleMS:           optionInt(p.Options, "stream_idle_ms", 0),
				NonStreamIdleMS:        optionInt(p.Options, "nonstream_idle_ms", 0),
				RetryMax:               optionInt(p.Options, "upstream_retry_max", 2),
				RetryBaseMS:            optionInt(p.Options, "upstream_retry_base_ms", 400),
				TimeoutSec:             p.TimeoutSec,
				Upstream:               upstream,
				ExtraBody:              extraBody,
				DefaultExtraBody:       defaultExtraBody,
			})
			if p.Discover {
				reg.RegisterProvider(p.Name, cc.Handle, cc.Discover)
			} else {
				reg.Register(p.Name, cc.Handle)
			}
		default:
			return nil, fmt.Errorf("provider %s: unknown type %q", p.Name, p.Type)
		}
		providers[p.Name] = true
	}
	return &registryResult{registry: reg, providers: providers, pools: poolsCtrl}, nil
}

// buildTable compiles routing (auto edges, pool alias rules, user rules) against
// an already-built registry. It is safe to call repeatedly with the same registry
// to recompile routing without recreating providers.
func buildTable(cfg Config, reg *dispatch.Registry, providers map[string]bool, poolsCtrl *pool.Controller) (*table.Table, error) {
	tbl := table.New()
	withPools := poolsCtrl != nil && len(cfg.Pools) > 0

	defaultProvider := ""
	defaultModel := ""
	for _, m := range cfg.Models {
		if m.Default && m.Provider != "" {
			defaultProvider = m.Provider
			defaultModel = m.Name
		}
	}

	// 自动接线（model 实体 → 路由边）
	var auto []table.Rule
	// pools 存在时，被动 ratelimit 上报器插在 observe 与 http 之间；它只上报，
	// 不选择、不重试。数据面因此只有 observe + pool alias 规则。
	afterObserve := packet.SourceHTTP
	if withPools {
		afterObserve = "ratelimit"
		auto = append(auto, table.Rule{From: "ratelimit", Action: "ratelimit", To: packet.SourceHTTP, Generated: true})
	}
	if reg.Has("authorize") {
		// authorize 必须挂在 http 之前:它在模型别名改写前做白名单判定,
		// 否则租户可以借别名绕过自己 key 的模型权限。
		auto = append(auto, table.Rule{From: packet.SourceIngress, Action: "observe", To: "authorize", Generated: true})
		auto = append(auto, table.Rule{From: "authorize", Action: "authorize", To: afterObserve, Generated: true})
	} else {
		auto = append(auto, table.Rule{From: packet.SourceIngress, Action: "observe", To: afterObserve, Generated: true})
	}
	for _, m := range cfg.Models {
		if m.Name == "" {
			continue
		}
		if !providers[m.Provider] {
			return nil, fmt.Errorf("model %s: references unknown provider %q", m.Name, m.Provider)
		}
		auto = append(auto, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpEq, Value: m.Name}),
			Action:    m.Provider,
			Generated: true,
		})
	}
	if defaultProvider != "" && providers[defaultProvider] {
		auto = append(auto, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpEmpty}),
			Set:       map[string]any{"model": defaultModel},
			Action:    defaultProvider,
			Generated: true,
		})
	}
	for name := range providers {
		auto = append(auto, table.Rule{From: name, Action: "usage", Generated: true})
	}
	auto = append(auto, table.Rule{From: packet.SourceResponse, Action: "usage", Generated: true})
	auto = append(auto, table.Rule{From: "usage", Action: "logprobs", Generated: true})
	auto = append(auto, table.Rule{From: "stream", Action: "stream", Generated: true})

	// 自动发现模型的前缀路由：[provider]/xxx → provider（仅显式开启 discovery 的 OpenAI-compatible provider）
	for name := range providers {
		if !providerDiscoveryEnabled(name, cfg.Providers) {
			continue
		}
		auto = append(auto, table.Rule{
			From:      "http",
			Match:     match.All(match.Condition{Field: "model", Op: match.OpPrefix, Value: name + "/"}),
			Action:    name,
			Generated: true,
		})
	}

	// pools（handler-less）：alias → active 后端模型 id 的纯变换规则。控制器在
	// 内存中轮换 active 后通过 Manager.Recompile 重新生成这条规则。
	if len(cfg.Pools) > 0 {
		modelNames := map[string]bool{}
		for _, m := range cfg.Models {
			modelNames[m.Name] = true
		}
		for _, p := range cfg.Pools {
			if p.Model == "" || len(p.Backend) == 0 {
				return nil, fmt.Errorf("pool: model and backend are required")
			}
			if modelNames[p.Model] {
				return nil, fmt.Errorf("pool %s: name collides with a model", p.Model)
			}
			for _, backend := range p.Backend {
				if backend == p.Model {
					return nil, fmt.Errorf("pool %s: a backend must differ from the pool model", p.Model)
				}
			}
			poolsCtrl.Register(pool.Config{
				Model:        p.Model,
				Backend:      p.Backend,
				CooldownSec:  p.CooldownSec,
				OnAllLimited: p.OnAllLimited,
			})
			active, ok := poolsCtrl.Active(p.Model)
			if !ok {
				active = p.Backend[0]
			}
			auto = append(auto, table.Rule{
				From:      "http",
				Match:     match.All(match.Condition{Field: "model", Op: match.OpEq, Value: p.Model}),
				Set:       map[string]any{"model": active},
				To:        "http",
				Generated: true,
			})
		}
	}

	// 用户规则（编译 + 校验）
	var user []table.Rule
	for i, r := range cfg.Rules {
		if r.From == "" {
			return nil, fmt.Errorf("rules[%d]: from is required", i)
		}
		for path := range r.Set {
			if strings.HasPrefix(path, "req.") || strings.HasPrefix(path, "resp.") {
				return nil, fmt.Errorf("rules[%d]: set path %q must not use req./resp. prefix", i, path)
			}
		}
		for path := range r.Default {
			if strings.HasPrefix(path, "req.") || strings.HasPrefix(path, "resp.") {
				return nil, fmt.Errorf("rules[%d]: default path %q must not use req./resp. prefix", i, path)
			}
		}
		for j, condition := range r.Match {
			if !validRuleMatchField(condition.Field) {
				return nil, fmt.Errorf("rules[%d].match[%d]: unknown field %q", i, j, condition.Field)
			}
		}
		if r.Action == "" && r.To == "" {
			return nil, fmt.Errorf("rules[%d]: action or to is required", i)
		}
		if r.Action == "" && len(r.Set) == 0 && len(r.Default) == 0 {
			return nil, fmt.Errorf("rules[%d]: pure transform rule (no action) needs set or default", i)
		}
		if r.Action != "" && r.Action != dispatch.ActionRespond && r.Action != dispatch.ActionError && !reg.Has(r.Action) {
			return nil, fmt.Errorf("rules[%d]: action %q not registered (provider, builtin handler, or respond/error)", i, r.Action)
		}
		var conds []match.Condition
		for j, c := range r.Match {
			value := c.Value
			if c.Op == "in" {
				value = c.Values
			}
			cond, err := match.Compile(c.Field, match.Op(c.Op), value)
			if err != nil {
				return nil, fmt.Errorf("rules[%d].match[%d]: %w", i, j, err)
			}
			conds = append(conds, cond)
		}
		user = append(user, table.Rule{ID: r.ID, From: r.From, Match: match.All(conds...), Set: r.Set, Default: r.Default, Action: r.Action, To: r.To})
	}

	// override：同 (from, match) 用户边替换自动边；分层：用户先 add，自动后 add
	auto = applyOverride(auto, user)
	for _, r := range user {
		tbl.Add(r)
	}
	for _, r := range auto {
		tbl.Add(r)
	}
	tbl.Add(table.Rule{Action: dispatch.ActionRespond, Generated: true}) // 全局 respond 兜底
	return tbl, nil
}

// rebuildRouting reuses an existing registry and only recompiles routing. Used by
// the manager for in-memory pool rotation so provider state is preserved.
func rebuildRouting(cfg Config, prev *Runtime, poolsCtrl *pool.Controller) (*Runtime, error) {
	tbl, err := buildTable(cfg, prev.Registry, prev.providers, poolsCtrl)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		Config:     cfg,
		Registry:   prev.Registry,
		Table:      tbl,
		Dispatcher: dispatch.New(tbl, prev.Registry),
		providers:  prev.providers,
	}, nil
}

func ProviderDocumentType(providerType string) (string, bool) {
	return lm.ProviderDocumentType(providerType)
}

func validRuleMatchField(field string) bool {
	for _, allowed := range []string{"type", "model", "stream", "metadata.", "raw.", "provider", "error", "error_kind"} {
		if field == allowed || strings.HasSuffix(allowed, ".") && strings.HasPrefix(field, allowed) {
			return true
		}
	}
	return false
}

// streamUsageDisabled reports whether a provider explicitly opts out of the
// gateway-injected stream_options.include_usage. Default is enabled.
func streamUsageDisabled(options map[string]any) bool {
	value, ok := options["stream_usage"].(bool)
	return ok && !value
}

func optionString(options map[string]any, key string) string {
	if options == nil {
		return ""
	}
	value, _ := options[key].(string)
	return value
}

func optionBool(options map[string]any, key string, fallback bool) bool {
	if options == nil {
		return fallback
	}
	value, ok := options[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func optionInt(options map[string]any, key string, fallback int) int {
	if options == nil {
		return fallback
	}
	switch value := options[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

func providerDiscoveryEnabled(name string, providers []ProviderCfg) bool {
	for _, p := range providers {
		if p.Name == name {
			return p.Discover
		}
	}
	return false
}

// applyOverride 移除与用户边同 (from, match) 的自动边
func applyOverride(auto, user []table.Rule) []table.Rule {
	userKeys := map[string]bool{}
	for _, u := range user {
		userKeys[ruleKey(u)] = true
	}
	var out []table.Rule
	for _, a := range auto {
		if !userKeys[ruleKey(a)] {
			out = append(out, a)
		}
	}
	return out
}

// ruleKey 规范化 (from + 排序后的条件) 作为身份
func ruleKey(r table.Rule) string {
	var conds []string
	for _, c := range r.Match.Conds {
		conds = append(conds, c.Field+"|"+string(c.Op)+"|"+fmt.Sprint(c.Value))
	}
	sort.Strings(conds)
	return r.From + ";" + strings.Join(conds, ";")
}

// ---- store 装载 ----

// ProviderKey / ModelKey / RuleKey / SettingKey
func ProviderKey(name string) string { return "provider/" + name }
func ModelKey(name string) string    { return "model/" + name }
func PoolKey(model string) string    { return "pool/" + model }
func RuleKey(id string) string       { return "rule/" + id }
func SettingKey(name string) string  { return "setting/" + name }

func cloneConfig(cfg Config) Config {
	out := cfg
	out.Providers = append([]ProviderCfg(nil), cfg.Providers...)
	for i := range out.Providers {
		out.Providers[i].Options = cloneAnyMap(cfg.Providers[i].Options)
	}
	out.Models = append([]ModelCfg(nil), cfg.Models...)
	for i := range out.Models {
		out.Models[i].ExtraBody = cloneAnyMap(cfg.Models[i].ExtraBody)
	}
	out.Pools = append([]PoolCfg(nil), cfg.Pools...)
	for i := range out.Pools {
		out.Pools[i].Backend = append([]string(nil), cfg.Pools[i].Backend...)
	}
	out.Rules = append([]RuleCfg(nil), cfg.Rules...)
	for i := range out.Rules {
		out.Rules[i].Set = cloneAnyMap(cfg.Rules[i].Set)
		out.Rules[i].Match = append([]ConditionCfg(nil), cfg.Rules[i].Match...)
		for j := range out.Rules[i].Match {
			out.Rules[i].Match[j].Value = cloneAny(out.Rules[i].Match[j].Value)
			out.Rules[i].Match[j].Values = append([]any(nil), cfg.Rules[i].Match[j].Values...)
		}
	}
	out.Spend.RollupDimensions = append([]string(nil), cfg.Spend.RollupDimensions...)
	return out
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = cloneAny(value)
	}
	return out
}

func cloneAny(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneAny(item)
		}
		return out
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

type ResolvedItem struct {
	Key       string
	Source    string
	Version   int64
	UpdatedAt time.Time
	Data      json.RawMessage
}

func ResolveItem(ctx context.Context, js store.JSONStore, base Config, key string) (ResolvedItem, error) {
	baseline, hasBaseline := BaselineItem(base, key)
	doc, err := js.Get(ctx, key)
	if err == store.ErrNotFound {
		if !hasBaseline {
			return ResolvedItem{}, store.ErrNotFound
		}
		data, marshalErr := json.Marshal(baseline)
		return ResolvedItem{Key: key, Source: SourceYAML, Data: data}, marshalErr
	}
	if err != nil {
		return ResolvedItem{}, err
	}
	source, payload, hasSource, err := DecodeItem(doc.Data)
	if err != nil {
		return ResolvedItem{}, fmt.Errorf("decode config item %s: %w", key, err)
	}
	if hasSource && source == SourceYAML {
		if !hasBaseline {
			return ResolvedItem{}, store.ErrNotFound
		}
		payload, err = json.Marshal(baseline)
		if err != nil {
			return ResolvedItem{}, err
		}
	} else if !hasSource {
		if hasBaseline {
			source = SourceDBOverride
		} else {
			source = SourceDB
		}
	}
	return ResolvedItem{Key: key, Source: source, Version: doc.Version, UpdatedAt: doc.UpdatedAt, Data: payload}, nil
}

func ListResolvedItems(ctx context.Context, js store.JSONStore, base Config, prefix string) ([]ResolvedItem, error) {
	keys := map[string]bool{}
	switch prefix {
	case "provider/":
		for _, value := range base.Providers {
			keys[ProviderKey(value.Name)] = true
		}
	case "model/":
		for _, value := range base.Models {
			keys[ModelKey(value.Name)] = true
		}
	case "pool/":
		for _, value := range base.Pools {
			keys[PoolKey(value.Model)] = true
		}
	case "rule/":
		for index, value := range base.Rules {
			id := value.ID
			if id == "" {
				id = fmt.Sprintf("rule-%d", index)
			}
			keys[RuleKey(id)] = true
		}
	case "setting/":
		keys[ServerSettingKey] = true
		keys[SpendSettingKey] = true
		keys[LogprobsSettingKey] = true
		keys[DisplaySettingKey] = true
	}
	docs, err := js.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		keys[doc.Key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	items := make([]ResolvedItem, 0, len(ordered))
	for _, key := range ordered {
		item, err := ResolveItem(ctx, js, base, key)
		if err == store.ErrNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// ServerSettingKey / SpendSettingKey are persisted configuration item keys.
const (
	ServerSettingKey   = "setting/server"
	SpendSettingKey    = "setting/spend"
	LogprobsSettingKey = "setting/logprobs"
	DisplaySettingKey  = "setting/display"
)

const (
	SourceYAML       = "yaml"
	SourceDBOverride = "db_override"
	SourceDB         = "db"
)

func EncodeItem(source string, value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var item map[string]any
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, err
	}
	item["source"] = source
	return item, nil
}

func DecodeItem(data []byte) (source string, payload []byte, hasSource bool, err error) {
	var item map[string]any
	if err := json.Unmarshal(data, &item); err != nil {
		return "", nil, false, err
	}
	value, ok := item["source"]
	if !ok {
		return "", data, false, nil
	}
	source, ok = value.(string)
	if !ok || source == "" {
		return "", nil, true, fmt.Errorf("invalid config item source")
	}
	if source != SourceYAML && source != SourceDBOverride && source != SourceDB {
		return "", nil, true, fmt.Errorf("unknown config item source %q", source)
	}
	delete(item, "source")
	payload, err = json.Marshal(item)
	return source, payload, true, err
}

func BaselineItem(base Config, key string) (any, bool) {
	switch {
	case strings.HasPrefix(key, "provider/"):
		name := strings.TrimPrefix(key, "provider/")
		for _, value := range base.Providers {
			if value.Name == name {
				return value, true
			}
		}
	case strings.HasPrefix(key, "model/"):
		name := strings.TrimPrefix(key, "model/")
		for _, value := range base.Models {
			if value.Name == name {
				return value, true
			}
		}
	case strings.HasPrefix(key, "pool/"):
		model := strings.TrimPrefix(key, "pool/")
		for _, value := range base.Pools {
			if value.Model == model {
				return value, true
			}
		}
	case strings.HasPrefix(key, "rule/"):
		id := strings.TrimPrefix(key, "rule/")
		for index, value := range base.Rules {
			if value.ID == id {
				return value, true
			}
			if value.ID == "" && fmt.Sprintf("rule-%d", index) == id {
				value.ID = id
				return value, true
			}
		}
	case key == ServerSettingKey:
		return base.Server, true
	case key == SpendSettingKey:
		spend := base.Spend
		if spend.RawRetentionDays <= 0 {
			spend.RawRetentionDays = DefaultSpendCfg().RawRetentionDays
		}
		if len(spend.RollupDimensions) == 0 {
			spend.RollupDimensions = DefaultSpendCfg().RollupDimensions
		}
		return spend, true
	case key == LogprobsSettingKey:
		return base.Logprobs, true
	case key == DisplaySettingKey:
		return NormalizeDisplayCfg(base.Display), true
	}
	return nil, false
}
func resolveStoredItem(base Config, doc *store.JSONDoc) ([]byte, string, error) {
	source, payload, hasSource, err := DecodeItem(doc.Data)
	if err != nil {
		return nil, "", fmt.Errorf("decode config item %s: %w", doc.Key, err)
	}
	if hasSource && source == SourceYAML {
		value, ok := BaselineItem(base, doc.Key)
		if !ok {
			return nil, source, nil
		}
		payload, err = json.Marshal(value)
		return payload, source, err
	}
	if !hasSource {
		if _, ok := BaselineItem(base, doc.Key); ok {
			source = SourceDBOverride
		} else {
			source = SourceDB
		}
	}
	return payload, source, nil
}

// ConfigFromStore 从 JSONStore 组装 Config（热配置数据源）
func ConfigFromStore(ctx context.Context, js store.JSONStore) (Config, error) {
	return ConfigFromStoreWithBase(ctx, js, Config{})
}

func ConfigFromStoreWithBase(ctx context.Context, js store.JSONStore, base Config) (Config, error) {
	cfg := cloneConfig(base)
	if sd, err := js.Get(ctx, ServerSettingKey); err == nil {
		data, source, resolveErr := resolveStoredItem(base, sd)
		if resolveErr != nil {
			return cfg, resolveErr
		}
		if data != nil && source != SourceYAML {
			// 注意：ServerCfg.MasterKey 的 tag 是 json:"-"，因此这里**永远不会**
			// 被存储文档覆盖——主密钥只能来自 YAML/环境变量，是刻意的启动凭据。
			// 也就是说设置文档既不能回显也不能改写它（见 config_test.go 的不变量测试）。
			_ = json.Unmarshal(data, &cfg.Server)
		}
	} else if err != store.ErrNotFound {
		return cfg, err
	}
	if sd, err := js.Get(ctx, SpendSettingKey); err == nil {
		data, source, resolveErr := resolveStoredItem(base, sd)
		if resolveErr != nil {
			return cfg, resolveErr
		}
		if data != nil && source != SourceYAML {
			_ = json.Unmarshal(data, &cfg.Spend)
		}
	} else if err != store.ErrNotFound {
		return cfg, err
	}
	if sd, err := js.Get(ctx, LogprobsSettingKey); err == nil {
		data, source, resolveErr := resolveStoredItem(base, sd)
		if resolveErr != nil {
			return cfg, resolveErr
		}
		if data != nil && source != SourceYAML {
			_ = json.Unmarshal(data, &cfg.Logprobs)
		}
	} else if err != store.ErrNotFound {
		return cfg, err
	}
	if sd, err := js.Get(ctx, DisplaySettingKey); err == nil {
		data, source, resolveErr := resolveStoredItem(base, sd)
		if resolveErr != nil {
			return cfg, resolveErr
		}
		if data != nil && source != SourceYAML {
			_ = json.Unmarshal(data, &cfg.Display)
		}
	} else if err != store.ErrNotFound {
		return cfg, err
	}
	cfg.Display = NormalizeDisplayCfg(cfg.Display)

	{
		docs, err := js.List(ctx, "provider/")
		if err != nil {
			return cfg, err
		}
		providers := make(map[string]int, len(cfg.Providers))
		for i, p := range cfg.Providers {
			providers[p.Name] = i
		}
		for _, d := range docs {
			data, _, resolveErr := resolveStoredItem(base, d)
			if resolveErr != nil {
				return cfg, resolveErr
			}
			if data == nil {
				continue
			}
			var p ProviderCfg
			if err := json.Unmarshal(data, &p); err != nil {
				return cfg, fmt.Errorf("decode provider %s: %w", d.Key, err)
			}
			if i, ok := providers[p.Name]; ok {
				cfg.Providers[i] = p
			} else {
				providers[p.Name] = len(cfg.Providers)
				cfg.Providers = append(cfg.Providers, p)
			}
		}
	}
	{
		docs, err := js.List(ctx, "model/")
		if err != nil {
			return cfg, err
		}
		models := make(map[string]int, len(cfg.Models))
		for i, m := range cfg.Models {
			models[m.Name] = i
		}
		for _, d := range docs {
			data, _, resolveErr := resolveStoredItem(base, d)
			if resolveErr != nil {
				return cfg, resolveErr
			}
			if data == nil {
				continue
			}
			var m ModelCfg
			if err := json.Unmarshal(data, &m); err != nil {
				return cfg, fmt.Errorf("decode model %s: %w", d.Key, err)
			}
			if i, ok := models[m.Name]; ok {
				cfg.Models[i] = m
			} else {
				models[m.Name] = len(cfg.Models)
				cfg.Models = append(cfg.Models, m)
			}
		}
	}
	{
		docs, err := js.List(ctx, "pool/")
		if err != nil {
			return cfg, err
		}
		pools := make(map[string]int, len(cfg.Pools))
		for i, p := range cfg.Pools {
			pools[p.Model] = i
		}
		for _, d := range docs {
			data, _, resolveErr := resolveStoredItem(base, d)
			if resolveErr != nil {
				return cfg, resolveErr
			}
			if data == nil {
				continue
			}
			var p PoolCfg
			if err := json.Unmarshal(data, &p); err != nil {
				return cfg, fmt.Errorf("decode pool %s: %w", d.Key, err)
			}
			if p.Model == "" {
				p.Model = strings.TrimPrefix(d.Key, "pool/")
			}
			if i, ok := pools[p.Model]; ok {
				cfg.Pools[i] = p
			} else {
				pools[p.Model] = len(cfg.Pools)
				cfg.Pools = append(cfg.Pools, p)
			}
		}
	}
	{
		docs, err := js.List(ctx, "rule/")
		if err != nil {
			return cfg, err
		}
		rules := make(map[string]int, len(cfg.Rules))
		for i, r := range cfg.Rules {
			if r.ID != "" {
				rules[r.ID] = i
			}
		}
		for _, d := range docs {
			data, _, resolveErr := resolveStoredItem(base, d)
			if resolveErr != nil {
				return cfg, resolveErr
			}
			if data == nil {
				continue
			}
			var r RuleCfg
			if err := json.Unmarshal(data, &r); err != nil {
				return cfg, fmt.Errorf("decode rule %s: %w", d.Key, err)
			}
			if i, ok := rules[r.ID]; ok {
				cfg.Rules[i] = r
			} else {
				rules[r.ID] = len(cfg.Rules)
				cfg.Rules = append(cfg.Rules, r)
			}
		}
	}
	return cfg, nil
}

// SeedFromFile loads the YAML baseline and removes only database documents
// that are exact copies of that baseline. YAML is never persisted.
func SeedFromFile(ctx context.Context, js store.JSONStore, path string) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	return ReconcileBaseline(ctx, js, cfg)
}

func ReconcileBaseline(ctx context.Context, js store.JSONStore, cfg Config) error {
	for _, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("baseline: provider name is required")
		}
		if err := reconcileItem(ctx, js, ProviderKey(p.Name), p); err != nil {
			return err
		}
	}
	for _, m := range cfg.Models {
		if m.Name == "" {
			return fmt.Errorf("baseline: model name is required")
		}
		if err := reconcileItem(ctx, js, ModelKey(m.Name), m); err != nil {
			return err
		}
	}
	for _, p := range cfg.Pools {
		if p.Model == "" {
			return fmt.Errorf("baseline: pool model is required")
		}
		if err := reconcileItem(ctx, js, PoolKey(p.Model), p); err != nil {
			return err
		}
	}
	for i, r := range cfg.Rules {
		if r.ID == "" {
			r.ID = fmt.Sprintf("rule-%d", i)
		}
		if err := reconcileItem(ctx, js, RuleKey(r.ID), r); err != nil {
			return err
		}
	}
	if cfg.Server.Addr != "" || cfg.Server.MasterKey != "" {
		if err := reconcileItem(ctx, js, ServerSettingKey, cfg.Server); err != nil {
			return err
		}
	}
	spend := cfg.Spend
	if spend.RawRetentionDays > 0 || len(spend.RollupDimensions) > 0 || spend.Timezone != "" {
		if spend.RawRetentionDays <= 0 {
			spend.RawRetentionDays = DefaultSpendCfg().RawRetentionDays
		}
		if len(spend.RollupDimensions) == 0 {
			spend.RollupDimensions = DefaultSpendCfg().RollupDimensions
		}
		if err := reconcileItem(ctx, js, SpendSettingKey, spend); err != nil {
			return err
		}
	}
	if cfg.Logprobs.WebhookURL != "" {
		if err := reconcileItem(ctx, js, LogprobsSettingKey, cfg.Logprobs); err != nil {
			return err
		}
	}
	if cfg.Display.Currency != "" || cfg.Display.USDToCNY > 0 {
		if err := reconcileItem(ctx, js, DisplaySettingKey, NormalizeDisplayCfg(cfg.Display)); err != nil {
			return err
		}
	}
	return nil
}

func reconcileItem(ctx context.Context, js store.JSONStore, key string, baseline any) error {
	doc, err := js.Get(ctx, key)
	if err == store.ErrNotFound {
		item := map[string]any{"source": SourceYAML}
		_, err = js.Put(ctx, key, item)
		return err
	}
	if err != nil {
		return err
	}
	source, payload, hasSource, err := DecodeItem(doc.Data)
	if err != nil {
		return fmt.Errorf("decode stored config item %s: %w", key, err)
	}
	if hasSource {
		if source == SourceYAML {
			_, err = js.Put(ctx, key, map[string]any{"source": SourceYAML})
		}
		return err
	}
	var current any
	if err := json.Unmarshal(payload, &current); err != nil {
		return err
	}
	encoded, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	var expected any
	if err := json.Unmarshal(encoded, &expected); err != nil {
		return err
	}
	if reflect.DeepEqual(current, expected) {
		_, err = js.Put(ctx, key, map[string]any{"source": SourceYAML})
		return err
	}
	item, ok := current.(map[string]any)
	if !ok {
		return fmt.Errorf("stored config item %s must be an object", key)
	}
	item["source"] = SourceDBOverride
	_, err = js.Put(ctx, key, item)
	return err
}
