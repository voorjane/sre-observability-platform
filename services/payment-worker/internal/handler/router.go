package handler

import (
	"log"
	"net/http"
	"time"
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

	mux.HandleFunc("GET /healthz", healthHandler.Healthz)
	mux.HandleFunc("POST /internal/process", paymentHandler.Process)

	return LoggingMiddleware(mux)
}
