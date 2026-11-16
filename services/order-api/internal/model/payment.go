package model

// PaymentRequest represents the payload sent to the payment worker.
type PaymentRequest struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

// PaymentResponse represents the response received from the payment worker.
type PaymentResponse struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
