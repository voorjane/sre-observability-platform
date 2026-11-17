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
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	client := NewClient(server.URL, time.Second)
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
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"bad JSON", http.StatusOK, "not-a-valid-json"},
		{"empty body", http.StatusOK, ""},
		{"null", http.StatusOK, `null`},
		{"missing fields", http.StatusOK, `{}`},
		{"different order", http.StatusOK, `{"order_id":"other-order","status":"PROCESSED"}`},
		{"unexpected payment status", http.StatusOK, `{"order_id":"test-ord-123","status":"FAILED"}`},
		{"trailing garbage", http.StatusOK, `{"order_id":"test-ord-123","status":"PROCESSED"} garbage`},
		{"multiple values", http.StatusOK, `{"order_id":"test-ord-123","status":"PROCESSED"} {}`},
		{"worker client error", http.StatusBadRequest, `{"error":"invalid request payload"}`},
		{"worker internal error", http.StatusInternalServerError, `{"error":"internal server error"}`},
		{"worker unavailable response", http.StatusServiceUnavailable, `{"error":"unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := NewClient(server.URL, time.Second)
			_, err := client.ProcessPayment(context.Background(), model.PaymentRequest{OrderID: "test-ord-123", Amount: 49.99})
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("expected ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestProcessPaymentTimeout(t *testing.T) {
	for _, phase := range []string{"before headers", "during body"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "during body" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"order_id":`))
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer server.Close()
			defer close(release)

			client := NewClient(server.URL, 50*time.Millisecond)
			_, err := client.ProcessPayment(context.Background(), model.PaymentRequest{OrderID: "test-ord-123", Amount: 49.99})
			if !errors.Is(err, ErrUnreachable) {
				t.Errorf("expected ErrUnreachable on timeout, got %v", err)
			}
		})
	}
}
