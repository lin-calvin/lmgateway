// Package commandcode implements the Command Code (commandcode.ai) coding-plan
// provider. The upstream speaks a proprietary envelope (/alpha/generate) plus
// device fingerprint / lifecycle pre-requests. Downstream is standard lm
// documents: this handler normalizes any client request to Chat, builds the CC
// envelope, and projects the CC event stream back through lm so clients can
// speak Chat Completions or Responses.
package commandcode

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

const (
	defaultBaseURL       = "https://api.commandcode.ai"
	defaultProjectSlug   = "cc-proxy"
	defaultModel         = "deepseek/deepseek-v4-flash"
	ccProtocolVersion    = "1.53.1"
	fpSalt               = "command-code:device-fingerprint:v1"
	initRefresh          = 8 * time.Hour
	initJitter           = 2 * time.Hour
	sessionDuration      = 12 * time.Hour
	sessionJitter        = time.Hour
	modelCacheTTL        = 5 * time.Minute
	defaultStreamIdle    = 30 * time.Second
	defaultNonStreamIdle = 90 * time.Second
	defaultRetryMax      = 2
	defaultRetryBaseMS   = 400
)

// Config controls a Command Code provider instance.
type Config struct {
	Name                   string
	BaseURL                string
	APIKey                 string
	ProjectSlug            string
	DeviceProjectDir       string
	FingerprintSalt        string
	CLIMode                string
	CLISessionMode         string
	ZDR                    bool
	EmptySystemPlaceholder bool
	TimeoutSec             int
	StreamIdleMS           int
	NonStreamIdleMS        int
	RetryMax               int
	RetryBaseMS            int
	Upstream               map[string]string
	ExtraBody              map[string]map[string]any
	DefaultExtraBody       map[string]map[string]any
}

// CommandCode is a Command Code provider instance.
type CommandCode struct {
	cfg          Config
	client       *http.Client
	streamClient *http.Client

	mu       sync.Mutex
	keys     map[string]*keyState
	sessions map[string]*sessionState
	models   []dispatch.ProviderModel
	modelsAt time.Time
}

type keyState struct {
	fingerprint map[string]any
	nextInitAt  time.Time
}

type sessionState struct {
	id        string
	expiresAt time.Time
}

func New(cfg Config) *CommandCode {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.ProjectSlug == "" {
		cfg.ProjectSlug = defaultProjectSlug
	}
	if cfg.CLIMode == "" {
		cfg.CLIMode = "agent"
	}
	if cfg.CLISessionMode == "" {
		cfg.CLISessionMode = "interactive"
	}
	timeout := cfg.TimeoutSec
	if timeout <= 0 {
		timeout = 600
	}
	if cfg.StreamIdleMS <= 0 {
		cfg.StreamIdleMS = int(defaultStreamIdle / time.Millisecond)
	}
	if cfg.NonStreamIdleMS <= 0 {
		cfg.NonStreamIdleMS = int(defaultNonStreamIdle / time.Millisecond)
	}
	if cfg.RetryBaseMS <= 0 {
		cfg.RetryBaseMS = defaultRetryBaseMS
	}
	return &CommandCode{
		cfg:          cfg,
		client:       &http.Client{Timeout: time.Duration(timeout) * time.Second},
		streamClient: &http.Client{},
		keys:         map[string]*keyState{},
		sessions:     map[string]*sessionState{},
	}
}

func (o *CommandCode) projectDir() string {
	if o.cfg.DeviceProjectDir != "" {
		return o.cfg.DeviceProjectDir
	}
	return `C:\Users\dev\projects\app`
}

