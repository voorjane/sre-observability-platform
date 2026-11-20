package service

import (
	"context"
	"time"

	"payment-worker/internal/model"
	"payment-worker/internal/telemetry"
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
	ctx, span := telemetry.Tracer(ctx, "payment-worker/service").Start(ctx, "payment.process")
	defer span.End()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			telemetry.RecordFailure(span, telemetry.NetworkErrorKind(ctx.Err()))
			return nil, ctx.Err()
		}
	}

	return &model.PaymentResponse{
		OrderID: req.OrderID,
		Status:  "PROCESSED",
		Message: "Payment authorized successfully",
	}, nil
}
