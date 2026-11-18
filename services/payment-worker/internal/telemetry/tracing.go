package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"payment-worker/internal/config"
)

// NewTracing creates an independent provider. Exporting never blocks request handlers.
func NewTracing(ctx context.Context, service string, cfg config.TracingConfig) (*sdktrace.TracerProvider, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("OTLP endpoint must be an absolute http or https URL")
	}
	if cfg.ExportTimeout <= 0 || !(cfg.SampleRatio >= 0 && cfg.SampleRatio <= 1) {
		return nil, fmt.Errorf("invalid tracing timeout or sample ratio")
	}
	res, err := resource.New(ctx, resource.WithTelemetrySDK(), resource.WithAttributes(attribute.String("service.name", service)))
	if err != nil {
		return nil, fmt.Errorf("create tracing resource: %w", err)
	}
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithTimeout(cfg.ExportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled: true, InitialInterval: 100 * time.Millisecond,
			MaxInterval: 300 * time.Millisecond, MaxElapsedTime: cfg.ExportTimeout,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(cfg.ExportTimeout),
			sdktrace.WithMaxQueueSize(256),
			sdktrace.WithMaxExportBatchSize(64),
		),
	), nil
}