// Handle implements dispatch.HandlerFunc.
func (o *CommandCode) Handle(pkt packet.Packet, serves ...dispatch.Serve) packet.Packet {
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
	clientType, ok := lm.TypeOf(clientReq)
	if !ok || (clientType != "openai" && clientType != "openai_response") {
		return serve(pkt.WithError("unsupported commandcode client document type"))
	}

	pkt.Set("provider", o.cfg.Name)

	chatReq, err := lm.NewRequest("openai", map[string]any{})
	if err != nil {
		return serve(pkt.WithError(err.Error()))
	}
	chatReq, err = chatReq.ConvertFrom(clientReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, "commandcode request conversion failed: "+err.Error()))
	}
	chatReq, err = lm.Clone(chatReq)
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	chatRaw, ok := lm.Map(chatReq)
	if !ok {
		return serve(pkt.WithError("cannot read commandcode chat request"))
	}

	modelName, _ := chatRaw["model"].(string)
	upstreamModel := modelName
	if um := o.cfg.Upstream[modelName]; um != "" {
		upstreamModel = um
	} else if strings.HasPrefix(modelName, o.cfg.Name+"/") {
		upstreamModel = strings.TrimPrefix(modelName, o.cfg.Name+"/")
	}
	if upstreamModel == "" {
		upstreamModel = defaultModel
	}
	applyExtraBody(chatRaw, o.cfg.DefaultExtraBody[modelName], o.cfg.ExtraBody[modelName])

	streaming, _ := chatRaw["stream"].(bool)
	promptCacheKey, _ := chatRaw["prompt_cache_key"].(string)
	body := buildCCRequest(chatRaw, upstreamModel, promptCacheKey, o.cfg)

	ctx, _ := pkt[packet.KeyCtx].(context.Context)
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey := o.cfg.APIKey
	o.ensureInitialized(ctx, apiKey)

	sessionID := o.sessionID(pkt, apiKey, promptCacheKey)
	applyThreadID(body, sessionID)
	payload, err := json.Marshal(body)
	if err != nil {
		return serve(pkt.WithError("marshal commandcode request failed: " + err.Error()))
	}

	if streaming {
		events, err := o.openStream(ctx, apiKey, sessionID, payload, upstreamModel)
		if err != nil {
			return serve(pkt.Fail(packet.ErrUpstream, "commandcode call failed: "+err.Error()))
		}
		sourceResponse, err := lm.NewResponseStream("openai", events, ctx)
		if err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
		clientResponse, err := lm.NewResponseTarget(clientType, ctx)
		if err != nil {
			return serve(pkt.Fail(packet.ErrInternal, err.Error()))
		}
		converted, err := clientResponse.ConvertFrom(sourceResponse)
		if err != nil {
			return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
		}
		stream, ok := converted.(lm.LMResponse)
		if !ok {
			return serve(pkt.Fail(packet.ErrInternal, "stream conversion did not return LMResponse"))
		}
		pkt.Set(packet.KeyResp, stream)
		pkt.SetPhase(packet.PhaseStream)
		return serve(pkt, "stream")
	}

	chatDoc, err := o.complete(ctx, apiKey, sessionID, payload, upstreamModel)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, "commandcode call failed: "+err.Error()))
	}
	clientResponse, err := lm.NewResponse(clientType, map[string]any{})
	if err != nil {
		return serve(pkt.Fail(packet.ErrInternal, err.Error()))
	}
	converted, err := clientResponse.ConvertFrom(chatDoc)
	if err != nil {
		return serve(pkt.Fail(packet.ErrUpstream, err.Error()))
	}
	pkt.Set(packet.KeyResp, converted)
	pkt.SetPhase(packet.PhaseResp)
	return serve(pkt)
}

// openStream posts the CC request and returns a client event channel, retrying
// transport failures and streams that die before delivering any event. Once the
// first event has been observed it is committed, matching the proxy rule that a
// request is only retryable while nothing has been written downstream.
func (o *CommandCode) openStream(ctx context.Context, apiKey, sessionID string, payload []byte, model string) (<-chan lm.LMEvent, error) {
	attempts := o.cfg.RetryMax + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(o.cfg.RetryBaseMS*attempt)*time.Millisecond); err != nil {
				return nil, err
			}
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		upstream, err := o.post(attemptCtx, apiKey, sessionID, payload, true)
		if err != nil {
			cancel()
			lastErr = err
			if !retryableNetError(attemptCtx, err) {
				return nil, err
			}
			continue
		}
		if upstream.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(upstream.Body)
			upstream.Body.Close()
			cancel()
			return nil, errors.New(mapCCError(upstream.StatusCode, b))
		}
		body, idle := o.watch(upstream.Body, o.cfg.StreamIdleMS, cancel)
		raw := make(chan lm.LMEvent)
		go func() {
			o.translate(attemptCtx, body, raw, model)
			close(raw)
		}()
		first, ok := <-raw
		if !ok {
			cancel()
			if idle.Triggered() {
				return nil, errCCIdleTimeout
			}
			lastErr = errors.New("commandcode stream ended before first event")
			continue
		}
		out := make(chan lm.LMEvent)
		go func() {
			defer close(out)
			defer cancel()
			if !sendCCEvent(ctx, out, first) {
				return
			}
			for event := range raw {
				if !sendCCEvent(ctx, out, event) {
					return
				}
			}
			if idle.Triggered() {
				_ = sendCCEvent(ctx, out, lm.NewChatEvent(ccIdleErrorChunk()))
			}
		}()
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("commandcode upstream failed")
	}
	return nil, lastErr
}

