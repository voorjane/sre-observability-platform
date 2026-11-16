package service

import (
	"context"
	"errors"
	"testing"

	"order-api/internal/model"
)

type mockPaymentClient struct {
	processPaymentFunc func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error)
}

func (m *mockPaymentClient) ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
	return m.processPaymentFunc(ctx, req)
}

func TestCreateOrderSuccess(t *testing.T) {
	mockClient := &mockPaymentClient{
		processPaymentFunc: func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
			if req.OrderID != "ord-static-id" {
				t.Errorf("unexpected order id: %s", req.OrderID)
			}
			if req.Amount != 100.50 {
				t.Errorf("unexpected amount: %f", req.Amount)
			}
			return &model.PaymentResponse{
				OrderID: req.OrderID,
				Status:  "PROCESSED",
				Message: "Payment authorized successfully",
			}, nil
		},
	}

	svc := NewOrderServiceWithIDGenerator(mockClient, func() string {
		return "ord-static-id"
	})

	resp, err := svc.CreateOrder(context.Background(), model.OrderRequest{
		ItemID:     "item-1",
		Amount:     100.50,
		CustomerID: "cust-1",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.OrderID != "ord-static-id" {
		t.Errorf("expected order id ord-static-id, got %s", resp.OrderID)
	}
	if resp.Status != "COMPLETED" {
		t.Errorf("expected status COMPLETED, got %s", resp.Status)
	}
	if resp.PaymentStatus != "PROCESSED" {
		t.Errorf("expected payment status PROCESSED, got %s", resp.PaymentStatus)
	}
}

func TestCreateOrderPaymentFailure(t *testing.T) {
	expectedErr := errors.New("network failure")
	mockClient := &mockPaymentClient{
		processPaymentFunc: func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
			return nil, expectedErr
		},
	}

	svc := NewOrderServiceWithIDGenerator(mockClient, func() string {
		return "ord-static-id"
	})

	_, err := svc.CreateOrder(context.Background(), model.OrderRequest{
		ItemID:     "item-1",
		Amount:     100.50,
		CustomerID: "cust-1",
	})

	if !errors.Is(err, expectedErr) {
		t.Errorf("expected %v, got %v", expectedErr, err)
	}
}
