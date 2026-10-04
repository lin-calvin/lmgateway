package observability

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
)

func Init(ctx context.Context) (func(context.Context) error, error) {
	phoenixEndpoint := os.Getenv("PHOENIX_COLLECTOR_HTTP_ENDPOINT")
	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	tracesEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if phoenixEndpoint == "" && otlpEndpoint == "" && tracesEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	var options []otlptracehttp.Option
	if phoenixEndpoint != "" {
		options = append(options, otlptracehttp.WithEndpointURL(phoenixEndpoint))
		if apiKey := os.Getenv("PHOENIX_API_KEY"); apiKey != "" {
			options = append(options, otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer " + apiKey}))
		}
	}
	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, err
	}
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "lmgateway"
	}
	attributes := []attribute.KeyValue{attribute.String("service.name", serviceName)}
	if projectName := os.Getenv("PHOENIX_PROJECT_NAME"); projectName != "" {
		attributes = append(attributes, attribute.String("openinference.project.name", projectName))
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		"https://opentelemetry.io/schemas/1.26.0",
		attributes...,
	))
	if err != nil {
		return nil, err
	}
	provider := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