// complete posts the CC request and accumulates the full chat response,
// retrying transport failures and incomplete streams (no finish event).
func (o *CommandCode) complete(ctx context.Context, apiKey, sessionID string, payload []byte, model string) (lm.LMDocument, error) {
	attempts := o.cfg.RetryMax + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(o.cfg.RetryBaseMS*attempt)*time.Millisecond); err != nil {
				return nil, err
			}
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		upstream, err := o.post(attemptCtx, apiKey, sessionID, payload, false)
		if err != nil {
			cancel()
			lastErr = err
			if !retryableNetError(attemptCtx, err) {
				return nil, err
			}
			continue
		}
		if upstream.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(upstream.Body)
			upstream.Body.Close()
			cancel()
			return nil, errors.New(mapCCError(upstream.StatusCode, b))
		}
		body, idle := o.watch(upstream.Body, o.cfg.NonStreamIdleMS, cancel)
		events := make(chan lm.LMEvent)
		trCh := make(chan *translator, 1)
		go func() {
			trCh <- o.translate(attemptCtx, body, events, model)
			close(events)
		}()
		chatDoc, err := lm.NewResponse("openai", map[string]any{})
		if err != nil {
			<-trCh
			cancel()
			return nil, err
		}
		for event := range events {
			_ = lm.ApplyEvent(chatDoc, event)
		}
		tr := <-trCh
		cancel()
		if tr.err != nil {
			// Semantic upstream error event: surface it, never retry.
			return nil, errors.New(asString(tr.err["message"]))
		}
		if idle.Triggered() {
			return nil, errCCIdleTimeout
		}
		if !tr.sawFinish {
			lastErr = errors.New("commandcode stream ended incomplete")
			continue
		}
		return chatDoc, nil
	}
	if lastErr == nil {
		lastErr = errors.New("commandcode upstream failed")
	}
	return nil, lastErr
}

func (o *CommandCode) watch(body io.ReadCloser, idleMS int, cancel context.CancelFunc) (io.ReadCloser, *idleTimeout) {
	idle := &idleTimeout{}
	if idleMS <= 0 {
		return body, idle
	}
	reader := newIdleReader(body, time.Duration(idleMS)*time.Millisecond, func() {
		idle.trigger()
		cancel()
	})
	return reader, idle
}

func ccIdleErrorChunk() map[string]any {
	return map[string]any{"error": map[string]any{
		"message":     "commandcode response timeout",
		"type":        "rate_limit_error",
		"retry_after": 30,
	}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryableNetError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// sessionID resolves the CC session id: client-supplied session headers or a
// prompt cache key win, otherwise a per-API-key session with a 12h + 1h jitter
// lifetime is reused.
func (o *CommandCode) sessionID(pkt packet.Packet, apiKey, promptCacheKey string) string {
	if meta, ok := pkt[packet.KeyHTTPMeta].(map[string]any); ok {
		for _, key := range []string{"session_id", "claude_code_session_id"} {
			if id, _ := meta[key].(string); len(id) >= 8 {
				return id
			}
		}
	}
	if len(promptCacheKey) >= 8 {
		return promptCacheKey
	}
	return o.ensureSession(apiKey)
}

func (o *CommandCode) ensureSession(apiKey string) string {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if state, ok := o.sessions[apiKey]; ok && now.Before(state.expiresAt) {
		return state.id
	}
	state := &sessionState{
		id:        newUUID(),
		expiresAt: now.Add(sessionDuration + randInt(sessionJitter)),
	}
	o.sessions[apiKey] = state
	return state.id
}

// applyThreadID inserts config.threadId, but only when the session id is a valid
// UUID (matching the CLI's toWireThreadId, which drops the key otherwise).
func applyThreadID(body map[string]any, sessionID string) {
	if !isUUID(sessionID) {
		return
	}
	body["threadId"] = sessionID
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// Discover fetches the live model catalog from the Command Code provider API.
func (o *CommandCode) Discover(ctx context.Context) ([]dispatch.ProviderModel, error) {
	o.mu.Lock()
	if len(o.models) > 0 && time.Since(o.modelsAt) < modelCacheTTL {
		cached := o.models
		o.mu.Unlock()
		return cached, nil
	}
	o.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cfg.BaseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, err
	}
	o.setWireHeaders(req, o.cfg.APIKey)
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("commandcode models returned %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	models := make([]dispatch.ProviderModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, dispatch.ProviderModel{ID: m.ID, OwnedBy: o.cfg.Name})
	}
	o.mu.Lock()
	o.models = models
	o.modelsAt = time.Now()
	o.mu.Unlock()
	return models, nil
}

func applyExtraBody(raw map[string]any, defaults, overrides map[string]any) {
	for k, v := range defaults {
		if _, ok := raw[k]; !ok {
			raw[k] = v
		}
	}
	for k, v := range overrides {
		raw[k] = v
	}
}

func (o *CommandCode) post(ctx context.Context, apiKey, sessionID string, payload []byte, streaming bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.BaseURL+"/alpha/generate", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	o.setWireHeaders(req, apiKey)
	req.Header.Set("x-project-slug", slugify(o.projectDir()))
	req.Header.Set("x-taste-learning", "false")
	req.Header.Set("x-session-id", sessionID)
	req.Header.Set("traceparent", generateTraceparent())
	if o.cfg.ZDR {
		req.Header.Set("x-cmd-zdr", "1")
	}
	client := o.client
	if streaming {
		client = o.streamClient
	}
	return client.Do(req)
}

func (o *CommandCode) setWireHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", ccProtocolVersion)
	req.Header.Set("x-cli-environment", "production")
}

// ---- fingerprint + lifecycle ----

func (o *CommandCode) ensureInitialized(ctx context.Context, apiKey string) {
	if apiKey == "" {
		return
	}
	o.mu.Lock()
	state, ok := o.keys[apiKey]
	if !ok {
		state = &keyState{fingerprint: generateFingerprint(apiKey, o.cfg.FingerprintSalt)}
		o.keys[apiKey] = state
	}
	if time.Now().Before(state.nextInitAt) {
		o.mu.Unlock()
		return
	}
	fingerprint := state.fingerprint
	o.mu.Unlock()

	headers := map[string]string{}
	o.doPost(ctx, o.cfg.BaseURL+"/alpha/fingerprint/record", apiKey, fingerprint, headers)
	o.doPost(ctx, o.cfg.BaseURL+"/alpha/lifecycle-events", apiKey, map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]any{
			"sessionId":  "sess_" + randomHex(8),
			"cliVersion": ccProtocolVersion,
			"mode":       o.cfg.CLISessionMode,
			"os":         "win32-x64",
		},
	}, headers)

	o.mu.Lock()
	state.nextInitAt = time.Now().Add(initRefresh + time.Duration(randInt(initJitter)))
	o.mu.Unlock()
}

