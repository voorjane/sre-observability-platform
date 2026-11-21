package handler

import (
	"net/http"

	"go.uber.org/zap"

	"order-api/internal/telemetry"
)

// NewRouter sets up routing for the order-api service.
func NewRouter(healthHandler *HealthHandler, orderHandler *OrderHandler, logger *zap.Logger) http.Handler {
	mux := http.NewServeMux()
	metrics := telemetry.New("order-api")

	mux.HandleFunc("GET /healthz", healthHandler.Healthz)
	mux.Handle("POST /api/v1/orders", telemetry.ServerHandler("/api/v1/orders", http.HandlerFunc(orderHandler.CreateOrder)))
	mux.Handle("GET /metrics", metrics.Handler())

	return telemetry.RequestLogging(logger, mux, metrics.Middleware(mux))
}
