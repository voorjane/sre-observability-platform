package telemetry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type providerKey struct{}

// WithTracerProvider keeps the application provider local to each request.
func WithTracerProvider(provider trace.TracerProvider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), providerKey{}, provider)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func Tracer(ctx context.Context, scope string) trace.Tracer {
	provider, ok := ctx.Value(providerKey{}).(trace.TracerProvider)
	if !ok {
		provider = trace.SpanFromContext(ctx).TracerProvider()
	}
	return provider.Tracer(scope)
}

// ServerHandler instruments a registered route without collecting raw URLs or bodies.
func ServerHandler(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		method := metricMethod(r.Method)
		ctx, span := Tracer(ctx, "http/server").Start(ctx, method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route)),
		)
		writer, status := observeResponse(w)
		completed := false
		defer func() {
			if completed {
				span.SetAttributes(attribute.Int("http.response.status_code", status()))
				if ctx.Err() != nil {
					RecordFailure(span, NetworkErrorKind(ctx.Err()))
				} else if status() >= 500 {
					RecordFailure(span, "http_5xx")
				}
			} else {
				RecordFailure(span, "handler_panic")
			}
			span.End()
		}()
		next.ServeHTTP(writer, r.WithContext(ctx))
		completed = true
	})
}

func InjectTraceContext(ctx context.Context, header http.Header) {
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(header))
}

// RecordFailure deliberately records a stable category, never a raw error message.
func RecordFailure(span trace.Span, kind string) {
	span.SetAttributes(attribute.String("error.type", kind))
	span.SetStatus(codes.Error, kind)
	span.RecordError(errors.New(kind))
}

func NetworkErrorKind(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	return "unreachable"
}

// observeResponse preserves optional writer interfaces and captures the first final status.
func observeResponse(w http.ResponseWriter) (http.ResponseWriter, func() int) {
	status := 0
	implicitOK := func() {
		if status == 0 {
			status = http.StatusOK
		}
	}
	writer := httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				next(code)
				if status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
					status = code
				}
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				implicitOK()
				return next(body)
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				implicitOK()
				next()
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(body io.Reader) (int64, error) {
				implicitOK()
				return next(body)
			}
		},
	})
	return writer, func() int { implicitOK(); return status }
}