func (o *CommandCode) doPost(ctx context.Context, url, apiKey string, body any, extra map[string]string) {
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-cli-environment", "production")
	o.setWireHeaders(req, apiKey)
	if o.cfg.ZDR {
		req.Header.Set("x-cmd-zdr", "1")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		log.Printf("[commandcode] %s failed: %v", url, err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

var fingerprintCPUs = []struct {
	model string
	cores int
}{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var (
	fingerprintMems      = []int{8, 16, 24, 32, 48, 64}
	fingerprintTZs       = []string{"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto", "Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow", "Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong", "Australia/Sydney", "Pacific/Auckland"}
	fingerprintMacCounts = []int{2, 3, 4, 5}
	fingerprintOSUsers   = []string{"dev", "user", "admin", "coder", "engineer", "work"}
	fingerprintDomains   = []string{"gmail.com", "outlook.com", "qq.com", "163.com"}
)

func fpDigest(apiKey, salt, field string) []byte {
	sum := sha256.Sum256([]byte(salt + "\x00" + apiKey + "\x00" + field))
	return sum[:]
}

func fpPick(apiKey, salt, field string, n int) int {
	best, bestScore := 0, []byte(nil)
	for i := 0; i < n; i++ {
		score := fpDigest(apiKey, salt, fmt.Sprintf("%s\x00%d", field, i))
		if bestScore == nil || bytesGreater(score, bestScore) {
			bestScore, best = score, i
		}
	}
	return best
}

func bytesGreater(a, b []byte) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}

func fingerprintHash(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fpSalt + "\x00" + v))
	return hex.EncodeToString(sum[:])
}

func generateFingerprint(apiKey, salt string) map[string]any {
	cpu := fingerprintCPUs[fpPick(apiKey, salt, "cpu", len(fingerprintCPUs))]
	mem := fingerprintMems[fpPick(apiKey, salt, "mem", len(fingerprintMems))]
	tz := fingerprintTZs[fpPick(apiKey, salt, "timezone", len(fingerprintTZs))]
	macCount := fingerprintMacCounts[fpPick(apiKey, salt, "macCount", len(fingerprintMacCounts))]
	osUser := fingerprintOSUsers[fpPick(apiKey, salt, "osUser", len(fingerprintOSUsers))]
	domain := fingerprintDomains[fpPick(apiKey, salt, "mailDomain", len(fingerprintDomains))]
	hexField := func(field string, n int) string {
		return hex.EncodeToString(fpDigest(apiKey, salt, field)[:n])
	}
	mid := hexField("machineId", 16)
	machineID := fmt.Sprintf("%s-%s-%s-%s-%s", mid[0:8], mid[8:12], mid[12:16], mid[16:20], mid[20:32])
	macs := make([]string, 0, macCount)
	for i := 0; i < macCount; i++ {
		raw := fpDigest(apiKey, salt, fmt.Sprintf("mac%d", i))[:6]
		parts := make([]string, len(raw))
		for j, b := range raw {
			parts[j] = hex.EncodeToString([]byte{b})
		}
		macs = append(macs, strings.Join(parts, ":"))
	}
	sort.Strings(macs)
	hostname := "DESKTOP-" + strings.ToUpper(hexField("hostname", 4))
	gitEmail := fmt.Sprintf("%s.%s@%s", osUser, hexField("gitEmail", 3), domain)

	thumbSeed := []string{machineID}
	thumbSeed = append(thumbSeed, strings.Join(macs, ","))
	thumb := sha256.Sum256([]byte(fpSalt + "\x00machine\x00" + strings.Join(thumbSeed, "|")))
	macHashes := make([]any, 0, len(macs))
	for _, mac := range macs {
		if h := fingerprintHash(mac); h != "" {
			macHashes = append(macHashes, h)
		}
	}

	return map[string]any{
		"thumbmark": hex.EncodeToString(thumb[:]),
		"components": map[string]any{
			"machineIdHash":    fingerprintHash(machineID),
			"macHashes":        macHashes,
			"osUserHash":       fingerprintHash(osUser),
			"hostnameHash":     fingerprintHash(hostname),
			"gitEmailHash":     fingerprintHash(gitEmail),
			"platform":         "win32",
			"arch":             "x64",
			"osRelease":        "10.0.22631",
			"cpuModel":         cpu.model,
			"cpuCount":         cpu.cores,
			"memGiB":           mem,
			"isContainer":      false,
			"timezone":         tz,
			"runtime":          "cli",
			"collectorVersion": 1,
		},
	}
}

// ---- request building ----

var toolNameAliases = map[string]string{
	"bash_output":         "shell_output",
	"task_output":         "shell_output",
	"tool_search":         "search_tools",
	"read_multiple_files": "read_file",
}

func buildCCRequest(chat map[string]any, model, promptCacheKey string, cfg Config) map[string]any {
	if model == "" {
		model = defaultModel
	}
	messages := asSlice(chat["messages"])
	systemBlocks := []any{}
	chatMessages := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			systemBlocks = append(systemBlocks, systemContentBlocks(msg["content"])...)
			continue
		}
		chatMessages = append(chatMessages, msg)
	}
	for i := 0; i < len(systemBlocks)-1; i++ {
		block := systemBlocks[i].(map[string]any)
		block["text"] = asString(block["text"]) + "\n"
	}
	// Cache breakpoints already present are preserved; otherwise an OpenAI
	// prompt_cache_key marks the last system block (the cached prefix).
	if promptCacheKey != "" && len(systemBlocks) > 0 && !hasCacheMarker(systemBlocks, chatMessages) {
		systemBlocks[len(systemBlocks)-1].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral"}
	}

	toolNames := map[string]string{}
	for _, raw := range chatMessages {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		if msg["role"] != "assistant" {
			continue
		}
		for _, rawCall := range asSlice(msg["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			if id, _ := call["id"].(string); id != "" {
				fn, _ := call["function"].(map[string]any)
				toolNames[id], _ = fn["name"].(string)
			}
		}
	}

	ccMessages := make([]any, 0, len(chatMessages))
	for _, raw := range chatMessages {
		msg, _ := raw.(map[string]any)
		role, _ := msg["role"].(string)
		switch role {
		case "user":
			ccMessages = append(ccMessages, map[string]any{"role": "user", "content": userContent(msg["content"])})
		case "assistant":
			ccMessages = append(ccMessages, map[string]any{"role": "assistant", "content": assistantContent(msg)})
		case "tool":
			callID, _ := msg["tool_call_id"].(string)
			name, _ := msg["name"].(string)
			if name == "" {
				name = toolNames[callID]
			}
			ccMessages = append(ccMessages, map[string]any{"role": "tool", "content": []any{map[string]any{
				"type": "tool-result", "toolCallId": callID, "toolName": name,
				"output": map[string]any{"type": "text", "value": toolOutputValue(msg["content"])},
			}}})
		default:
			ccMessages = append(ccMessages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": asString(msg["content"])}}})
		}
	}

	params := map[string]any{
		"model":      model,
		"messages":   ccMessages,
		"max_tokens": minInt(numberOr(chat["max_tokens"], 64000), 200000),
		"stream":     true,
	}
	if len(systemBlocks) > 0 {
		params["system"] = systemBlocks
	} else if cfg.EmptySystemPlaceholder {
		params["system"] = []any{map[string]any{"type": "text", "text": " "}}
	}
	if v, ok := chat["temperature"]; ok {
		params["temperature"] = v
	}
	if v, ok := chat["reasoning_effort"]; ok {
		params["reasoning_effort"] = v
	}
	tools := make([]any, 0)
	for _, rawTool := range asSlice(chat["tools"]) {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name := asString(fn["name"])
		if name == "" {
			name = asString(tool["name"])
		}
		description := asString(fn["description"])
		if description == "" {
			description = asString(tool["description"])
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = tool["input_schema"]
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		wire := toolNameAliases[name]
		if wire == "" {
			wire = name
		}
		tools = append(tools, map[string]any{"name": wire, "description": description, "input_schema": schema})
	}
	params["tools"] = tools
	if v, ok := chat["tool_choice"]; ok {
		params["tool_choice"] = ccToolChoice(v)
	}
	if v, ok := chat["parallel_tool_calls"]; ok {
		params["parallel_tool_calls"] = v
	}

	body := map[string]any{
		"config": map[string]any{
			"workingDir":    cfg.DeviceProjectDir,
			"date":          time.Now().UTC().Format("2006-01-02"),
			"environment":   "win32",
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"mode":           cfg.CLIMode,
		"params":         params,
	}
	if body["config"].(map[string]any)["workingDir"] == "" {
		body["config"].(map[string]any)["workingDir"] = `C:\Users\dev\projects\app`
	}
	return body
}

// systemContentBlocks normalizes system/developer content into CC text blocks,
// preserving per-block cache_control markers.
func systemContentBlocks(raw any) []any {
	switch content := raw.(type) {
	case string:
		if content == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": content}}
	case []any:
		blocks := []any{}
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if part == nil {
				continue
			}
			text := asString(part["text"])
			if text == "" {
				text = asString(part["content"])
			}
			if text == "" && part["cache_control"] == nil {
				continue
			}
			block := map[string]any{"type": "text", "text": text}
			if cache, ok := part["cache_control"]; ok {
				block["cache_control"] = cache
			}
			blocks = append(blocks, block)
		}
		return blocks
	default:
		if raw == nil {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": fmt.Sprint(raw)}}
	}
}

func hasCacheMarker(systemBlocks, chatMessages []any) bool {
	for _, raw := range systemBlocks {
		if block, _ := raw.(map[string]any); block != nil && block["cache_control"] != nil {
			return true
		}
	}
	for _, raw := range chatMessages {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		for _, rawPart := range asSlice(msg["content"]) {
			if part, _ := rawPart.(map[string]any); part != nil && part["cache_control"] != nil {
				return true
			}
		}
	}
	return false
}

func userContent(raw any) []any {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	out := []any{}
	for _, rawPart := range asSlice(raw) {
		part, _ := rawPart.(map[string]any)
		if part == nil {
			continue
		}
		if part["type"] == "image_url" {
			image, _ := part["image_url"].(map[string]any)
			url := asString(image["url"])
			item := map[string]any{"type": "image", "image": url}
			if i := strings.Index(url, ":"); i > 0 {
				if j := strings.Index(url, ";"); j > i {
					item["mimeType"] = url[i+1 : j]
				}
			}
			out = append(out, item)
			continue
		}
		out = append(out, part)
	}
	return out
}

func assistantContent(msg map[string]any) []any {
	parts := []any{}
	hasReasoning := false
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		parts = append(parts, map[string]any{"type": "reasoning", "text": reasoning})
		hasReasoning = true
	}
	switch content := msg["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, map[string]any{"type": "text", "text": content})
		}
	case []any:
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if part == nil {
				continue
			}
			switch part["type"] {
			case "text":
				parts = append(parts, part)
			case "reasoning":
				if !hasReasoning {
					parts = append(parts, part)
				}
			}
		}
	}
	for _, rawCall := range asSlice(msg["tool_calls"]) {
		call, _ := rawCall.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		var input any
		if args, ok := fn["arguments"].(string); ok {
			input = tryParseJSON(args)
		} else {
			input = fn["arguments"]
		}
		parts = append(parts, map[string]any{"type": "tool-call", "toolCallId": call["id"], "toolName": fn["name"], "input": input})
	}
	return parts
}

