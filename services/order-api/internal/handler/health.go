package handler

import (
	"encoding/json"
	"net/http"
)

// HealthHandler handles health check requests.
type HealthHandler struct {
	serviceName string
	sprint      string
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler(serviceName, sprint string) *HealthHandler {
	return &HealthHandler{
		serviceName: serviceName,
		sprint:      sprint,
	}
}

// Healthz responds with service health status.
func (h *HealthHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": h.serviceName,
		"sprint":  h.sprint,
	})
}
