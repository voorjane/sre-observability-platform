package handler

import (
	"net/http"

	"go.uber.org/zap"

	"payment-worker/internal/telemetry"
)

// NewRouter sets up routing for the payment-worker service.
func NewRouter(healthHandler *HealthHandler, paymentHandler *PaymentHandler, logger *zap.Logger) http.Handler {
	mux := http.NewServeMux()
	metrics := telemetry.New("payment-worker")

	mux.HandleFunc("GET /healthz", healthHandler.Healthz)
	mux.Handle("POST /internal/process", telemetry.ServerHandler("/internal/process", http.HandlerFunc(paymentHandler.Process)))
	mux.Handle("GET /metrics", metrics.Handler())

	return telemetry.RequestLogging(logger, mux, metrics.Middleware(mux))
}
