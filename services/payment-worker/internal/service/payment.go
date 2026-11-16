package service

import (
	"context"
	"time"

	"payment-worker/internal/model"
)

// PaymentService defines operations for payment processing.
type PaymentService interface {
	ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error)
}

type paymentService struct {
	delay time.Duration
}

// NewPaymentService creates a new PaymentService instance.
func NewPaymentService(delay time.Duration) PaymentService {
	return &paymentService{
		delay: delay,
	}
}

// ProcessPayment simulates banking authorization delay and returns the processed status.
func (s *paymentService) ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return &model.PaymentResponse{
		OrderID: req.OrderID,
		Status:  "PROCESSED",
		Message: "Payment authorized successfully",
	}, nil
}