func toolOutputValue(raw any) string {
	if text, ok := raw.(string); ok {
		return text
	}
	parts := []string{}
	for _, rawPart := range asSlice(raw) {
		part, _ := rawPart.(map[string]any)
		if part != nil && part["type"] == "text" {
			parts = append(parts, asString(part["text"]))
		}
	}
	return strings.Join(parts, "\n")
}

func ccToolChoice(raw any) any {
	if text, ok := raw.(string); ok {
		switch text {
		case "auto", "none", "required":
			if text == "required" {
				return map[string]any{"type": "any"}
			}
			return map[string]any{"type": text}
		}
		return map[string]any{"type": "auto"}
	}
	obj, _ := raw.(map[string]any)
	if obj != nil && obj["type"] == "function" {
		fn, _ := obj["function"].(map[string]any)
		return map[string]any{"type": "tool", "name": fn["name"]}
	}
	return raw
}

func tryParseJSON(value string) any {
	var out any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return map[string]any{}
	}
	return out
}

// ---- CC NDJSON -> Chat chunk events ----

type translator struct {
	model        string
	id           string
	chunkIndex   int
	toolIndex    int
	finish       string
	usage        map[string]any
	inputTokens  int
	outputTokens int
	cachedTokens int
	sawFinish    bool
	err          map[string]any
}

