package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// TracingConfig configures OTLP/HTTP and parent-based trace ID ratio sampling.
type TracingConfig struct {
	Endpoint      string
	ExportTimeout time.Duration
	SampleRatio   float64
}

func loadTracing() TracingConfig {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		if base == "" {
			base = "http://127.0.0.1:4318"
		}
		endpoint = strings.TrimRight(base, "/") + "/v1/traces"
	}

	timeout := time.Second
	value := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT")
	if value == "" {
		value = os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT")
	}
	// OTLP timeout environment variables use milliseconds.
	if value != "" {
		if duration, err := time.ParseDuration(value + "ms"); err == nil && duration > 0 {
			timeout = duration
		}
	}
	ratio := 1.0
	if value := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && parsed <= 1 {
			ratio = parsed
		}
	}
	return TracingConfig{Endpoint: endpoint, ExportTimeout: timeout, SampleRatio: ratio}
}
