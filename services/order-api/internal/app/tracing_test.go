package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"order-api/internal/config"
	"order-api/internal/model"
	"order-api/internal/telemetry"
)

func recordingProvider(t *testing.T, service string) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider, exporter
}

func TestOrderDistributedTrace(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		kind string
	}{
		{"success", 201, ""},
		{"worker failure", 502, "dependency_http_error"},
		{"invalid response", 502, "invalid_dependency_response"},
		{"timeout before headers", 503, "dependency_timeout"},
		{"timeout during body", 503, "dependency_timeout"},
		{"unreachable", 503, "dependency_unreachable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ordersProvider, ordersExporter := recordingProvider(t, "order-api")
			workerProvider, workerExporter := recordingProvider(t, "payment-worker")
			headers := make(chan http.Header, 1)
			worker := httptest.NewServer(telemetry.WithTracerProvider(workerProvider, telemetry.ServerHandler("/internal/process", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				ctx, span := telemetry.Tracer(r.Context(), "payment-worker/service").Start(r.Context(), "payment.process")
				defer span.End()
				var payment model.PaymentRequest
				if err := json.NewDecoder(r.Body).Decode(&payment); err != nil {
					t.Error(err)
				}
				switch test.name {
				case "worker failure":
					telemetry.RecordFailure(span, "worker_failure")
					http.Error(w, "private-upstream-message", http.StatusInternalServerError)
				case "invalid response":
					_, _ = w.Write([]byte("private-invalid-json"))
				case "timeout before headers", "timeout during body":
					if test.name == "timeout during body" {
						_, _ = w.Write([]byte(`{"order_id":`))
						w.(http.Flusher).Flush()
					}
					<-ctx.Done()
					telemetry.RecordFailure(span, "canceled")
				default:
					_ = json.NewEncoder(w).Encode(model.PaymentResponse{OrderID: payment.OrderID, Status: "PROCESSED"})
				}
			}))))
			defer worker.Close()
			if test.name == "unreachable" {
				worker.Close()
			}
			application := New(&config.Config{PaymentWorkerURL: worker.URL, PaymentTimeout: 100 * time.Millisecond})
			orders := httptest.NewServer(telemetry.WithTracerProvider(ordersProvider, application.httpServer.Handler))
			defer orders.Close()
			payload := []byte(`{"item_id":"private-item","amount":49.99,"customer_id":"private-customer"}`)
			request, err := http.NewRequest(http.MethodPost, orders.URL+"/api/v1/orders?customer=private-query", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
			request.Header.Set("tracestate", "vendor=value")
			request.Header.Set("baggage", "customer=private-baggage")
			response, err := orders.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != test.code {
				t.Fatalf("expected HTTP %d, got %d", test.code, response.StatusCode)
			}
			wantWorker := 2
			if test.name == "unreachable" {
				wantWorker = 0
			}
			deadline := time.Now().Add(time.Second)
			for len(workerExporter.GetSpans()) != wantWorker && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			spans := append(ordersExporter.GetSpans(), workerExporter.GetSpans()...)
			if len(spans) != 3+wantWorker {
				t.Fatalf("expected %d spans, got %d", 3+wantWorker, len(spans))
			}
			byRole := map[string]tracetest.SpanStub{}
			for _, span := range spans {
				if span.SpanContext.TraceID().String() != "11111111111111111111111111111111" {
					t.Errorf("span %s started another trace", span.Name)
				}
				byRole[span.Name+"/"+span.SpanKind.String()] = span
			}
			server := byRole["POST /api/v1/orders/server"]
			operation := byRole["order.create/internal"]
			client := byRole["POST /internal/process/client"]
			if server.Parent.SpanID().String() != "2222222222222222" || operation.Parent.SpanID() != server.SpanContext.SpanID() || client.Parent.SpanID() != operation.SpanContext.SpanID() {
				t.Error("order server -> business -> client parent chain is broken")
			}
			if wantWorker != 0 {
				workerServer := byRole["POST /internal/process/server"]
				workerOperation := byRole["payment.process/internal"]
				if workerServer.Parent.SpanID() != client.SpanContext.SpanID() || workerOperation.Parent.SpanID() != workerServer.SpanContext.SpanID() {
					t.Error("client -> worker server -> business parent chain is broken")
				}
				forwarded := <-headers
				parent := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(forwarded)))
				if parent.SpanID() != client.SpanContext.SpanID() || forwarded.Get("tracestate") != "vendor=value" || forwarded.Get("baggage") != "" {
					t.Error("W3C propagation or baggage isolation is incorrect")
				}
			}
			if test.kind != "" {
				if client.Status.Code != codes.Error || client.Status.Description != test.kind || operation.Status.Code != codes.Error || server.Status.Code != codes.Error {
					t.Errorf("failure missing from trace: client=%v operation=%v server=%v", client.Status, operation.Status, server.Status)
				}
				if len(client.Events) == 0 {
					t.Error("client failure event was not recorded")
				}
			} else if client.Status.Code == codes.Error || operation.Status.Code == codes.Error || server.Status.Code == codes.Error {
				t.Error("successful order was marked as an error")
			}
			encoded, _ := json.Marshal(spans)
			for _, secret := range []string{"private-item", "private-customer", "private-query", "private-baggage", "private-upstream-message", "private-invalid-json", worker.URL} {
				if strings.Contains(string(encoded), secret) {
					t.Errorf("private data exported: %s", secret)
				}
			}
		})
	}
}