// translate reads the CC NDJSON stream and emits chat chunk events. It returns
// the translator state; the caller owns out and closes it.
func (o *CommandCode) translate(ctx context.Context, body io.ReadCloser, out chan<- lm.LMEvent, model string) *translator {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	tr := &translator{model: model, id: "chatcmpl-" + randomHex(6), finish: "stop"}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return tr
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "[DONE]" || strings.HasPrefix(line, ":") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		for _, chunk := range tr.parse(event) {
			select {
			case out <- lm.NewChatEvent(chunk):
			case <-ctx.Done():
				return tr
			}
		}
	}
	return tr
}

func (t *translator) parse(event map[string]any) []map[string]any {
	typ, _ := event["type"].(string)
	switch typ {
	case "text-delta":
		text := asString(event["text"])
		if text == "" {
			text = asString(event["delta"])
		}
		if text == "" {
			return nil
		}
		return t.emit(map[string]any{"content": text}, "")
	case "reasoning-delta":
		text := asString(event["text"])
		if text == "" {
			return nil
		}
		return t.emit(map[string]any{"reasoning_content": text}, "")
	case "tool-call":
		id := asString(event["toolCallId"])
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), t.toolIndex)
		}
		var args string
		if s, ok := event["input"].(string); ok {
			args = s
		} else {
			b, _ := json.Marshal(event["input"])
			args = string(b)
		}
		call := map[string]any{"index": t.toolIndex, "id": id, "type": "function",
			"function": map[string]any{"name": asString(event["toolName"]), "arguments": args}}
		t.toolIndex++
		return t.emit(map[string]any{"tool_calls": []any{call}}, "")
	case "finish-step":
		t.sawFinish = true
		if reason := asString(event["finishReason"]); reason != "" {
			t.finish = mapFinishReason(reason)
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			t.usage = usage
		}
		return nil
	case "finish":
		t.sawFinish = true
		if usage, ok := event["totalUsage"].(map[string]any); ok {
			t.usage = usage
		} else if t.usage == nil {
			t.usage = map[string]any{}
		}
		if reason := asString(event["finishReason"]); reason != "" {
			t.finish = mapFinishReason(reason)
		}
		return t.emit(nil, t.finish)
	case "error":
		errObj := mapCCEventError(event)
		t.err = errObj
		return []map[string]any{{"error": errObj}}
	default:
		return nil
	}
}

