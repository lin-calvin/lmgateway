package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
	"lmgateway/internal/sse"
)

type OpenAIConfig struct {
	Name               string
	BaseURL            string
	APIKey             string
	DocumentType       string
	TimeoutSec         int
	Upstream           map[string]string
	ExtraBody          map[string]map[string]any
	DefaultExtraBody   map[string]map[string]any
	DisableStreamUsage bool
}

func (o *OpenAI) Discover(ctx context.Context) ([]dispatch.ProviderModel, error) {
	endpoint := strings.TrimSuffix(o.cfg.BaseURL, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if o.cfg.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	response, err := o.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider %s: models returned %d", o.cfg.Name, response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID                   string   `json:"id"`
			Slug                 string   `json:"slug"`
			OwnedBy              string   `json:"owned_by"`
			Created              int64    `json:"created"`
			AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("provider %s: decode models failed: %w", o.cfg.Name, err)
	}
	models := make([]dispatch.ProviderModel, 0, len(body.Data))
	for _, model := range body.Data {
		id := model.ID
		if id == "" {
			id = model.Slug
		}
		if id == "" {
			continue
		}
		models = append(models, dispatch.ProviderModel{ID: id, OwnedBy: model.OwnedBy, Created: model.Created, SpeedTiers: model.AdditionalSpeedTiers})
	}
	return models, nil
}

type OpenAI struct {
	cfg               OpenAIConfig
	client            *http.Client
	streamClient      *http.Client
	endpoint          string
	responsesEndpoint string
}

func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.DocumentType == "" {
		cfg.DocumentType = "openai"
	}
	timeout := cfg.TimeoutSec
	if timeout <= 0 {
		timeout = 120
	}
	return &OpenAI{
		cfg:               cfg,
		client:            &http.Client{Timeout: time.Duration(timeout) * time.Second},
		streamClient:      &http.Client{},
		endpoint:          strings.TrimSuffix(cfg.BaseURL, "/") + "/chat/completions",
		responsesEndpoint: strings.TrimSuffix(cfg.BaseURL, "/") + "/responses",
	}
}

// retryAfterSeconds parses a Retry-After header expressed in seconds.
func retryAfterSeconds(resp *http.Response) int {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return seconds
}

func setStreamUsage(doc lm.LMDocument) error {
	options := map[string]any{}
	if current, ok := doc.Get("stream_options"); ok {
		if existing, ok := current.(map[string]any); ok {
			for key, value := range existing {
				options[key] = value
			}
		}
	}
	options["include_usage"] = true
	return doc.Set("stream_options", options)
}

func (o *OpenAI) Handle(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
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
	pkt.Set("provider", o.cfg.Name)

	clientType, ok := lm.TypeOf(clientReq)
	if !ok {
		return serve(pkt.WithError("unknown request document type"))
	}
	upstreamReq, err := lm.NewRequest(o.cfg.DocumentType, map[string]any{})
	if err != nil {
		return serve(pkt.WithError(err.Error()))
	}
	upstreamReq, err = upstreamReq.ConvertFrom(clientReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
	}
	upstreamReq, err = lm.Clone(upstreamReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	model, _ := clientReq.Get("model")
	modelName, _ := model.(string)
	upstreamModel := modelName
	if um := o.cfg.Upstream[modelName]; um != "" {
		upstreamModel = um
	} else if strings.HasPrefix(modelName, o.cfg.Name+"/") {
		upstreamModel = strings.TrimPrefix(modelName, o.cfg.Name+"/")
	}
	if err := upstreamReq.Set("model", upstreamModel); err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	if extra := o.cfg.DefaultExtraBody[modelName]; len(extra) > 0 {
		for key, value := range extra {
			if err := upstreamReq.SetDefault("raw."+key, value); err != nil {
				return serve(pkt.Fail(packet.ErrInternal, err.Error()))
			}
		}
	}
	if extra := o.cfg.ExtraBody[modelName]; len(extra) > 0 {
		for key, value := range extra {
			if err := upstreamReq.Set("raw."+key, value); err != nil {
				return serve(pkt.Fail(packet.ErrInternal, err.Error()))
			}
		}
	}
	// OpenAI-compatible streaming only reports usage when the request opts in
	// via stream_options.include_usage. Force it so the gateway can account for
	// providers (for example DeepSeek) that otherwise return no usage.
	if streaming, _ := upstreamReq.Get("stream"); streaming == true {
		if upstreamType, _ := lm.TypeOf(upstreamReq); upstreamType == "openai" && !o.cfg.DisableStreamUsage {
			if err := setStreamUsage(upstreamReq); err != nil {
				return serve(pkt.Fail(packet.ErrInternal, err.Error()))
			}
		}
	}
	body, ok := lm.Map(upstreamReq)
	if !ok {
		return serve(pkt.WithError("cannot read upstream request document"))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return serve(pkt.WithError("marshal request failed: " + err.Error()))
	}

	ctx, _ := pkt[packet.KeyCtx].(context.Context)
	if ctx == nil {
		ctx = context.Background()
	}
	upstreamType, _ := lm.TypeOf(upstreamReq)
	endpoint := o.endpoint
	if upstreamType == "openai_response" {
		endpoint = o.responsesEndpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return serve(pkt.WithError("build upstream request failed: " + err.Error()))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if streaming, _ := upstreamReq.Get("stream"); streaming == true {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	client := o.client
	streaming, _ := upstreamReq.Get("stream")
	if streaming == true {
		client = o.streamClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return serve(pkt.FailStatus(packet.ErrUpstream, 0, 0, packet.ClassNetwork, "upstream call failed: "+err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, rerr := io.ReadAll(resp.Body)
		detail := ""
		if rerr == nil {
			detail = strings.TrimSpace(string(respBody))
		}
		return serve(pkt.FailStatus(packet.ErrUpstream, resp.StatusCode, retryAfterSeconds(resp), "", fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, detail)))
	}
	if streaming == true {
		var sourceEvents <-chan lm.LMEvent
		if upstreamType == "openai_response" {
			sourceEvents = sse.ResponsesEvents(ctx, resp.Body, modelName)
		} else {
			sourceEvents = sse.ChatEvents(ctx, resp.Body, modelName)
		}
		sourceResponse, err := lm.NewResponseStream(upstreamType, sourceEvents, ctx)
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
	defer resp.Body.Close()
	var upstreamBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&upstreamBody); err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, "parse upstream response failed: "+err.Error()))
	}
	upstreamResponse, err := lm.NewResponse(upstreamType, upstreamBody)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	clientResponse, err := lm.NewResponse(clientType, map[string]any{})
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	clientResponse, err = clientResponse.ConvertFrom(upstreamResponse)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
	}
	if modelName != "" {
		if err := clientResponse.Set("model", modelName); err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
	}
	pkt.Set(packet.KeyResp, clientResponse)
	pkt.SetPhase(packet.PhaseResp)
	return serve(pkt)
}
