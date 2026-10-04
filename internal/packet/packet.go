package packet

import (
	"fmt"
	"strings"

	"lmgateway/internal/lm"
)

type Packet map[string]any

const (
	KeyPhase    = "phase"
	KeySource   = "source"
	KeyReq      = "req"
	KeyResp     = "resp"
	KeyCtx      = "ctx"
	KeyProvider = "provider"
	KeyStart    = "start"
	KeyFirst    = "first_output"
	KeyEnd      = "end"
	KeyHTTPMeta = "http_meta"
	KeyReply    = "reply"
	// KeyIdentity 承载本请求的鉴权身份(仅进程内使用,不落库、不转发上游)。
	KeyIdentity        = "identity"
	KeyError           = "error"
	KeyErrorKind       = "error_kind"
	KeyErrorClass      = "error_class"
	KeyErrorStatus     = "error_status"
	KeyErrorRetryAfter = "error_retry_after"
)

// Reply is the transport output adapter used by the stream handler. The
// packet carries the adapter as a dependency, not routing state.
type Reply interface {
	WriteStream(Packet) Packet
	Committed() bool
}

const (
	ErrNoRoute        = "no_route"
	ErrChainNotClosed = "chain_not_closed"
	ErrUpstream       = "upstream"
	ErrInternal       = "internal"
	// ErrPolicy 网关自身的策略拒绝(模型白名单、配额等),与上游错误区分开,
	// 以便把确切的 HTTP 状态码透传给客户端。
	ErrPolicy = "policy"
)

// Upstream error classes: a coarse, provider-independent taxonomy used by the
// pool/ratelimit control plane. Providers set these from the upstream status.
const (
	ClassRateLimit  = "rate_limit"
	ClassOverloaded = "overloaded"
	ClassNetwork    = "network"
	ClassUpstream   = "upstream"
	ClassAuth       = "auth"
	ClassInvalid    = "invalid"
)

// ClassifyStatus maps an upstream HTTP status to a coarse error class.
func ClassifyStatus(status int) string {
	switch {
	case status == 429 || status == 402:
		return ClassRateLimit
	case status == 503:
		return ClassOverloaded
	case status == 401 || status == 403:
		return ClassAuth
	case status == 400 || status == 422:
		return ClassInvalid
	default:
		return ClassUpstream
	}
}

const (
	PhaseReq    = "req"
	PhaseStream = "stream"
	PhaseResp   = "resp"
)

const (
	SourceHTTP     = "http"
	SourceResponse = "response"
	SourceIngress  = "@ingress"
)

func New(phase string) Packet { return Packet{KeyPhase: phase} }

func NewReq(payload map[string]any) Packet {
	return NewReqDocument(lm.OpenAIChatRequest(payload))
}

func NewReqDocument(doc lm.LMDocument) Packet {
	return Packet{KeyPhase: PhaseReq, KeyReq: doc}
}

func (p Packet) Get(k string) (any, bool) {
	v, ok := p[k]
	return v, ok
}

func (p Packet) Map(k string) (map[string]any, bool) {
	if raw, ok := p[k].(map[string]any); ok {
		return raw, true
	}
	if doc, ok := p[k].(lm.LMDocument); ok {
		return lm.Map(doc)
	}
	return nil, false
}

func (p Packet) Str(k string) (string, bool) {
	v, ok := p[k].(string)
	return v, ok
}

func (p Packet) Set(k string, v any) {
	if k == KeyReq {
		if doc, ok := v.(lm.LMDocument); ok {
			p[k] = doc
			return
		}
		if raw, ok := v.(map[string]any); ok {
			p[k] = lm.OpenAIChatRequest(raw)
			return
		}
	}
	if k == KeyResp {
		if doc, ok := v.(lm.LMDocument); ok {
			p[k] = doc
			return
		}
		if raw, ok := v.(map[string]any); ok {
			p[k] = lm.OpenAIChatResponse(raw)
			return
		}
	}
	p[k] = v
}

func (p Packet) Request() (lm.LMDocument, bool) {
	if doc, ok := p[KeyReq].(lm.LMDocument); ok {
		return doc, true
	}
	if raw, ok := p[KeyReq].(map[string]any); ok {
		return lm.OpenAIChatRequest(raw), true
	}
	return nil, false
}

