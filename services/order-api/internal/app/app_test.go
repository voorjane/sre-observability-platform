package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"order-api/internal/config"
	"order-api/internal/model"
)

// Exercise the real router, order service and payment HTTP client together.
func TestOrderPaymentHTTPFlow(t *testing.T) {
	var paymentCalls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paymentCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/internal/process" {
			t.Errorf("unexpected payment request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var req model.PaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("invalid payment payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if req.OrderID == "" || req.Amount != 49.99 {
			t.Errorf("unexpected payment payload: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(model.PaymentResponse{OrderID: req.OrderID, Status: "PROCESSED"})
	}))
	defer worker.Close()

	application := New(&config.Config{PaymentWorkerURL: worker.URL, PaymentTimeout: time.Second})
	orders := httptest.NewServer(application.httpServer.Handler)
	defer orders.Close()
	client := orders.Client()
	client.Timeout = 2 * time.Second
	postOrder := func(body string, status int) []byte {
		t.Helper()
		resp, err := client.Post(orders.URL+"/api/v1/orders", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("expected status %d, got %d", status, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected application/json, got %q", got)
		}
		var payload json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("invalid response JSON: %v", err)
		}
		return payload
	}

	for _, body := range []string{
		"not json",
		`{"item_id":"item-1","amount":0,"customer_id":"cust-1"}`,
		`{"item_id":"item-1","amount":49.99,"customer_id":" "}`,
	} {
		postOrder(body, http.StatusBadRequest)
	}
	if calls := paymentCalls.Load(); calls != 0 {
		t.Fatalf("invalid orders triggered %d payment requests", calls)
	}

	validOrder := `{"item_id":"item-1","amount":49.99,"customer_id":"cust-1"}`
	payload := postOrder(validOrder+" \n\t", http.StatusCreated)
	var order model.OrderResponse
	if err := json.Unmarshal(payload, &order); err != nil {
		t.Fatal(err)
	}
	if order.OrderID == "" || order.Status != "COMPLETED" || order.PaymentStatus != "PROCESSED" {
		t.Fatalf("unexpected order: %+v", order)
	}
	if calls := paymentCalls.Load(); calls != 1 {
		t.Fatalf("expected one payment request, got %d", calls)
	}

	worker.Close()
	payload = postOrder(validOrder, http.StatusServiceUnavailable)
	var failure map[string]string
	if err := json.Unmarshal(payload, &failure); err != nil {
		t.Fatal(err)
	}
	if failure["error"] != "payment-worker unreachable" {
		t.Errorf("unexpected error: %s", payload)
	}
}
