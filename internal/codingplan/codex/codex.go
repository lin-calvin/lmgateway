package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
	"lmgateway/internal/sse"
)

const (
	defaultCodexResponsesBase = "https://chatgpt.com/backend-api/codex"
	codexClientVersion        = "0.0.0"
)

type ChatGPTCodexConfig struct {
	Name                  string
	BaseURL               string
	Originator            string
	ForwardClientMetadata bool
	TimeoutSec            int
	Auth                  *codexauth.Service
	Upstream              map[string]string
	ExtraBody             map[string]map[string]any
	DefaultExtraBody      map[string]map[string]any
}

func (o *ChatGPTCodex) Discover(ctx context.Context) ([]dispatch.ProviderModel, error) {
	if o.cfg.Auth == nil {
		return nil, fmt.Errorf("codex auth service is not configured")
	}
	token, err := o.cfg.Auth.GetAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(o.endpoint, "/responses") + "/models"
	query := url.Values{}
	query.Set("client_version", codexClientVersion)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("originator", o.cfg.Originator)
	if token.AccountID != "" {
		request.Header.Set("ChatGPT-Account-Id", token.AccountID)
	}
	if token.Residency != "" {
		request.Header.Set("x-openai-internal-codex-residency", token.Residency)
	}
	response, err := o.streamClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("codex models returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var body struct {
		Models []struct {
			ID                   string   `json:"id"`
			Slug                 string   `json:"slug"`
			OwnedBy              string   `json:"owned_by"`
			Created              int64    `json:"created"`
			AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
		} `json:"models"`
		Data []struct {
			ID                   string   `json:"id"`
			Slug                 string   `json:"slug"`
			OwnedBy              string   `json:"owned_by"`
			Created              int64    `json:"created"`
			AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode codex models failed: %w", err)
	}
	models := body.Models
	if len(models) == 0 {
		models = body.Data
	}
	out := make([]dispatch.ProviderModel, 0, len(models))
	for _, model := range models {
		id := model.ID
		if id == "" {
			id = model.Slug
		}
		if id == "" {
			continue
		}
		out = append(out, dispatch.ProviderModel{ID: id, OwnedBy: model.OwnedBy, Created: model.Created, SpeedTiers: model.AdditionalSpeedTiers})
	}
	return out, nil
}

type ChatGPTCodex struct {
	cfg          ChatGPTCodexConfig
	client       *http.Client
	streamClient *http.Client
	endpoint     string
}

func NewChatGPTCodex(cfg ChatGPTCodexConfig) *ChatGPTCodex {
	base := cfg.BaseURL
	if base == "" {
		base = defaultCodexResponsesBase
	}
	base = strings.TrimSuffix(base, "/")
	if !strings.HasSuffix(base, "/responses") {
		base += "/responses"
	}
	if cfg.Originator == "" {
		cfg.Originator = "lmgateway"
	}
	timeout := cfg.TimeoutSec
	if timeout <= 0 {
		timeout = 120
	}
	return &ChatGPTCodex{
		cfg:          cfg,
		client:       &http.Client{Timeout: time.Duration(timeout) * time.Second},
		streamClient: &http.Client{},
		endpoint:     base,
	}
}

func (o *ChatGPTCodex) Handle(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
	serve := dispatch.Serve(func(out packet.Packet, sources ...string) packet.Packet {
		if len(serves) == 0 {
			return out
		}
		return serves[0](out, sources...)
	})
	clientReq, ok := pkt.Request()
	if !ok {
		return serve(pkt.WithError("missing request document"))
	}
	if !lm.IsRequest(clientReq) {
		return serve(pkt.WithError("request document is not a request"))
	}
	if o.cfg.Auth == nil {
		return serve(pkt.WithError("codex auth service is not configured"))
	}
	clientType, ok := lm.TypeOf(clientReq)
	if !ok || (clientType != "openai" && clientType != "openai_response") {
		return serve(pkt.WithError("unsupported Codex client document type"))
	}

	pkt.Set("provider", o.cfg.Name)
	modelValue, _ := clientReq.Get("model")
	model, _ := modelValue.(string)
	clientStreaming, _ := clientReq.Get("stream")
	upstreamModel := model
	fast := false
	if um := o.cfg.Upstream[model]; um != "" {
		upstreamModel = um
	} else if strings.HasPrefix(model, o.cfg.Name+"/") {
		upstreamModel = strings.TrimPrefix(model, o.cfg.Name+"/")
	}
	if strings.HasSuffix(upstreamModel, "-fast") {
		upstreamModel = strings.TrimSuffix(upstreamModel, "-fast")
		fast = true
	}
	upstreamReq, err := lm.NewRequest("openai_response", map[string]any{})
	if err != nil {
		return serve(pkt.WithError(err.Error()))
	}
	upstreamReq, err = upstreamReq.ConvertFrom(clientReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, "codex request conversion failed: "+err.Error()))
	}
	upstreamReq, err = lm.Clone(upstreamReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	// stream_options is a Chat Completions-only option. Codex accepts the
	// Responses stream protocol but rejects this field, including include_usage.
	if err := upstreamReq.Delete("stream_options"); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if err := upstreamReq.Set("model", upstreamModel); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if err := upstreamReq.Set("stream", true); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if err := upstreamReq.Set("store", false); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if fast {
		if err := upstreamReq.Set("service_tier", "priority"); err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
	}
	if err := upstreamReq.Set("raw.include", []string{"reasoning.encrypted_content"}); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if clientType == "openai" {
		if err := upstreamReq.Delete("max_output_tokens"); err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
	}
	for key, value := range o.cfg.DefaultExtraBody[model] {
		if err := upstreamReq.SetDefault("raw."+key, value); err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
	}
	for key, value := range o.cfg.ExtraBody[model] {
		if err := upstreamReq.Set("raw."+key, value); err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
	}
	body, ok := lm.Map(upstreamReq)
	if !ok {
		return serve(pkt.WithError("cannot read Codex request document"))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return serve(pkt.WithError("marshal codex responses request failed: " + err.Error()))
	}

	ctx, _ := pkt[packet.KeyCtx].(context.Context)
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx := ctx
	if clientStreaming != true {
		timeout := o.cfg.TimeoutSec
		if timeout <= 0 {
			timeout = 120
		}
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	token, err := o.cfg.Auth.GetAccessToken(requestCtx)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
	}
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, o.endpoint, bytes.NewReader(payload))
	if err != nil {
		return serve(pkt.WithError("build codex request failed: " + err.Error()))
	}
	httpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("originator", o.cfg.Originator)
	if token.AccountID != "" {
		httpReq.Header.Set("ChatGPT-Account-Id", token.AccountID)
	}
	if token.Residency != "" {
		httpReq.Header.Set("x-openai-internal-codex-residency", token.Residency)
	}
	if o.cfg.ForwardClientMetadata {
		applyCodexClientMetadata(httpReq, pkt)
	}

	resp, err := o.streamClient.Do(httpReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, "codex call failed: "+err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return serve(pkt.Fail(packet.ErrUpstream, fmt.Sprintf("codex returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))))
	}

	if clientStreaming == true {
		sourceEvents := sse.ResponsesEvents(ctx, resp.Body, model)
		sourceResponse, err := lm.NewResponseStream("openai_response", sourceEvents, ctx)
		if err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
		clientResponse, err := lm.NewResponseTarget(clientType, ctx)
		if err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
		convertedResponse, err := clientResponse.ConvertFrom(sourceResponse)
		if err != nil {
			return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
		}
		response, ok := convertedResponse.(lm.LMResponse)
		if !ok {
			return serve(pkt.Fail(packet.ErrInternal, "stream conversion did not return LMResponse"))
		}
		pkt.Set(packet.KeyResp, response)
		pkt.SetPhase(packet.PhaseStream)
		return serve(pkt, "stream")
	}

	sourceEvents := sse.ResponsesEvents(ctx, resp.Body, model)
	sourceResponse, err := lm.NewResponseStream("openai_response", sourceEvents, ctx)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	clientResponse, err := lm.NewResponseTarget(clientType, ctx)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	convertedResponse, err := clientResponse.ConvertFrom(sourceResponse)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
	}
	response, ok := convertedResponse.(lm.LMResponse)
	if !ok {
		return serve(pkt.Fail(packet.ErrInternal, "response conversion did not return LMResponse"))
	}
	value, ok := response.Get("event")
	if !ok {
		return serve(pkt.Fail(packet.ErrInternal, "response conversion did not return event stream"))
	}
	events, ok := value.(<-chan lm.LMEvent)
	if !ok {
		return serve(pkt.Fail(packet.ErrInternal, "response event stream has invalid type"))
	}
	for range events {
	}
	pkt.Set(packet.KeyResp, response)
	pkt.SetPhase(packet.PhaseResp)
	return serve(pkt)
}

func (o *ChatGPTCodex) handleNonStreamingResponses(pkt packet.Packet, body io.ReadCloser, model string) packet.Packet {
	defer body.Close()
	stream := sse.NewNativeResponses(body)
	stream.SetResponseModel(model)
	defer stream.Close()
	for {
		event, err := stream.NextEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return pkt.Fail(packet.ErrUpstream, "parse codex responses stream failed: "+err.Error())
		}
		if event.Name == "response.failed" || event.Name == "error" {
			return pkt.Fail(packet.ErrUpstream, "codex responses request failed: "+string(event.Data))
		}
	}
	responseBody := stream.Response
	if responseBody == nil {
		responseBody = map[string]any{
			"id":     "resp_lmgateway",
			"object": "response",
			"model":  model,
			"status": "completed",
			"output": []any{},
		}
	}
	responseBody["object"] = "response"
	if model != "" {
		responseBody["model"] = model
	}
	response, err := lm.NewResponse("openai_response", responseBody)
	if err != nil {
		return pkt.Fail(packet.ErrInternal, err.Error())
	}
	pkt.Set(packet.KeyResp, response)
	pkt.SetPhase(packet.PhaseResp)
	return pkt
}

