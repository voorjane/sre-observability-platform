package handler

import (
	"net/http"

	"payment-worker/internal/model"
	"payment-worker/internal/service"
	"payment-worker/internal/telemetry"
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
	if err := decodeRequest(r.Body, &req); err != nil {
		telemetry.SetRequestError(r.Context(), "invalid_request_payload")
		writeError(w, http.StatusBadRequest, "invalid request payload")
		return
	}
	if err := req.Validate(); err != nil {
		telemetry.SetRequestError(r.Context(), "invalid_request_fields")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.paymentService.ProcessPayment(r.Context(), req)
	if err != nil {
		category := "internal_error"
		if r.Context().Err() != nil {
			category = telemetry.NetworkErrorKind(err)
		}
		telemetry.SetRequestError(r.Context(), category)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
