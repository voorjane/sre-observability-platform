package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"order-api/internal/client/payment"
	"order-api/internal/model"
)

type mockOrderService struct {
	createOrderFunc func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error)
}

func (m *mockOrderService) CreateOrder(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
	return m.createOrderFunc(ctx, req)
}

func TestHealthz(t *testing.T) {
	hh := NewHealthHandler("order-api", "1")
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

	if resp["status"] != "ok" || resp["service"] != "order-api" || resp["sprint"] != "1" {
		t.Errorf("unexpected healthz response: %v", resp)
	}
}

func TestCreateOrderSuccess(t *testing.T) {
	mockSvc := &mockOrderService{
		createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
			return &model.OrderResponse{
				OrderID:       "ord-12345",
				Status:        "COMPLETED",
				PaymentStatus: "PROCESSED",
				CreatedAt:     time.Now().UTC(),
			}, nil
		},
	}

	oh := NewOrderHandler(mockSvc)
	body, _ := json.Marshal(model.OrderRequest{
		ItemID:     "item-123",
		Amount:     99.99,
		CustomerID: "cust-456",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", rr.Code)
	}

	var resp model.OrderResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.OrderID != "ord-12345" || resp.Status != "COMPLETED" || resp.PaymentStatus != "PROCESSED" {
		t.Errorf("unexpected order response: %+v", resp)
	}
}

func TestCreateOrderInvalidPayload(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		message string
	}{
		{"invalid JSON", "invalid json", "invalid request payload"},
		{"empty body", "", "invalid request payload"},
		{"wrong type", `{"item_id":"item-1","amount":"10","customer_id":"cust-1"}`, "invalid request payload"},
		{"array", `[]`, "invalid request payload"},
		{"trailing garbage", `{"item_id":"item-1","amount":10,"customer_id":"cust-1"} garbage`, "invalid request payload"},
		{"multiple values", `{"item_id":"item-1","amount":10,"customer_id":"cust-1"} {}`, "invalid request payload"},
		{"null", `null`, "item_id is required"},
		{"missing fields", `{}`, "item_id is required"},
		{"blank item", `{"item_id":" \t","amount":10,"customer_id":"cust-1"}`, "item_id is required"},
		{"zero amount", `{"item_id":"item-1","amount":0,"customer_id":"cust-1"}`, "amount must be greater than zero"},
		{"negative amount", `{"item_id":"item-1","amount":-10,"customer_id":"cust-1"}`, "amount must be greater than zero"},
		{"blank customer", `{"item_id":"item-1","amount":10,"customer_id":" "}`, "customer_id is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			mockSvc := &mockOrderService{createOrderFunc: func(context.Context, model.OrderRequest) (*model.OrderResponse, error) {
				called = true
				return nil, errors.New("invalid order reached service")
			}}
			// Invalid requests must remain 400 even when chaos would fail a valid order.
			req := httptest.NewRequest(http.MethodPost, "/api/v1/orders?fail_rate=1.0", bytes.NewBufferString(tt.body))
			rr := httptest.NewRecorder()
			NewOrderHandler(mockSvc).CreateOrder(rr, req)
			assertErrorResponse(t, rr, http.StatusBadRequest, tt.message)
			if called {
				t.Fatal("invalid request called the order service")
			}
		})
	}
}

func TestCreateOrderPaymentWorkerUnreachable(t *testing.T) {
	mockSvc := &mockOrderService{
		createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
			return nil, &payment.UnreachableError{Err: errors.New("Post \"http://payment-worker:8001/internal/process\": connection refused\n")}
		},
	}
	oh := NewOrderHandler(mockSvc)

	body, _ := json.Marshal(model.OrderRequest{
		ItemID:     "item-1",
		Amount:     10.0,
		CustomerID: "cust-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	assertErrorResponse(t, rr, http.StatusServiceUnavailable, "payment-worker unreachable")
}

func TestCreateOrderPaymentWorkerInvalidResponse(t *testing.T) {
	mockSvc := &mockOrderService{
		createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
			return nil, fmt.Errorf("process payment: %w", payment.ErrInvalidResponse)
		},
	}
	oh := NewOrderHandler(mockSvc)

	body, _ := json.Marshal(model.OrderRequest{
		ItemID:     "item-1",
		Amount:     10.0,
		CustomerID: "cust-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(body))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	assertErrorResponse(t, rr, http.StatusBadGateway, "invalid response from payment-worker")
}

func TestCreateOrderChaosFailure(t *testing.T) {
	mockSvc := &mockOrderService{}
	oh := NewOrderHandler(mockSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders?fail_rate=1.0", bytes.NewBufferString(`{"item_id":"item-1","amount":10,"customer_id":"cust-1"}`))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	assertErrorResponse(t, rr, http.StatusInternalServerError, "chaos injected failure")
}

func TestCreateOrderServiceError(t *testing.T) {
	mockSvc := &mockOrderService{createOrderFunc: func(context.Context, model.OrderRequest) (*model.OrderResponse, error) {
		return nil, errors.New("internal \"failure\"\nwith details")
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewBufferString(`{"item_id":"item-1","amount":10,"customer_id":"cust-1"}`))
	rr := httptest.NewRecorder()
	NewOrderHandler(mockSvc).CreateOrder(rr, req)
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
