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
	tests := []struct {
		name    string
		body    string
		message string
	}{
		{"invalid JSON", "not valid json", "invalid request payload"},
		{"empty body", "", "invalid request payload"},
		{"wrong type", `{"order_id":"ord-1","amount":"10"}`, "invalid request payload"},
		{"array", `[]`, "invalid request payload"},
		{"trailing garbage", `{"order_id":"ord-1","amount":10} garbage`, "invalid request payload"},
		{"multiple values", `{"order_id":"ord-1","amount":10} {}`, "invalid request payload"},
		{"null", `null`, "order_id is required"},
		{"missing fields", `{}`, "order_id is required"},
		{"blank order", `{"order_id":" \t","amount":10}`, "order_id is required"},
		{"zero amount", `{"order_id":"ord-1","amount":0}`, "amount must be greater than zero"},
		{"negative amount", `{"order_id":"ord-1","amount":-10}`, "amount must be greater than zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			mockSvc := &mockPaymentService{processPaymentFunc: func(context.Context, model.PaymentRequest) (*model.PaymentResponse, error) {
				called = true
				return nil, errors.New("invalid payment reached service")
			}}
			req := httptest.NewRequest(http.MethodPost, "/internal/process", bytes.NewBufferString(tt.body))
			rr := httptest.NewRecorder()
			NewPaymentHandler(mockSvc).Process(rr, req)
			assertErrorResponse(t, rr, http.StatusBadRequest, tt.message)
			if called {
				t.Fatal("invalid request called the payment service")
			}
		})
	}
}

func TestProcessServiceError(t *testing.T) {
	mockSvc := &mockPaymentService{
		processPaymentFunc: func(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
			return nil, errors.New("database \"failure\"\nwith details")
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

	assertErrorResponse(t, rr, http.StatusInternalServerError, "internal server error")
}

func assertErrorResponse(t *testing.T, rr *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected application/json, got %q", got)
	}
	var payload map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON error response: %v: %s", err, rr.Body.String())
	}
	if len(payload) != 1 || payload["error"] != message {
		t.Errorf("expected error %q, got %v", message, payload)
	}
}
