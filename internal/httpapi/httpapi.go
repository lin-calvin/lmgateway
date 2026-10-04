package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/config"
	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

type Gateway interface {
	Dispatcher() *dispatch.Dispatcher
	ListModels(ctx context.Context, provider string) ([]config.ModelEntry, error)
}

func FromDispatcher(d *dispatch.Dispatcher) Gateway {
	return dispatcherAdapter{d}
}

type dispatcherAdapter struct{ d *dispatch.Dispatcher }

type httpReply struct {
	w         http.ResponseWriter
	committed bool
}

func (r *httpReply) Committed() bool { return r.committed }

func (r *httpReply) WriteStream(pkt packet.Packet) packet.Packet {
	response, ok := pkt.Response()
	if !ok {
		return pkt.Fail(packet.ErrInternal, "missing response document")
	}
	value, ok := response.Get("event")
	if !ok {
		return pkt.Fail(packet.ErrInternal, "response has no event stream")
	}
	events, ok := value.(<-chan lm.LMEvent)
	if !ok {
		return pkt.Fail(packet.ErrInternal, "response event stream has invalid type")
	}
	request, _ := pkt.Request()
	clientType, _ := lm.TypeOf(request)
	isResponses := clientType == "openai_response"
	writeSSEHeader(r.w)
	r.committed = true
	for event := range events {
		if _, ok := pkt[packet.KeyFirst]; !ok && hasOutput(event) {
			pkt.Set(packet.KeyFirst, time.Now())
		}
		if isResponses {
			writeLMEvent(r.w, event)
			continue
		}
		data, err := lm.EventData(event)
		if err != nil {
			writeSSE(r.w, map[string]any{"error": map[string]any{"message": err.Error()}})
			continue
		}
		writeSSEData(r.w, data)
	}
	pkt.Set(packet.KeyEnd, time.Now())
	if !isResponses {
		writeSSEDone(r.w)
	}
	return pkt
}

func hasOutput(event lm.LMEvent) bool {
	raw, ok := lm.MapEvent(event)
	if !ok {
		return false
	}
	if name := lm.EventName(event); name != "" {
		switch name {
		case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
			return eventString(raw["delta"]) != ""
		}
	}
	choices, _ := raw["choices"].([]any)
	for _, value := range choices {
		choice, _ := value.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if eventString(delta["content"]) != "" || eventString(delta["reasoning_content"]) != "" {
			return true
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, value := range calls {
			call, _ := value.(map[string]any)
			function, _ := call["function"].(map[string]any)
			if eventString(function["name"]) != "" || eventString(function["arguments"]) != "" {
				return true
			}
		}
	}
	return false
}

func eventString(value any) string {
	text, _ := value.(string)
	return text
}

func (a dispatcherAdapter) Dispatcher() *dispatch.Dispatcher { return a.d }
func (a dispatcherAdapter) ListModels(context.Context, string) ([]config.ModelEntry, error) {
	return nil, nil
}

func WithCORS(next http.Handler) http.Handler {
	origins := corsOrigins()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && (origins == nil || origins[origin])
		if allowed {
			if origins == nil {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Session-ID, Session-ID, Originator, Accept-Language, X-Request-ID")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Type, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsOrigins() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("LMGATEWAY_CORS_ORIGINS"))
	if raw == "" || raw == "*" {
		return nil
	}
	origins := map[string]bool{}
	for _, origin := range strings.Split(raw, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins[origin] = true
		}
	}
	return origins
}

func New(m Gateway) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("provider")
		entries, err := m.ListModels(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		data := []map[string]any{}
		for _, e := range entries {
			model := map[string]any{
				"id":       e.ID,
				"object":   "model",
				"created":  e.Created,
				"owned_by": e.OwnedBy,
			}
			if len(e.SpeedTiers) > 0 {
				model["speed_tiers"] = e.SpeedTiers
			}
			data = append(data, model)
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})

	handleRequest := func(protocol string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "request body is not valid JSON")
				return
			}

			documentType := "openai"
			if protocol == "responses" {
				documentType = "openai_response"
			}
			doc, err := lm.NewRequest(documentType, body)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			pkt := packet.NewReqDocument(doc)
			pkt.Set(packet.KeyCtx, r.Context())
			pkt.Set(packet.KeyStart, time.Now())
			pkt.Set(packet.KeyHTTPMeta, collectHTTPMetadata(r))
			reply := &httpReply{w: w}
			pkt.Set(packet.KeyReply, reply)

			d := m.Dispatcher()
			result := d.Serve(pkt, packet.SourceIngress)
			if reply.Committed() {
				return
			}
			writeResult(w, result)
		}
	}
	mux.HandleFunc("POST /v1/chat/completions", handleRequest("chat"))
	mux.HandleFunc("POST /v1/responses", handleRequest("responses"))

	return mux
}

func collectHTTPMetadata(r *http.Request) map[string]any {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("session-id")
	}
	return map[string]any{
		"user_agent":             r.UserAgent(),
		"origin":                 r.Header.Get("Origin"),
		"referer":                r.Referer(),
		"accept_language":        r.Header.Get("Accept-Language"),
		"request_id":             r.Header.Get("X-Request-ID"),
		"session_id":             sessionID,
		"claude_code_session_id": r.Header.Get("X-Claude-Code-Session-Id"),
		"originator":             r.Header.Get("Originator"),
		"method":                 r.Method,
		"path":                   r.URL.Path,
		"remote_addr":            r.RemoteAddr,
	}
}

func writeResult(w http.ResponseWriter, pkt packet.Packet) {
	if msg, ok := pkt.Error(); ok {
		if class, classOK := pkt.ErrorClass(); classOK && class == packet.ClassRateLimit {
			if retry, retryOK := pkt.ErrorRetryAfter(); retryOK && retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(retry))
			}
		}
		writeError(w, statusFor(pkt), msg)
		return
	}
	if out, ok := pkt.Map("resp"); ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	writeError(w, http.StatusInternalServerError, "dispatcher produced no response")
}

func statusFor(pkt packet.Packet) int {
	if class, ok := pkt.ErrorClass(); ok && class == packet.ClassRateLimit {
		return http.StatusTooManyRequests
	}
	switch kind, _ := pkt.ErrorKind(); kind {
	case packet.ErrNoRoute:
		return http.StatusNotFound
	case packet.ErrUpstream:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func writeSSEHeader(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
}

func writeLMEvent(w http.ResponseWriter, event lm.LMEvent) {
	data, err := lm.EventData(event)
	if err != nil {
		return
	}
	if name := lm.EventName(event); name != "" {
		fmt.Fprintf(w, "event: %s\n", name)
	}
	writeSSEData(w, data)
}

func writeSSEData(w http.ResponseWriter, data []byte) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSE(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("{}")
	}
	writeSSEData(w, b)
}

func writeSSEDone(w http.ResponseWriter) {
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "gateway_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
