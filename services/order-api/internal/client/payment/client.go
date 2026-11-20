package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"order-api/internal/model"
	"order-api/internal/telemetry"
)

var (
	// ErrUnreachable is returned when the payment-worker cannot be reached.
	ErrUnreachable = errors.New("payment-worker unreachable")
	// ErrInvalidResponse is returned when payment-worker violates its response contract.
	ErrInvalidResponse = errors.New("invalid response from payment-worker")
)

// UnreachableError wraps the underlying network/HTTP error.
type UnreachableError struct {
	Err error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("payment-worker unreachable: %v", e.Err)
}

func (e *UnreachableError) Unwrap() error {
	return e.Err
}

func (e *UnreachableError) Is(target error) bool {
	return target == ErrUnreachable
}

// Client defines the interface for communicating with the payment service.
type Client interface {
	ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error)
}

// HTTPClient implements Client using net/http.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new payment service HTTP client.
func NewClient(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// NewClientWithHTTPClient creates a client with a custom http.Client (e.g. for testing).
func NewClientWithHTTPClient(baseURL string, client *http.Client) *HTTPClient {
	return &HTTPClient{
		baseURL:    baseURL,
		httpClient: client,
		timeout:    client.Timeout,
	}
}

// ProcessPayment calls payment-worker to process an order payment.
func (c *HTTPClient) ProcessPayment(ctx context.Context, req model.PaymentRequest) (*model.PaymentResponse, error) {
	ctx, span := telemetry.Tracer(ctx, "order-api/payment").Start(ctx, "POST /internal/process",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", http.MethodPost), attribute.String("http.route", "/internal/process")),
	)
	defer span.End()
	reqBody, err := json.Marshal(req)
	if err != nil {
		telemetry.RecordFailure(span, "invalid_payment_request")
		return nil, fmt.Errorf("failed to marshal payment request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/process", bytes.NewReader(reqBody))
	if err != nil {
		telemetry.RecordFailure(span, "invalid_payment_endpoint")
		return nil, fmt.Errorf("failed to create payment request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	telemetry.InjectTraceContext(ctx, httpReq.Header)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		telemetry.RecordFailure(span, "dependency_"+telemetry.NetworkErrorKind(err))
		return nil, &UnreachableError{Err: err}
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		telemetry.RecordFailure(span, "dependency_http_error")
		return nil, ErrInvalidResponse
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		telemetry.RecordFailure(span, "dependency_"+telemetry.NetworkErrorKind(err))
		return nil, &UnreachableError{Err: err}
	}
	var paymentResp model.PaymentResponse
	if err := json.Unmarshal(body, &paymentResp); err != nil {
		telemetry.RecordFailure(span, "invalid_dependency_response")
		return nil, ErrInvalidResponse
	}
	if paymentResp.OrderID != req.OrderID || paymentResp.Status != "PROCESSED" {
		telemetry.RecordFailure(span, "invalid_dependency_response")
		return nil, ErrInvalidResponse
	}

	return &paymentResp, nil
}
