package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"payment-worker/internal/service"
)

func TestRouterMetricsIncludePaymentProcessingLatency(t *testing.T) {
	router := NewRouter(NewHealthHandler("payment-worker", "2"), NewPaymentHandler(service.NewPaymentService(30*time.Millisecond)))
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
