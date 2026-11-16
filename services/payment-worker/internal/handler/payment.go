package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"payment-worker/internal/model"
	"payment-worker/internal/service"
)

// PaymentHandler handles HTTP requests for payment processing.
type PaymentHandler struct {
	paymentService service.PaymentService
}

// NewPaymentHandler creates a new PaymentHandler.
func NewPaymentHandler(paymentService service.PaymentService) *PaymentHandler {
	return &PaymentHandler{
		paymentService: paymentService,
	}
}

// Process handles POST /internal/process.
func (h *PaymentHandler) Process(w http.ResponseWriter, r *http.Request) {
	var req model.PaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request payload"}`, http.StatusBadRequest)
		return
	}

	resp, err := h.paymentService.ProcessPayment(r.Context(), req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
