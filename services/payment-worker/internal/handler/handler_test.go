package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"payment-worker/internal/model"
)

type mockPaymentService struct {
	processPaymentFunc func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error)
}

func (m *mockPaymentService) ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
	return m.processPaymentFunc(ctx, req)
}

func TestHealthz(t *testing.T) {
	hh := NewHealthHandler("payment-worker", "1")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	hh.Healthz(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" || resp["service"] != "payment-worker" || resp["sprint"] != "1" {
		t.Errorf("unexpected healthz response: %v", resp)
	}
}

func TestProcessSuccess(t *testing.T) {
	mockSvc := &mockPaymentService{
		processPaymentFunc: func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
			return &model.PaymentResponse{
				OrderID: req.OrderID,
				Status:  "PROCESSED",
				Message: "Payment authorized successfully",
			}, nil
		},
	}

	ph := NewPaymentHandler(mockSvc)
	body, _ := json.Marshal(model.PaymentRequest{
		OrderID: "test-ord-1",
		Amount:  50.0,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/process", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	ph.Process(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var resp model.PaymentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.OrderID != "test-ord-1" || resp.Status != "PROCESSED" {
		t.Errorf("unexpected payment response: %+v", resp)
	}
}

func TestProcessInvalidPayload(t *testing.T) {
	mockSvc := &mockPaymentService{}
	ph := NewPaymentHandler(mockSvc)

	req := httptest.NewRequest(http.MethodPost, "/internal/process", bytes.NewReader([]byte("not valid json")))
	rr := httptest.NewRecorder()

	ph.Process(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rr.Code)
	}
}

func TestProcessServiceError(t *testing.T) {
	mockSvc := &mockPaymentService{
		processPaymentFunc: func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
			return nil, errors.New("database failure")
		},
	}
	ph := NewPaymentHandler(mockSvc)

	body, _ := json.Marshal(model.PaymentRequest{
		OrderID: "test-ord-1",
		Amount:  50.0,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/process", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	ph.Process(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rr.Code)
	}
}
