package handler

import (
	"errors"
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
	var req model.OrderRequest
	if err := decodeRequest(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request payload")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Chaos simulation query parameters
	if latencyStr := r.URL.Query().Get("latency_ms"); latencyStr != "" {
		if ms, err := strconv.Atoi(latencyStr); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}

	if failRateStr := r.URL.Query().Get("fail_rate"); failRateStr != "" {
		if rate, err := strconv.ParseFloat(failRateStr, 64); err == nil {
			if rand.Float64() < rate {
				writeError(w, http.StatusInternalServerError, "chaos injected failure")
				return
			}
		}
	}

	orderResp, err := h.orderService.CreateOrder(r.Context(), req)
	if err != nil {
		var unreachableErr *payment.UnreachableError
		if errors.As(err, &unreachableErr) || errors.Is(err, payment.ErrUnreachable) {
			writeError(w, http.StatusServiceUnavailable, "payment-worker unreachable")
			return
		}
		if errors.Is(err, payment.ErrInvalidResponse) {
			writeError(w, http.StatusBadGateway, "invalid response from payment-worker")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusCreated, orderResp)
}
