package telemetry

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func NewLogger(service string) *zap.Logger {
	return NewLoggerWithWriter(service, os.Stdout)
}

// NewLoggerWithWriter produces one JSON object per line without log sampling.
func NewLoggerWithWriter(service string, output io.Writer) *zap.Logger {
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey: "ts", LevelKey: "level", MessageKey: "msg",
		EncodeLevel: zapcore.LowercaseLevelEncoder,
		EncodeTime: func(value time.Time, encoder zapcore.PrimitiveArrayEncoder) {
			encoder.AppendString(value.UTC().Format(time.RFC3339Nano))
		},
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	return zap.New(zapcore.NewCore(encoder, zapcore.Lock(zapcore.AddSync(output)), zap.InfoLevel)).With(zap.String("service", service))
}

// InstallErrorHandler is called once by main, before initializing the SDK.
func InstallErrorHandler(logger *zap.Logger) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		logger.Error("telemetry export failed", zap.String("event", "telemetry_error"), zap.String("error.type", "otel_export_failed"))
	}))
}

type httpErrorWriter struct{ logger *zap.Logger }

func (w httpErrorWriter) Write(data []byte) (int, error) {
	// net/http diagnostics may contain remote IPs, panic values and raw requests.
	w.logger.Error("http server diagnostic", zap.String("event", "http_server_error"), zap.String("error.type", "http_server"))
	return len(data), nil
}

func HTTPErrorLogger(logger *zap.Logger) *log.Logger {
	return log.New(httpErrorWriter{logger: logger}, "", 0)
}

type requestLogKey struct{}

type requestLog struct {
	span    trace.SpanContext
	failure string
}

func rememberRequestSpan(ctx context.Context) {
	if state, ok := ctx.Value(requestLogKey{}).(*requestLog); ok {
		state.span = trace.SpanContextFromContext(ctx)
	}
}

// SetRequestError attaches a stable category to the single request completion log.
func SetRequestError(ctx context.Context, category string) {
	if state, ok := ctx.Value(requestLogKey{}).(*requestLog); ok {
		state.failure = category
	}
}

func RequestLogging(logger *zap.Logger, mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		state := &requestLog{span: trace.SpanContextFromContext(r.Context())}
		ctx := context.WithValue(r.Context(), requestLogKey{}, state)
		route := requestRoute(mux, r)
		writer, status := observeResponseStatus(w)
		completed := false
		defer func() {
			code, level, message := status(), zap.InfoLevel, "http request completed"
			if completed && code == 0 {
				code = http.StatusOK
			}
			if !completed {
				state.failure, level, message = "handler_panic", zap.ErrorLevel, "http request aborted"
			} else if code >= 500 {
				level = zap.ErrorLevel
				if state.failure == "" {
					state.failure = "http_5xx"
				}
			} else if code >= 400 {
				level = zap.WarnLevel
				if state.failure == "" {
					state.failure = "request_rejected"
				}
			}
			fields := []zap.Field{
				zap.String("event", "http_request"), zap.String("method", metricMethod(r.Method)),
				zap.String("route", route), zap.Int("status_code", code),
				zap.Float64("duration_ms", time.Since(started).Seconds()*1000),
			}
			if state.span.IsValid() {
				fields = append(fields, zap.String("trace_id", state.span.TraceID().String()), zap.String("span_id", state.span.SpanID().String()))
			}
			if state.failure != "" {
				fields = append(fields, zap.String("error.type", state.failure))
			}
			logger.Log(level, message, fields...)
		}()
		next.ServeHTTP(writer, r.WithContext(ctx))
		completed = true
	})
}
