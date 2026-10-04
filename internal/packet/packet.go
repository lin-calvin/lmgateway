package packet

import (
	"fmt"
	"strings"

	"lmgateway/internal/lm"
)

type Packet map[string]any

const (
	KeyPhase     = "phase"
	KeySource    = "source"
	KeyReq       = "req"
	KeyResp      = "resp"
	KeyCtx       = "ctx"
	KeyProvider  = "provider"
	KeyStart     = "start"
	KeyFirst     = "first_output"
	KeyEnd       = "end"
	KeyHTTPMeta  = "http_meta"
	KeyReply     = "reply"
	KeyError     = "error"
	KeyErrorKind = "error_kind"
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
)

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

func (p Packet) Error() (string, bool) {
	s, ok := p[KeyError].(string)
	return s, ok
}

func (p Packet) ErrorKind() (string, bool) {
	s, ok := p[KeyErrorKind].(string)
	return s, ok
}

func (p Packet) WithError(msg string) Packet { return p.Fail(ErrInternal, msg) }
