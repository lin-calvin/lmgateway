package dispatch

import (
	"context"
	"fmt"

	"lmgateway/internal/packet"
	"lmgateway/internal/table"
)

// 终态 action（不进入 handler 注册表）
const (
	ActionRespond = "respond" // 直接把当前包作为结果返回
	ActionError   = "error"   // 转成错误包返回
)

// Handler 一个可调度的处理器。处理器通过 Serve 主动决定是否继续流程。
type Handler struct {
	Name     string
	Fn       HandlerFunc
	Discover DiscoverFunc
}

// Serve continues the current route by default. A source argument selects an
// explicit branch, such as the stream handler.
type Serve func(packet.Packet, ...string) packet.Packet
type HandlerFunc func(packet.Packet, Serve) packet.Packet
type ProviderModel struct {
	ID         string
	OwnedBy    string
	Created    int64
	SpeedTiers []string
}
type DiscoverFunc func(context.Context) ([]ProviderModel, error)

// Registry handler 注册表：name → handler
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

func (r *Registry) Register(name string, fn any) {
	r.RegisterProvider(name, fn, nil)
}

func (r *Registry) RegisterProvider(name string, fn any, discover DiscoverFunc) {
	var handler HandlerFunc
	switch f := fn.(type) {
	case HandlerFunc:
		handler = f
	case func(packet.Packet, Serve) packet.Packet:
		handler = f
	case func(packet.Packet, ...Serve) packet.Packet:
		handler = func(pkt packet.Packet, serve Serve) packet.Packet {
			return f(pkt, serve)
		}
	case func(packet.Packet) packet.Packet:
		// Keep the old registration shape source-compatible while handlers are
		// migrated. The compatibility wrapper preserves automatic forwarding.
		handler = func(pkt packet.Packet, serve Serve) packet.Packet {
			return serve(f(pkt))
		}
	default:
		panic(fmt.Sprintf("unsupported handler type %T", fn))
	}
	r.handlers[name] = Handler{Name: name, Fn: handler, Discover: discover}
}

func (r *Registry) Get(name string) (Handler, bool) {
	h, ok := r.handlers[name]
	return h, ok
}

// Has 是否已注册（用于 config 校验 action 引用）
func (r *Registry) Has(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Dispatcher 递归核心，无状态：按调用帧中的 source 匹配规则并调用 handler。
// 图的 override/分层/接线都在 ConfigManager 编译期完成，dispatcher 只消费最终规则表。
//
//	Serve(pkt, "http")
//	  → match(http) → openai.Handle(pkt, serve)
//	  → handler calls serve(pkt) for the rule's next source
type Dispatcher struct {
	table    *table.Table
	reg      *Registry
	MaxDepth int
}

func New(t *table.Table, reg *Registry) *Dispatcher {
	return &Dispatcher{table: t, reg: reg, MaxDepth: 100}
}

// Serve 入口
func (d *Dispatcher) Serve(pkt packet.Packet, source string) packet.Packet {
	return d.serve(pkt, source, 0)
}

func (d *Dispatcher) serve(pkt packet.Packet, source string, depth int) packet.Packet {
	if depth > d.MaxDepth {
		return pkt.Fail(packet.ErrInternal,
			fmt.Sprintf("dispatch depth exceeded %d, possible rule loop", d.MaxDepth))
	}

	r := d.table.Match(source, pkt)
	if r == nil {
		if pkt.Phase() == packet.PhaseReq {
			return pkt.Fail(packet.ErrNoRoute,
				fmt.Sprintf("no route: phase=%s source=%s", pkt.Phase(), source))
		}
		return pkt.Fail(packet.ErrChainNotClosed,
			fmt.Sprintf("response chain not closed: phase=%s source=%s (missing terminal fallback rule)", pkt.Phase(), source))
	}

	// default 原语：仅当字段缺失/为空时写入，客户端显式值优先
	for k, v := range r.Default {
		if err := pkt.SetDefaultPath(k, v); err != nil {
			return pkt.Fail(packet.ErrInternal, fmt.Sprintf("rule default failed path=%s: %v", k, err))
		}
	}
	// set 原语：action 前无条件改写包字段（纯变换规则 = 改写后回走）
	for k, v := range r.Set {
		if err := pkt.SetPath(k, v); err != nil {
			return pkt.Fail(packet.ErrInternal, fmt.Sprintf("rule set failed path=%s: %v", k, err))
		}
	}

	switch r.Action {
	case ActionRespond:
		// req 相位落到 respond = 无路由（没有任何 provider 产生 resp）；resp 相位 = 正常终态
		if pkt.Phase() == packet.PhaseReq {
			return pkt.Fail(packet.ErrNoRoute,
				fmt.Sprintf("no route: phase=%s source=%s", pkt.Phase(), source))
		}
		return pkt
	case ActionError:
		return pkt.Fail(packet.ErrInternal, "routed to error terminal")
	}

	next := r.To
	if next == "" {
		next = r.Action
	}

	// 纯变换规则（无 Action，只有 Set+To）：改写后直接回走。
	if r.Action == "" {
		return d.serve(pkt, next, depth+1)
	}
	h, ok := d.reg.Get(r.Action)
	if !ok {
		return pkt.Fail(packet.ErrInternal, fmt.Sprintf("handler not registered: %s", r.Action))
	}

	continueWith := func(out packet.Packet, sources ...string) packet.Packet {
		source := next
		if len(sources) > 0 && sources[0] != "" {
			source = sources[0]
		}
		return d.serve(out, source, depth+1)
	}
	return h.Fn(pkt, continueWith)
}
