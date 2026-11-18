package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"order-api/internal/config"
)

func TestTracingSamplingAndShutdownFlush(t *testing.T) {
	for _, test := range []struct {
		name         string
		ratio        float64
		remoteParent bool
		sampled      bool
		wantExport   bool
	}{
		{"all roots", 1, false, false, true},
		{"no roots", 0, false, false, false},
		{"sampled parent with zero root ratio", 0, true, true, true},
		{"unsampled parent with full root ratio", 1, true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			batches := make(chan *collectortrace.ExportTraceServiceRequest, 4)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/custom/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
					t.Errorf("unexpected OTLP request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				batch := new(collectortrace.ExportTraceServiceRequest)
				if err := proto.Unmarshal(body, batch); err != nil {
					t.Error(err)
				}
				batches <- batch
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer collector.Close()
			provider, err := NewTracing(context.Background(), "test-service", config.TracingConfig{
				Endpoint: collector.URL + "/custom/traces", ExportTimeout: time.Second, SampleRatio: test.ratio,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.remoteParent {
				var flags trace.TraceFlags
				if test.sampled {
					flags = trace.FlagsSampled
				}
				ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: flags, Remote: true,
				}))
			}
			_, span := provider.Tracer("test").Start(ctx, "pending-at-shutdown")
			span.End()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := provider.Shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			if !test.wantExport {
				if calls.Load() != 0 {
					t.Error("unsampled span was exported")
				}
				return
			}
			select {
			case batch := <-batches:
				if len(batch.ResourceSpans) != 1 {
					t.Fatalf("unexpected resource spans: %v", batch)
				}
				resource := batch.ResourceSpans[0]
				service := ""
				for _, attr := range resource.Resource.Attributes {
					if attr.Key == "service.name" {
						service = attr.Value.GetStringValue()
					}
				}
				if service != "test-service" || len(resource.ScopeSpans) != 1 || len(resource.ScopeSpans[0].Spans) != 1 || resource.ScopeSpans[0].Spans[0].Name != "pending-at-shutdown" {
					t.Fatalf("incorrect exported resource/span: %v", batch)
				}
			default:
				t.Fatal("shutdown did not flush the pending span")
			}
		})
	}
}

func TestTracingInitializationDoesNotWaitForCollector(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := collector.URL + "/v1/traces"
	collector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	provider, err := NewTracing(ctx, "offline-service", config.TracingConfig{Endpoint: endpoint, ExportTimeout: 100 * time.Millisecond, SampleRatio: 1})
	if err != nil {
		t.Fatalf("offline collector prevented initialization: %v", err)
	}
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
