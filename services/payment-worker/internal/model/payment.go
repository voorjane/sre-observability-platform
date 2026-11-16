package model

import (
	"errors"
	"strings"
)

// PaymentRequest represents the payload for processing a payment.
type PaymentRequest struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

// Validate validates the payment request fields.
func (r *PaymentRequest) Validate() error {
	if strings.TrimSpace(r.OrderID) == "" {
		return errors.New("order_id is required")
	}
	if r.Amount <= 0 {
		return errors.New("amount must be greater than zero")
	}
	return nil
}

// PaymentResponse represents the response returned after payment processing.
type PaymentResponse struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
