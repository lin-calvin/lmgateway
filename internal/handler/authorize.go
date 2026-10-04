package handler

import (
	"net/http"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
	"lmgateway/internal/tenancy"
)

// Authorizer 数据面授权 handler:模型白名单/黑名单 + RPM/TPM/成本配额。
//
// 位置很重要:它挂在 @ingress → observe → authorize → http 这条链上,即
// **在模型别名改写之前**(http 源上的别名规则尚未运行)。这样租户不能通过
// 请求别名来绕过自己 key 的模型白名单。
//
// 失败时直接以终态返回(不再调用 Continue),因此不会打到上游、不产生费用。
type Authorizer struct {
	limiter          *tenancy.Limiter
	pricing          map[string]Pricing
	defaultMaxOutput int
}

// NewAuthorizer 构造。pricing 与 model 实体一致(和 usage handler 用同一张表)。
func NewAuthorizer(limiter *tenancy.Limiter, pricing map[string]Pricing, defaultMaxOutput int) *Authorizer {
	if defaultMaxOutput <= 0 {
		defaultMaxOutput = 4096
	}
	return &Authorizer{limiter: limiter, pricing: pricing, defaultMaxOutput: defaultMaxOutput}
}

// Handle 实现 handler 契约。
func (a *Authorizer) Handle(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
	next := dispatch.Serve(func(out packet.Packet, sources ...string) packet.Packet {
		if len(serves) == 0 {
			return out
		}
		return serves[0](out, sources...)
	})

	identity, ok := IdentityOf(pkt)
	if !ok || identity.Kind != tenancy.IdentityAPIKey {
		// 鉴权关闭,或运维 master key 调用:不受租户配额约束。
		return next(pkt)
	}

	req, _ := pkt.Map("req")
	model, _ := req["model"].(string)

	// 1) 模型授权(客户端原始模型名,别名改写前)
	if !identity.AllowsModel(model) {
		return pkt.FailStatus(packet.ErrPolicy, http.StatusForbidden, 0, "",
			"model "+model+" is not permitted for this API key")
	}

	if a.limiter == nil {
		return next(pkt)
	}

	// 2) 成本上界估计 → 预扣
	estTokens := tenancy.EstimateOutputTokens(req, a.defaultMaxOutput)
	estCost := a.estimateCost(model, tenancy.EstimateInputTokens(req), estTokens)
	reservation, denial := a.limiter.Reserve(identity, estCost, estTokens)
	if denial != nil {
		return pkt.FailStatus(packet.ErrPolicy, http.StatusTooManyRequests, denial.RetryAfter, packet.ClassRateLimit, denial.Message)
	}

	// 3) 执行下游,结束后按实际用量结算
	out := next(pkt)
	if reservation != nil {
		tokens, cost := actualUsage(out, model, a.pricing)
		reservation.Settle(tokens, cost)
	}
	return out
}

// IdentityOf 从 packet 里取身份(由 HTTP 层写入)。
func IdentityOf(pkt packet.Packet) (*tenancy.Identity, bool) {
	if value, ok := pkt[packet.KeyIdentity]; ok {
		if identity, ok := value.(*tenancy.Identity); ok && identity != nil {
			return identity, true
		}
	}
	return nil, false
}

// estimateCost 用模型定价估算成本上界。模型没有定价时返回 0(不阻塞,仅不预扣)。
func (a *Authorizer) estimateCost(model string, inputTokens, outputTokens int) float64 {
	price, ok := a.pricing[model]
	if !ok {
		return 0
	}
	// 入口还不知道会命中多少缓存，用上界（含缓存价可能更高的情况）
	return price.CostUpperBound(inputTokens, outputTokens)
}

// actualUsage 从最终 packet 的响应文档里读真实 usage 并算成本。
func actualUsage(pkt packet.Packet, model string, pricing map[string]Pricing) (tokens int, cost float64) {
	resp, ok := pkt.Map("resp")
	if !ok {
		return 0, 0
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0
	}
	prompt := intField(usage, "prompt_tokens")
	if prompt == 0 {
		prompt = intField(usage, "input_tokens")
	}
	completion := intField(usage, "completion_tokens")
	if completion == 0 {
		completion = intField(usage, "output_tokens")
	}
	cached := cacheHitTokens(usage)
	if cached == 0 {
		cached = intField(nested(usage, "input_tokens_details"), "cached_tokens")
	}
	tokens = prompt + completion
	if price, ok := pricing[model]; ok {
		cost = price.Cost(prompt, cached, completion)
	}
	return tokens, cost
}

// 确保 time 被引用(保留给未来的窗口化限流实现)。
var _ = time.Now
