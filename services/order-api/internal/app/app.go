package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"go.uber.org/zap"

	"order-api/internal/client/payment"
	"order-api/internal/config"
	"order-api/internal/handler"
	"order-api/internal/service"
	"order-api/internal/telemetry"
)

// App manages the lifecycle of the order-api application.
type App struct {
	logger     *zap.Logger
	cfg        *config.Config
	httpServer *http.Server
}

// New creates and configures a new App instance with all required dependencies.
func New(cfg *config.Config) *App {
	return NewWithLogger(cfg, telemetry.NewLogger("order-api"))
}

// NewWithLogger configures the application with a process-scoped logger.
func NewWithLogger(cfg *config.Config, logger *zap.Logger) *App {
	paymentClient := payment.NewClient(cfg.PaymentWorkerURL, cfg.PaymentTimeout)
	orderService := service.NewOrderService(paymentClient)

	healthHandler := handler.NewHealthHandler("order-api", cfg.Sprint)
	orderHandler := handler.NewOrderHandler(orderService)

	router := handler.NewRouter(healthHandler, orderHandler, logger)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		ErrorLog:     telemetry.HTTPErrorLogger(logger),
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return &App{
		logger:     logger,
		cfg:        cfg,
		httpServer: server,
	}
}

// Run starts the HTTP server and handles graceful shutdown when ctx is canceled.
func (a *App) Run(ctx context.Context) (runErr error) {
	provider, err := telemetry.NewTracing(ctx, "order-api", a.cfg.Tracing)
	if err != nil {
		a.logger.Error("tracing initialization failed", zap.String("event", "startup_failed"), zap.String("error.type", "tracing_init_failed"))
		return err
	}
	a.httpServer.Handler = telemetry.WithTracerProvider(provider, a.httpServer.Handler)
	defer func() {
		a.logger.Info("service shutting down", zap.String("event", "service_stopping"))
		// HTTP drain and telemetry flush share the same total shutdown budget.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		defer cancel()
		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			a.logger.Warn("http shutdown deadline exceeded", zap.String("event", "shutdown_error"), zap.String("error.type", "http_shutdown_failed"))
			_ = a.httpServer.Close()
			runErr = errors.Join(runErr, fmt.Errorf("order-api forced to shutdown: %w", err))
		}
		if err := provider.Shutdown(shutdownCtx); err != nil {
			a.logger.Warn("tracing shutdown failed", zap.String("event", "shutdown_error"), zap.String("error.type", "tracing_shutdown_failed"))
		}
		a.logger.Info("service stopped", zap.String("event", "service_stopped"))
	}()
	listener, err := net.Listen("tcp", a.httpServer.Addr)
	if err != nil {
		a.logger.Error("http listener failed", zap.String("event", "startup_failed"), zap.String("error.type", "http_listen_failed"))
		return fmt.Errorf("http listener failed: %w", err)
	}
	_, startup := provider.Tracer("order-api/lifecycle").Start(ctx, "service.start")
	startup.End()
	serverErrCh := make(chan error, 1)

	a.logger.Info("service listening", zap.String("event", "service_started"), zap.String("listen_addr", listener.Addr().String()))
	go func() {
		if err := a.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("http server failed", zap.String("event", "service_error"), zap.String("error.type", "http_serve_failed"))
			serverErrCh <- fmt.Errorf("http server failed: %w", err)
		}
	}()

	select {
	case err := <-serverErrCh:
		return err
	case <-ctx.Done():
		return nil
	}
}
