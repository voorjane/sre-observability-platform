package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"payment-worker/internal/service"
	"payment-worker/internal/telemetry"
)

func TestRouterMetricsIncludePaymentProcessingLatency(t *testing.T) {
	router := NewRouter(NewHealthHandler("payment-worker", "2"), NewPaymentHandler(service.NewPaymentService(30*time.Millisecond)), zap.NewNop())
	for _, request := range []struct {
		body string
		code int
	}{
		{`{"order_id":"ord-1","amount":10}`, 200},
		{`{"order_id":"ord-2","amount":0}`, 400},
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/internal/process?request_id=unique", bytes.NewBufferString(request.body)))
		if rr.Code != request.code {
			t.Fatalf("expected HTTP %d, got %d", request.code, rr.Code)
		}
	}
	for i := 0; i < 2; i++ {
		families := scrapeMetrics(t, router)
		if len(families["http_requests_total"].Metric) != 2 {
			t.Fatalf("expected two status series, got %v", families["http_requests_total"])
		}
		for _, code := range []string{"200", "400"} {
			labels := map[string]string{"service": "payment-worker", "method": "POST", "handler": "/internal/process", "status_code": code}
			if got := metricWithLabels(t, families["http_requests_total"], labels).GetCounter().GetValue(); got != 1 {
				t.Errorf("status %s: expected one request, got %v", code, got)
			}
			histogram := metricWithLabels(t, families["http_request_duration_seconds"], labels).GetHistogram()
			if histogram.GetSampleCount() != 1 {
				t.Errorf("status %s: unexpected histogram count %d", code, histogram.GetSampleCount())
			}
			if code == "200" && histogram.GetSampleSum() < 0.03 {
				t.Errorf("histogram excluded payment processing latency: %f", histogram.GetSampleSum())
			}
		}
		if got := metricWithLabels(t, families["http_in_flight_requests"], map[string]string{"service": "payment-worker"}).GetGauge().GetValue(); got != 0 {
			t.Errorf("expected zero in-flight requests, got %v", got)
		}
	}
}

func TestPaymentRouterTracing(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		cancelled bool
		code      int
		spans     int
	}{
		{"success", `{"order_id":"private-order","amount":10}`, false, 200, 2},
		{"invalid request", `{}`, false, 400, 1},
		{"cancelled", `{"order_id":"private-order","amount":10}`, true, 500, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "payment-worker"))))
			defer provider.Shutdown(context.Background())
			var logs bytes.Buffer
			router := telemetry.WithTracerProvider(provider, NewRouter(NewHealthHandler("payment-worker", "2"), NewPaymentHandler(service.NewPaymentService(5*time.Millisecond)), telemetry.NewLoggerWithWriter("payment-worker", &logs)))
			req := httptest.NewRequest(http.MethodPost, "/internal/process?customer=private-query", strings.NewReader(test.body))
			req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
			if test.cancelled {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != test.code {
				t.Fatalf("expected HTTP %d, got %d", test.code, rr.Code)
			}
			for _, path := range []string{"/healthz", "/metrics"} {
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			}
			spans := exporter.GetSpans()
			if len(spans) != test.spans {
				t.Fatalf("expected %d spans, got %d", test.spans, len(spans))
			}
			server := spans[len(spans)-1]
			var entry map[string]any
			if err := json.NewDecoder(&logs).Decode(&entry); err != nil {
				t.Fatal(err)
			}
			if entry["service"] != "payment-worker" || entry["status_code"] != float64(test.code) || entry["span_id"] != server.SpanContext.SpanID().String() || entry["trace_id"] != server.SpanContext.TraceID().String() {
				t.Fatalf("request log does not match server span: %v", entry)
			}
			if test.cancelled && entry["error.type"] != "canceled" || test.code == 400 && entry["error.type"] != "invalid_request_fields" {
				t.Fatalf("request error category missing: %v", entry)
			}
			if server.Name != "POST /internal/process" || server.SpanKind != trace.SpanKindServer || server.Parent.SpanID().String() != "2222222222222222" {
				t.Fatalf("unexpected server span: %+v", server)
			}
			if test.spans == 2 {
				operation := spans[0]
				if operation.Name != "payment.process" || operation.Parent.SpanID() != server.SpanContext.SpanID() {
					t.Error("payment business span is not a child of the HTTP span")
				}
				if test.cancelled && (operation.Status.Code != codes.Error || operation.Status.Description != "canceled" || len(operation.Events) == 0) {
					t.Error("payment cancellation was not recorded")
				}
			}
			for _, span := range spans {
				if span.SpanContext.TraceID().String() != "11111111111111111111111111111111" {
					t.Error("worker started a different trace")
				}
			}
			encoded, _ := json.Marshal(spans)
			if strings.Contains(string(encoded), "private-order") || strings.Contains(string(encoded), "private-query") {
				t.Error("payment data leaked into tracing")
			}
		})
	}
}

func scrapeMetrics(t *testing.T, router http.Handler) map[string]*dto.MetricFamily {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics endpoint returned %d: %s", rr.Code, rr.Body.String())
	}
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(rr.Body)
	if err != nil {
		t.Fatalf("invalid Prometheus exposition: %v", err)
	}
	if families["go_goroutines"] == nil || families["process_cpu_seconds_total"] == nil {
		t.Error("missing Go runtime or process metrics")
	}
	return families
}

func metricWithLabels(t *testing.T, family *dto.MetricFamily, expected map[string]string) *dto.Metric {
	t.Helper()
	for _, metric := range family.GetMetric() {
		if len(metric.Label) != len(expected) {
			continue
		}
		matches := true
		for _, label := range metric.Label {
			if value, ok := expected[label.GetName()]; !ok || value != label.GetValue() {
				matches = false
			}
		}
		if matches {
			return metric
		}
	}
	t.Fatalf("metric %s with labels %v not found", family.GetName(), expected)
	return nil
}
