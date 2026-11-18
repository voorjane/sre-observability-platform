package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"order-api/internal/client/payment"
	"order-api/internal/model"
)

func TestRouterMetricsIncludeOrderErrorsAndChaosLatency(t *testing.T) {
	svc := &mockOrderService{createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
		switch req.ItemID {
		case "invalid-response":
			return nil, payment.ErrInvalidResponse
		case "unavailable":
			return nil, &payment.UnreachableError{Err: errors.New("connection refused")}
		default:
			return &model.OrderResponse{OrderID: "ord-1", Status: "COMPLETED", PaymentStatus: "PROCESSED"}, nil
		}
	}}
	router := NewRouter(NewHealthHandler("order-api", "2"), NewOrderHandler(svc))
	for _, request := range []struct {
		path string
		body string
		code int
	}{
		{"/api/v1/orders", `{"item_id":"item-1","amount":10,"customer_id":"cust-1"}`, 201},
		{"/api/v1/orders", `{}`, 400},
		{"/api/v1/orders?fail_rate=1&latency_ms=40", `{"item_id":"item-1","amount":10,"customer_id":"cust-1"}`, 500},
		{"/api/v1/orders", `{"item_id":"invalid-response","amount":10,"customer_id":"cust-1"}`, 502},
		{"/api/v1/orders", `{"item_id":"unavailable","amount":10,"customer_id":"cust-1"}`, 503},
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, request.path, bytes.NewBufferString(request.body)))
		if rr.Code != request.code {
			t.Fatalf("expected HTTP %d, got %d", request.code, rr.Code)
		}
	}
	for i := 0; i < 2; i++ {
		families := scrapeMetrics(t, router)
		if len(families["http_requests_total"].Metric) != 5 {
			t.Fatalf("expected five status series, got %v", families["http_requests_total"])
		}
		for _, code := range []string{"201", "400", "500", "502", "503"} {
			labels := map[string]string{"service": "order-api", "method": "POST", "handler": "/api/v1/orders", "status_code": code}
			if got := metricWithLabels(t, families["http_requests_total"], labels).GetCounter().GetValue(); got != 1 {
				t.Errorf("status %s: expected one request, got %v", code, got)
			}
			histogram := metricWithLabels(t, families["http_request_duration_seconds"], labels).GetHistogram()
			if histogram.GetSampleCount() != 1 {
				t.Errorf("status %s: unexpected histogram count %d", code, histogram.GetSampleCount())
			}
			if code == "500" && histogram.GetSampleSum() < 0.04 {
				t.Errorf("histogram excluded chaos latency: %f", histogram.GetSampleSum())
			}
		}
		if got := metricWithLabels(t, families["http_in_flight_requests"], map[string]string{"service": "order-api"}).GetGauge().GetValue(); got != 0 {
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