func assembleChatCompletion(chunks []map[string]any, model string, usage map[string]any) map[string]any {
	var content, reasoning string
	finish := "stop"
	id := ""
	toolCallsByIndex := map[int]map[string]any{}
	var toolCallOrder []int
	for _, chunk := range chunks {
		if v, ok := chunk["id"].(string); ok && v != "" {
			id = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			if f, ok := choice["finish_reason"].(string); ok && f != "" {
				finish = f
			}
			delta, _ := choice["delta"].(map[string]any)
			if c, ok := delta["content"].(string); ok {
				content += c
			}
			if c, ok := delta["reasoning_content"].(string); ok {
				reasoning += c
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					idx := len(toolCallOrder)
					switch n := call["index"].(type) {
					case int:
						idx = n
					case float64:
						idx = int(n)
					}
					assembled, exists := toolCallsByIndex[idx]
					if !exists {
						assembled = map[string]any{"index": idx, "type": "function", "function": map[string]any{"name": "", "arguments": ""}}
						toolCallsByIndex[idx] = assembled
						toolCallOrder = append(toolCallOrder, idx)
					}
					if id, ok := call["id"].(string); ok && id != "" {
						assembled["id"] = id
					}
					fn, _ := call["function"].(map[string]any)
					assembledFn, _ := assembled["function"].(map[string]any)
					if name, ok := fn["name"].(string); ok && name != "" {
						assembledFn["name"] = name
					}
					if args, ok := fn["arguments"].(string); ok {
						previous, _ := assembledFn["arguments"].(string)
						assembledFn["arguments"] = previous + args
					}
				}
			}
		}
		if usage == nil {
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCallsByIndex) > 0 {
		sort.Ints(toolCallOrder)
		toolCalls := make([]any, 0, len(toolCallOrder))
		for _, idx := range toolCallOrder {
			toolCalls = append(toolCalls, toolCallsByIndex[idx])
		}
		message["tool_calls"] = toolCalls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

func applyCodexClientMetadata(req *http.Request, pkt packet.Packet) {
	meta, ok := pkt[packet.KeyHTTPMeta].(map[string]any)
	if !ok {
		return
	}
	if ua, _ := meta["user_agent"].(string); ua != "" {
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-LMGateway-Client-User-Agent", ua)
	}
	if language, _ := meta["accept_language"].(string); language != "" {
		req.Header.Set("Accept-Language", language)
	}
	if sessionID, _ := meta["session_id"].(string); sessionID != "" {
		req.Header.Set("session_id", sessionID)
		req.Header.Set("session-id", sessionID)
	}
}
