package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestServerTraceContextAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		name, parent string
		wantSpans    int
		remote       bool
	}{
		{"sampled", "00-11111111111111111111111111111111-2222222222222222-01", 1, true},
		{"unsampled", "00-11111111111111111111111111111111-2222222222222222-00", 0, true},
		{"invalid", "invalid-parent", 1, false},
		{"missing", "", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			var received trace.SpanContext
			handler := WithTracerProvider(provider, ServerHandler("/operation", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = trace.SpanContextFromContext(r.Context())
				w.WriteHeader(http.StatusInternalServerError)
			})))
			req := httptest.NewRequest(http.MethodPost, "/operation?customer_id=private-customer", strings.NewReader(`{"amount":123,"customer_id":"private-customer"}`))
			req.Header.Set("traceparent", test.parent)
			req.Header.Set("tracestate", "vendor=value")
			req.Header.Set("baggage", "customer_id=private-customer")
			handler.ServeHTTP(httptest.NewRecorder(), req)
			if !received.IsValid() {
				t.Fatal("missing server span context")
			}
			if test.remote && received.TraceID().String() != "11111111111111111111111111111111" {
				t.Error("remote trace ID was not preserved")
			}
			spans := exporter.GetSpans()
			if len(spans) != test.wantSpans {
				t.Fatalf("expected %d exported spans, got %d", test.wantSpans, len(spans))
			}
			if len(spans) == 0 {
				return
			}
			span := spans[0]
			if span.Name != "POST /operation" || span.SpanKind != trace.SpanKindServer || span.Status.Code != codes.Error {
				t.Fatalf("unexpected server span: %+v", span)
			}
			if test.remote && (span.Parent.SpanID().String() != "2222222222222222" || !span.Parent.IsRemote()) {
				t.Error("remote parent relationship was lost")
			}
			encoded, err := json.Marshal(spans)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"private-customer", "customer_id", "baggage", "url.full", "url.query"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("private request data exported: %s", forbidden)
				}
			}
		})
	}
}

func TestServerPanicEndsSpanWithoutRecordingPanicText(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer provider.Shutdown(context.Background())
	handler := WithTracerProvider(provider, ServerHandler("/operation", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private-panic-payload")
	})))
	func() {
		defer func() {
			if recover() != "private-panic-payload" {
				t.Error("middleware changed panic propagation")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/operation", nil))
	}()
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Description != "handler_panic" {
		t.Fatalf("panic span not completed correctly: %v", spans)
	}
	encoded, _ := json.Marshal(spans)
	if strings.Contains(string(encoded), "private-panic-payload") {
		t.Error("panic contents leaked into trace")
	}
}
