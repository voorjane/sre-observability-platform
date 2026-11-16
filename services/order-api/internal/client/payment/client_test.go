package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"order-api/internal/model"
)

func TestProcessPaymentSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/process" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		var req model.PaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		resp := model.PaymentResponse{
			OrderID: req.OrderID,
			Status:  "PROCESSED",
			Message: "Payment authorized successfully",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, 2*time.Second)
	resp, err := client.ProcessPayment(context.Background(), model.PaymentRequest{
		OrderID: "test-ord-123",
		Amount:  49.99,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.OrderID != "test-ord-123" {
		t.Errorf("expected order ID test-ord-123, got %s", resp.OrderID)
	}
	if resp.Status != "PROCESSED" {
		t.Errorf("expected status PROCESSED, got %s", resp.Status)
	}
}

func TestProcessPaymentUnreachable(t *testing.T) {
	// Connect to an invalid address
	client := NewClient("http://127.0.0.1:54321", 50*time.Millisecond)
	_, err := client.ProcessPayment(context.Background(), model.PaymentRequest{
		OrderID: "test-ord-123",
		Amount:  49.99,
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("expected ErrUnreachable, got %v", err)
	}
}

func TestProcessPaymentInvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-a-valid-json"))
	}))
	defer server.Close()

	client := NewClient(server.URL, 2*time.Second)
	_, err := client.ProcessPayment(context.Background(), model.PaymentRequest{
		OrderID: "test-ord-123",
		Amount:  49.99,
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("expected ErrInvalidResponse, got %v", err)
	}
}