func (t *translator) emit(delta map[string]any, finish string) []map[string]any {
	chunkDelta := delta
	if chunkDelta == nil {
		chunkDelta = map[string]any{}
	}
	if t.chunkIndex == 0 {
		chunkDelta["role"] = "assistant"
	}
	t.chunkIndex++
	chunk := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{map[string]any{"index": 0, "delta": chunkDelta, "finish_reason": anyOrNil(finish)}},
	}
	if finish != "" {
		if usage := t.normalizedUsage(); usage != nil {
			chunk["usage"] = usage
		}
	}
	return []map[string]any{chunk}
}

func (t *translator) normalizedUsage() map[string]any {
	if t.usage == nil {
		return nil
	}
	input := numberOr(t.usage["inputTokens"], 0)
	output := numberOr(t.usage["outputTokens"], 0)
	cached := numberOr(t.usage["cachedInputTokens"], 0)
	if cached == 0 {
		if details, ok := t.usage["inputTokenDetails"].(map[string]any); ok {
			cached = numberOr(details["cacheReadTokens"], 0)
		}
	}
	if output == 0 {
		input, cached = 0, 0
	}
	t.inputTokens, t.outputTokens, t.cachedTokens = input, output, cached
	return map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
	}
}

func mapFinishReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	if r == "" {
		return "stop"
	}
	switch r {
	case "tool-calls", "tool_calls", "tool_use":
		return "tool_calls"
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "length"
	}
	return r
}

