package service

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"order-api/internal/client/payment"
	"order-api/internal/model"
)

// IDGenerator generates unique order identifiers.
type IDGenerator func() string

func defaultIDGenerator() string {
	return fmt.Sprintf("ord-%d-%d", time.Now().UnixNano(), rand.Intn(10000))
}

// OrderService defines the business logic operations for orders.
type OrderService interface {
	CreateOrder(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error)
}

type orderService struct {
	paymentClient payment.Client
	generateID    IDGenerator
}

// NewOrderService creates a new OrderService instance.
func NewOrderService(paymentClient payment.Client) OrderService {
	return &orderService{
		paymentClient: paymentClient,
		generateID:    defaultIDGenerator,
	}
}

// NewOrderServiceWithIDGenerator creates a new OrderService with a custom ID generator (useful for testing).
func NewOrderServiceWithIDGenerator(paymentClient payment.Client, gen IDGenerator) OrderService {
	return &orderService{
		paymentClient: paymentClient,
		generateID:    gen,
	}
}

// CreateOrder generates an order, delegates payment to payment-worker, and produces the finalized order response.
func (s *orderService) CreateOrder(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
	orderID := s.generateID()

	paymentReq := model.PaymentRequest{
		OrderID: orderID,
		Amount:  req.Amount,
	}

	paymentResp, err := s.paymentClient.ProcessPayment(ctx, paymentReq)
	if err != nil {
		return nil, err
	}

	return &model.OrderResponse{
		OrderID:       orderID,
		Status:        "COMPLETED",
		PaymentStatus: paymentResp.Status,
		CreatedAt:     time.Now().UTC(),
	}, nil
}
