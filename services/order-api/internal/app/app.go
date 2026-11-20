package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"order-api/internal/client/payment"
	"order-api/internal/config"
	"order-api/internal/handler"
	"order-api/internal/service"
	"order-api/internal/telemetry"
)

// App manages the lifecycle of the order-api application.
type App struct {
	cfg        *config.Config
	httpServer *http.Server
}

// New creates and configures a new App instance with all required dependencies.
func New(cfg *config.Config) *App {
	paymentClient := payment.NewClient(cfg.PaymentWorkerURL, cfg.PaymentTimeout)
	orderService := service.NewOrderService(paymentClient)

	healthHandler := handler.NewHealthHandler("order-api", cfg.Sprint)
	orderHandler := handler.NewOrderHandler(orderService)

	router := handler.NewRouter(healthHandler, orderHandler)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return &App{
		cfg:        cfg,
		httpServer: server,
	}
}

// Run starts the HTTP server and handles graceful shutdown when ctx is canceled.
func (a *App) Run(ctx context.Context) (runErr error) {
	provider, err := telemetry.NewTracing(ctx, "order-api", a.cfg.Tracing)
	if err != nil {
		return err
	}
	a.httpServer.Handler = telemetry.WithTracerProvider(provider, a.httpServer.Handler)
	defer func() {
		// HTTP drain and telemetry flush share the same total shutdown budget.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		defer cancel()
		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			_ = a.httpServer.Close()
			runErr = errors.Join(runErr, fmt.Errorf("order-api forced to shutdown: %w", err))
		}
		if err := provider.Shutdown(shutdownCtx); err != nil {
			log.Printf("order-api tracing shutdown: %v", err)
		}
		log.Println("order-api stopped")
	}()
	listener, err := net.Listen("tcp", a.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("http listener failed: %w", err)
	}
	_, startup := provider.Tracer("order-api/lifecycle").Start(ctx, "service.start")
	startup.End()
	serverErrCh := make(chan error, 1)

	go func() {
		log.Printf("[Sprint %s] order-api listening on :%s (payment-worker: %s)", a.cfg.Sprint, a.cfg.Port, a.cfg.PaymentWorkerURL)
		if err := a.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- fmt.Errorf("http server failed: %w", err)
		}
	}()

	select {
	case err := <-serverErrCh:
		return err
	case <-ctx.Done():
		log.Println("order-api shutting down gracefully...")
		return nil
	}
}
