package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"order-api/internal/client/payment"
	"order-api/internal/model"
	"order-api/internal/service"
)

// OrderHandler handles HTTP requests related to orders.
type OrderHandler struct {
	orderService service.OrderService
}

// NewOrderHandler creates a new OrderHandler.
func NewOrderHandler(orderService service.OrderService) *OrderHandler {
	return &OrderHandler{
		orderService: orderService,
	}
}

// CreateOrder handles POST /api/v1/orders.
func (h *OrderHandler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	// Chaos simulation query parameters
	if latencyStr := r.URL.Query().Get("latency_ms"); latencyStr != "" {
		if ms, err := strconv.Atoi(latencyStr); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}

	if failRateStr := r.URL.Query().Get("fail_rate"); failRateStr != "" {
		if rate, err := strconv.ParseFloat(failRateStr, 64); err == nil {
			if rand.Float64() < rate {
				http.Error(w, `{"error":"chaos injected failure"}`, http.StatusInternalServerError)
				return
			}
		}
	}

	var req model.OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request payload"}`, http.StatusBadRequest)
		return
	}

	orderResp, err := h.orderService.CreateOrder(r.Context(), req)
	if err != nil {
		var unreachableErr *payment.UnreachableError
		if errors.As(err, &unreachableErr) || errors.Is(err, payment.ErrUnreachable) {
			http.Error(w, fmt.Sprintf(`{"error":"payment-worker unreachable: %v"}`, err), http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, payment.ErrInvalidResponse) {
			http.Error(w, `{"error":"invalid response from payment-worker"}`, http.StatusBadGateway)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(orderResp)
}
