package handler

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"lmgateway/internal/keytag"
	"lmgateway/internal/packet"
)

func TestLimitObservationValueSanitizesUTF8(t *testing.T) {
	got := limitObservationValue(string([]byte{'o', 'k', 0xff, 'x'}))
	if got != "ok\ufffdx" {
		t.Fatalf("unexpected sanitized value: %q", got)
	}
}

func TestTagAttributesExportedToOTel(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(previous)

	pkt := packet.NewReq(map[string]any{
		"model":           "m",
		keytag.TagField:   "claude-code",
		keytag.HintField:  "pool_policy=price_first",
		keytag.HintsField: map[string]any{"pool_policy": "price_first"},
	})
	Observe{}.Handle(pkt, noopServe)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	attrs := map[string]string{}
	for _, attr := range spans[0].Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	if attrs["lmgateway.key_tag"] != "claude-code" {
		t.Fatalf("key tag attr = %v", attrs)
	}
	if attrs["lmgateway.route_hint"] != "pool_policy=price_first" {
		t.Fatalf("route hint attr = %v", attrs)
	}
	if attrs["lmgateway.route_hints.pool_policy"] != "price_first" {
		t.Fatalf("route hints attr = %v", attrs)
	}
}

func TestObservationBudgetLimitsContent(t *testing.T) {
	budget := newObservationBudget(10)
	if got := budget.take("0123456789abc", 5); got != "01234" {
		t.Fatalf("first take = %q", got)
	}
	if got := budget.take("abcdef", 10); got != "abcde" {
		t.Fatalf("second take = %q", got)
	}
	if got := budget.take("more", 10); got != "" {
		t.Fatalf("exhausted take = %q", got)
	}
}
