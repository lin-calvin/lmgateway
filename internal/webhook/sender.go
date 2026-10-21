// Package webhook is a small, shared JSON webhook sender. It is fire-and-forget
// with a bounded in-flight buffer, so a slow or dead receiver never blocks the
// caller. Both the logprobs exporter and the pool-event alarm use it.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

// Config is resolved on every Emit, so URL/timeout can change at runtime
// without re-wiring the sender. An empty URL disables delivery.
type Config struct {
	URL        string
	TimeoutSec int
}

// Sender POSTs JSON payloads to a configurable URL.
type Sender struct {
	config   func() Config
	client   *http.Client
	inFlight chan struct{}
}

// New returns a sender. config is called on every Emit.
func New(config func() Config) *Sender {
	return &Sender{
		config:   config,
		client:   &http.Client{},
		inFlight: make(chan struct{}, 256),
	}
}

// Enabled reports whether the sender currently has a URL configured.
func (s *Sender) Enabled() bool { return s.config().URL != "" }

// Emit serialises payload and delivers it in the background. If the buffer is
// full the event is dropped (never blocks the caller).
func (s *Sender) Emit(payload any) {
	cfg := s.config()
	if cfg.URL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[webhook] marshal failed: %v", err)
		return
	}
	select {
	case s.inFlight <- struct{}{}:
		go s.send(cfg, body)
	default:
		log.Printf("[webhook] buffer full, dropping payload")
	}
}

func (s *Sender) send(cfg Config, body []byte) {
	defer func() { <-s.inFlight }()
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[webhook] request: %v", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		log.Printf("[webhook] post failed: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode >= 300 {
		log.Printf("[webhook] returned %d", response.StatusCode)
	}
}
