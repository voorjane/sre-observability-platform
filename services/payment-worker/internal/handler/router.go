package handler

import (
	"log"
	"net/http"
	"time"

	"payment-worker/internal/telemetry"
)

// LoggingMiddleware logs incoming HTTP requests with method, path, and duration.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// NewRouter sets up routing for the payment-worker service.
func NewRouter(healthHandler *HealthHandler, paymentHandler *PaymentHandler) http.Handler {
	mux := http.NewServeMux()
	metrics := telemetry.New("payment-worker")

	mux.HandleFunc("GET /healthz", healthHandler.Healthz)
	mux.HandleFunc("POST /internal/process", paymentHandler.Process)
	mux.Handle("GET /metrics", metrics.Handler())

	return LoggingMiddleware(metrics.Middleware(mux))
}
