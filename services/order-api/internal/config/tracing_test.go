package config

import (
	"testing"
	"time"
)

func clearTracingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_TRACES_SAMPLER_ARG"} {
		t.Setenv(name, "")
	}
}

func TestTracingDefaults(t *testing.T) {
	clearTracingEnv(t)
	cfg := Load().Tracing
	if cfg.Endpoint != "http://127.0.0.1:4318/v1/traces" || cfg.ExportTimeout != time.Second || cfg.SampleRatio != 1 {
		t.Fatalf("unexpected tracing defaults: %+v", cfg)
	}
}

func TestTracingOverrides(t *testing.T) {
	clearTracingEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318/custom/")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "2000")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
	cfg := Load().Tracing
	if cfg.Endpoint != "http://collector:4318/custom/v1/traces" || cfg.ExportTimeout != 2*time.Second || cfg.SampleRatio != .25 {
		t.Fatalf("unexpected generic tracing settings: %+v", cfg)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://traces.example/ingest")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "250")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
	cfg = Load().Tracing
	if cfg.Endpoint != "https://traces.example/ingest" || cfg.ExportTimeout != 250*time.Millisecond || cfg.SampleRatio != 0 {
		t.Fatalf("signal-specific settings did not take precedence: %+v", cfg)
	}
}

func TestInvalidTracingNumbersUseDefaults(t *testing.T) {
	for _, value := range []string{"invalid", "NaN", "Inf", "-1", "2"} {
		t.Run(value, func(t *testing.T) {
			clearTracingEnv(t)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
			t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "-100")
			cfg := Load().Tracing
			if cfg.SampleRatio != 1 || cfg.ExportTimeout != time.Second {
				t.Fatalf("invalid values did not use defaults: %+v", cfg)
			}
		})
	}
}