// ccStatusMap mirrors the proxy's CC_STATUS_MAP: upstream CC status → downstream
// status plus an Anthropic/OpenAI-style error type.
var ccStatusMap = map[int]struct {
	Status int
	Type   string
}{
	400: {400, "invalid_request_error"},
	401: {401, "authentication_error"},
	402: {429, "rate_limit_error"}, // payment required → rate limit
	403: {401, "authentication_error"},
	404: {404, "not_found"},
	422: {400, "invalid_request_error"},
	429: {429, "rate_limit_error"},
	500: {502, "upstream_error"},
	502: {502, "upstream_error"},
	503: {503, "temporarily_unavailable"},
}

func mapCCStatus(status int) (int, string) {
	if mapped, ok := ccStatusMap[status]; ok {
		return mapped.Status, mapped.Type
	}
	return 502, "upstream_error"
}

func mapCCError(status int, body []byte) string {
	message := fmt.Sprintf("commandcode returned %d", status)
	code := ""
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if errObj, ok := parsed["error"].(map[string]any); ok {
			if m := asString(errObj["message"]); m != "" {
				message = m
			}
			code = asString(errObj["code"])
		} else if m := asString(parsed["message"]); m != "" {
			message = m
		}
	}
	_, typ := mapCCStatus(status)
	if code != "" {
		return fmt.Sprintf("%s (%s, code=%s)", message, typ, code)
	}
	return fmt.Sprintf("%s (%s)", message, typ)
}

// mapCCEventError normalizes a CC stream error event into the downstream error
// object, preserving the machine-readable code and retry hint.
func mapCCEventError(event map[string]any) map[string]any {
	errObj, _ := event["error"].(map[string]any)
	message := ""
	code := ""
	reported := 0
	if errObj != nil {
		message = asString(errObj["message"])
		code = asString(errObj["code"])
		reported = numberOr(errObj["statusCode"], 0)
	}
	if message == "" {
		message = asString(event["message"])
	}
	if code == "" {
		code = asString(event["code"])
	}
	if status := embeddedStatus(message); status != 0 {
		reported = status
	}
	if reported == 0 {
		reported = 502
	}
	status, typ := mapCCStatus(reported)
	out := map[string]any{"message": message, "type": typ}
	if code != "" {
		out["code"] = code
	}
	if status == 429 {
		out["retry_after"] = 30
	}
	return out
}

// embeddedStatus parses the leading "<NNN>" prefix the CC CLI embeds in some
// stream error messages.
func embeddedStatus(message string) int {
	if len(message) < 5 || message[0] != '<' || message[4] != '>' {
		return 0
	}
	status := 0
	for i := 1; i < 4; i++ {
		if message[i] < '0' || message[i] > '9' {
			return 0
		}
		status = status*10 + int(message[i]-'0')
	}
	return status
}

var errCCIdleTimeout = errors.New("commandcode response timeout")

// idleTimeout records whether the read-idle watchdog fired, so callers can
// surface a 429-style timeout instead of retrying (which would replay the whole
// context).
type idleTimeout struct {
	fired atomic.Bool
}

func (t *idleTimeout) trigger()        { t.fired.Store(true) }
func (t *idleTimeout) Triggered() bool { return t.fired.Load() }

func sendCCEvent(ctx context.Context, out chan<- lm.LMEvent, event lm.LMEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// idleReader cancels the request when no bytes have arrived for the configured
// idle timeout, resetting on every successful read.
type idleReader struct {
	inner   io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
}

func newIdleReader(inner io.ReadCloser, timeout time.Duration, onIdle func()) *idleReader {
	r := &idleReader{inner: inner, timeout: timeout}
	r.timer = time.AfterFunc(timeout, onIdle)
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.timer != nil {
		r.timer.Reset(r.timeout)
	}
	return r.inner.Read(p)
}

func (r *idleReader) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	return r.inner.Close()
}

// ---- helpers ----

func asSlice(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func numberOr(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return fallback
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func anyOrNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func slugify(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func generateTraceparent() string {
	return "00-" + randomHex(16) + "-" + randomHex(8) + "-01"
}

// newUUID returns a RFC 4122 version 4 UUID. The CC upstream validates the
// version/variant bits, so a plain random hex string is rejected.
func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

func randInt(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return 0
	}
	n := int64(buf[0])<<24 | int64(buf[1])<<16 | int64(buf[2])<<8 | int64(buf[3])
	return time.Duration(n % int64(max))
}
