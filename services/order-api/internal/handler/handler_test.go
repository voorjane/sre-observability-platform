package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	mockSvc := &mockOrderService{}
	oh := NewOrderHandler(mockSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader([]byte("invalid json")))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rr.Code)
	}
}

func TestCreateOrderPaymentWorkerUnreachable(t *testing.T) {
	mockSvc := &mockOrderService{
		createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
			return nil, &payment.UnreachableError{Err: errors.New("connection refused")}
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

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rr.Code)
	}
}

func TestCreateOrderPaymentWorkerInvalidResponse(t *testing.T) {
	mockSvc := &mockOrderService{
		createOrderFunc: func(ctx context.Context, req model.OrderRequest) (*model.OrderResponse, error) {
			return nil, payment.ErrInvalidResponse
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

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", rr.Code)
	}
}

func TestCreateOrderChaosFailure(t *testing.T) {
	mockSvc := &mockOrderService{}
	oh := NewOrderHandler(mockSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders?fail_rate=1.0", bytes.NewReader([]byte("{}")))
	rr := httptest.NewRecorder()

	oh.CreateOrder(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500 on chaos fail_rate=1.0, got %d", rr.Code)
	}
}
