package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func logEntries(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log %q: %v", line, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, entry["ts"].(string)); err != nil {
			t.Fatal(err)
		}
		if entry["service"] != "test-service" || entry["msg"] == "" || entry["level"] == "" {
			t.Fatalf("missing common log fields: %v", entry)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestRequestLogMatchesServerSpanAndOmitsPrivateData(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		t.Run(map[bool]string{true: "sampled", false: "unsampled"}[sampled], func(t *testing.T) {
			var output bytes.Buffer
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())))
			defer provider.Shutdown(context.Background())
			var server trace.SpanContext
			mux := http.NewServeMux()
			mux.Handle("POST /orders/{id}", ServerHandler("/orders/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server = trace.SpanContextFromContext(r.Context())
				_, child := Tracer(r.Context(), "business").Start(r.Context(), "operation")
				defer child.End()
				w.WriteHeader(http.StatusCreated)
			})))
			handler := WithTracerProvider(provider, RequestLogging(NewLoggerWithWriter("test-service", &output), mux, mux))
			req := httptest.NewRequest(http.MethodPost, "/orders/private-path?token=private-query", strings.NewReader(`{"customer":"private-body"}`))
			flags := "01"
			if !sampled {
				flags = "00"
			}
			req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-"+flags)
			req.Header.Set("Authorization", "Bearer private-auth")
			handler.ServeHTTP(httptest.NewRecorder(), req)
			entries := logEntries(t, &output)
			if len(entries) != 1 {
				t.Fatalf("expected one log per request, got %d", len(entries))
			}
			entry := entries[0]
			if entry["trace_id"] != server.TraceID().String() || entry["span_id"] != server.SpanID().String() || entry["span_id"] == "2222222222222222" {
				t.Fatalf("log must identify server span: %v", entry)
			}
			if entry["trace_id"] != "11111111111111111111111111111111" || entry["route"] != "/orders/{id}" || entry["status_code"] != float64(201) || entry["method"] != "POST" || entry["duration_ms"].(float64) < 0 {
				t.Fatalf("unexpected request fields: %v", entry)
			}
			if sampled {
				spans := exporter.GetSpans()
				if len(spans) != 2 || entry["span_id"] != spans[1].SpanContext.SpanID().String() {
					t.Fatal("log does not match exported server span")
				}
			} else if len(exporter.GetSpans()) != 0 {
				t.Fatal("unsampled trace was exported")
			}
			for _, forbidden := range []string{"private-", "customer", "Authorization", "token"} {
				if strings.Contains(output.String(), forbidden) {
					t.Fatalf("private data in log: %s", forbidden)
				}
			}
		})
	}
}

func TestRequestLogStatusLevelAndInterfaces(t *testing.T) {
	for _, test := range []struct {
		name            string
		handle          http.HandlerFunc
		status          int
		level, category string
	}{
		{"implicit", func(http.ResponseWriter, *http.Request) {}, 200, "info", ""},
		{"first final", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201); w.WriteHeader(500) }, 201, "info", ""},
		{"informational", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(201) }, 201, "info", ""},
		{"write", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")); w.WriteHeader(500) }, 200, "info", ""},
		{"flush", func(w http.ResponseWriter, _ *http.Request) { w.(http.Flusher).Flush(); w.WriteHeader(500) }, 200, "info", ""},
		{"bad request", func(w http.ResponseWriter, r *http.Request) {
			SetRequestError(r.Context(), "invalid_request_payload")
			w.WriteHeader(400)
		}, 400, "warn", "invalid_request_payload"},
		{"server failure", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }, 502, "error", "http_5xx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			mux := http.NewServeMux()
			mux.Handle("POST /operation", test.handle)
			RequestLogging(NewLoggerWithWriter("test-service", &output), mux, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/operation", nil))
			entry := logEntries(t, &output)[0]
			if entry["status_code"] != float64(test.status) || entry["level"] != test.level || entry["trace_id"] != nil || entry["span_id"] != nil {
				t.Fatalf("unexpected fields: %v", entry)
			}
			if test.category != "" && entry["error.type"] != test.category {
				t.Fatalf("missing safe error category: %v", entry)
			}
		})
	}
}

func TestRequestLogUnmatchedRoutesAndNoSampling(t *testing.T) {
	var output bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	handler := RequestLogging(NewLoggerWithWriter("test-service", &output), mux, mux)
	for i := 0; i < 150; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	for _, test := range []struct {
		method, path string
		status       int
		route        string
	}{
		{"PRIVATE-METHOD", "/private-path?secret=private-query", 404, "unmatched"},
		{"POST", "/healthz", 405, "unmatched"},
		{"GET", "/private/../healthz", 301, "/healthz"},
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(test.method, test.path, nil))
		entries := logEntries(t, &output)
		entry := entries[len(entries)-1]
		if entry["route"] != test.route || entry["status_code"] != float64(test.status) {
			t.Fatalf("unsafe route or incorrect status: %v", entry)
		}
		if test.method == "PRIVATE-METHOD" && entry["method"] != "OTHER" {
			t.Fatal("raw method logged")
		}
	}
	if len(logEntries(t, &output)) != 153 || strings.Contains(output.String(), "private") {
		t.Fatal("requests were sampled or raw URL was logged")
	}
}

func TestRequestPanicLogAndSanitizedDiagnostics(t *testing.T) {
	var output bytes.Buffer
	logger := NewLoggerWithWriter("test-service", &output)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /panic", func(http.ResponseWriter, *http.Request) { panic("private-panic") })
	func() {
		defer func() {
			if recover() != "private-panic" {
				t.Error("panic propagation changed")
			}
		}()
		RequestLogging(logger, mux, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panic", nil))
	}()
	HTTPErrorLogger(logger).Println("private-http-diagnostic")
	entries := logEntries(t, &output)
	if len(entries) != 2 || entries[0]["status_code"] != float64(0) || entries[0]["error.type"] != "handler_panic" || entries[1]["event"] != "http_server_error" {
		t.Fatalf("missing panic/diagnostic logs: %v", entries)
	}
	if strings.Contains(output.String(), "private-") {
		t.Fatal("raw diagnostics leaked into log")
	}
}

func TestPanicLogPreservesAlreadyCommittedStatus(t *testing.T) {
	var output bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("POST /panic", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		panic("private-panic")
	})
	func() {
		defer func() {
			if recover() != "private-panic" {
				t.Error("panic propagation changed")
			}
		}()
		RequestLogging(NewLoggerWithWriter("test-service", &output), mux, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panic", nil))
	}()
	entry := logEntries(t, &output)[0]
	if entry["status_code"] != float64(201) || entry["level"] != "error" || entry["error.type"] != "handler_panic" {
		t.Fatalf("committed response status lost on panic: %v", entry)
	}
}
