package handler

import (
	"log"
	"net/http"
	"time"

	"order-api/internal/telemetry"
)

// LoggingMiddleware logs incoming HTTP requests with method, path, and duration.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// NewRouter sets up routing for the order-api service.
func NewRouter(healthHandler *HealthHandler, orderHandler *OrderHandler) http.Handler {
	mux := http.NewServeMux()
	metrics := telemetry.New("order-api")

	mux.HandleFunc("GET /healthz", healthHandler.Healthz)
	mux.Handle("POST /api/v1/orders", telemetry.ServerHandler("/api/v1/orders", http.HandlerFunc(orderHandler.CreateOrder)))
	mux.Handle("GET /metrics", metrics.Handler())

	return LoggingMiddleware(metrics.Middleware(mux))
}
