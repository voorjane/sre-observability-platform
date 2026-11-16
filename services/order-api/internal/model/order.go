package model

import (
	"errors"
	"strings"
	"time"
)

// OrderRequest represents the incoming payload for creating an order.
type OrderRequest struct {
	ItemID     string  `json:"item_id"`
	Amount     float64 `json:"amount"`
	CustomerID string  `json:"customer_id"`
}

// Validate validates the order request fields.
func (r *OrderRequest) Validate() error {
	if strings.TrimSpace(r.ItemID) == "" {
		return errors.New("item_id is required")
	}
	if r.Amount <= 0 {
		return errors.New("amount must be greater than zero")
	}
	if strings.TrimSpace(r.CustomerID) == "" {
		return errors.New("customer_id is required")
	}
	return nil
}

// OrderResponse represents the response sent after order creation.
type OrderResponse struct {
	OrderID       string    `json:"order_id"`
	Status        string    `json:"status"`
	PaymentStatus string    `json:"payment_status"`
	CreatedAt     time.Time `json:"created_at"`
}
