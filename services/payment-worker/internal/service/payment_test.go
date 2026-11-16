package service

import (
	"context"
	"testing"
	"time"

	"payment-worker/internal/model"
)

func TestProcessPaymentSuccess(t *testing.T) {
	svc := NewPaymentService(5 * time.Millisecond)

	resp, err := svc.ProcessPayment(context.Background(), model.PaymentRequest{
		OrderID: "test-ord-1",
		Amount:  123.45,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.OrderID != "test-ord-1" {
		t.Errorf("expected order id test-ord-1, got %s", resp.OrderID)
	}
	if resp.Status != "PROCESSED" {
		t.Errorf("expected status PROCESSED, got %s", resp.Status)
	}
	if resp.Message != "Payment authorized successfully" {
		t.Errorf("expected authorized message, got %s", resp.Message)
	}
}

func TestProcessPaymentContextCanceled(t *testing.T) {
	svc := NewPaymentService(100 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := svc.ProcessPayment(ctx, model.PaymentRequest{
		OrderID: "test-ord-1",
		Amount:  123.45,
	})

	if err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
}