func (p Packet) Response() (lm.LMDocument, bool) {
	if doc, ok := p[KeyResp].(lm.LMDocument); ok {
		return doc, true
	}
	if raw, ok := p[KeyResp].(map[string]any); ok {
		return lm.OpenAIChatResponse(raw), true
	}
	return nil, false
}

func (p Packet) ActiveDocument() (lm.LMDocument, bool) {
	if p.Phase() == PhaseStream || p.Phase() == PhaseResp {
		return p.Response()
	}
	return p.Request()
}

func (p Packet) Resolve(path string) (any, bool) {
	if path == "stream" {
		if doc, ok := p.Request(); ok {
			return doc.Get("stream")
		}
		return nil, false
	}
	if path == "type" {
		if doc, ok := p.ActiveDocument(); ok {
			return lm.TypeOf(doc)
		}
		return nil, false
	}
	if strings.HasPrefix(path, "req.") {
		if doc, ok := p.Request(); ok {
			return doc.Get(strings.TrimPrefix(path, "req."))
		}
		return nil, false
	}
	if strings.HasPrefix(path, "resp.") {
		if doc, ok := p.Response(); ok {
			return doc.Get(strings.TrimPrefix(path, "resp."))
		}
		return nil, false
	}
	if doc, ok := p.ActiveDocument(); ok {
		if value, found := doc.Get(path); found {
			return value, true
		}
	}
	parts := strings.Split(path, ".")
	var current any = map[string]any(p)
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func (p Packet) SetPath(path string, value any) error {
	if strings.HasPrefix(path, "req.") || strings.HasPrefix(path, "resp.") {
		return fmt.Errorf("rule set path %q must not use req./resp. prefix", path)
	}
	doc, ok := p.ActiveDocument()
	if !ok {
		return fmt.Errorf("no active LM document for rule set %q", path)
	}
	return doc.Set(path, value)
}

// SetDefaultPath 按点分路径写入软默认值：仅当字段缺失/为空时生效。
func (p Packet) SetDefaultPath(path string, value any) error {
	if strings.HasPrefix(path, "req.") || strings.HasPrefix(path, "resp.") {
		return fmt.Errorf("rule default path %q must not use req./resp. prefix", path)
	}
	doc, ok := p.ActiveDocument()
	if !ok {
		return fmt.Errorf("no active LM document for rule default %q", path)
	}
	return doc.SetDefault(path, value)
}

func (p Packet) Phase() string {
	s, _ := p.Str(KeyPhase)
	return s
}

func (p Packet) SetPhase(phase string) { p[KeyPhase] = phase }

func (p Packet) Source() string {
	s, _ := p.Str(KeySource)
	return s
}

func (p Packet) Fail(kind, msg string) Packet {
	p[KeyError] = msg
	p[KeyErrorKind] = kind
	p[KeyPhase] = PhaseResp
	return p
}

// FailStatus records an upstream failure together with its HTTP status, retry
// hint and coarse class. status/retryAfter of 0 mean "unknown"; class is
// defaulted from status when empty.
func (p Packet) FailStatus(kind string, status, retryAfter int, class, msg string) Packet {
	p[KeyError] = msg
	p[KeyErrorKind] = kind
	if status != 0 {
		p[KeyErrorStatus] = status
	}
	if retryAfter != 0 {
		p[KeyErrorRetryAfter] = retryAfter
	}
	if class == "" {
		class = ClassifyStatus(status)
	}
	p[KeyErrorClass] = class
	p[KeyPhase] = PhaseResp
	return p
}

func (p Packet) Error() (string, bool) {
	s, ok := p[KeyError].(string)
	return s, ok
}

func (p Packet) ErrorKind() (string, bool) {
	s, ok := p[KeyErrorKind].(string)
	return s, ok
}

func (p Packet) ErrorClass() (string, bool) {
	s, ok := p[KeyErrorClass].(string)
	return s, ok
}

func (p Packet) ErrorStatus() (int, bool) {
	switch v := p[KeyErrorStatus].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func (p Packet) ErrorRetryAfter() (int, bool) {
	switch v := p[KeyErrorRetryAfter].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func (p Packet) WithError(msg string) Packet { return p.Fail(ErrInternal, msg) }
