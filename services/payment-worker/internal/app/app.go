package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"payment-worker/internal/config"
	"payment-worker/internal/handler"
	"payment-worker/internal/service"
	"payment-worker/internal/telemetry"
)

// App manages the lifecycle of the payment-worker application.
type App struct {
	cfg        *config.Config
	httpServer *http.Server
}

// New creates and configures a new App instance with all required dependencies.
func New(cfg *config.Config) *App {
	paymentService := service.NewPaymentService(cfg.ProcessDelay)

	healthHandler := handler.NewHealthHandler("payment-worker", cfg.Sprint)
	paymentHandler := handler.NewPaymentHandler(paymentService)

	router := handler.NewRouter(healthHandler, paymentHandler)

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
	provider, err := telemetry.NewTracing(ctx, "payment-worker", a.cfg.Tracing)
	if err != nil {
		return err
	}
	defer func() {
		// HTTP drain and telemetry flush share the same total shutdown budget.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		defer cancel()
		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			_ = a.httpServer.Close()
			runErr = errors.Join(runErr, fmt.Errorf("payment-worker forced to shutdown: %w", err))
		}
		if err := provider.Shutdown(shutdownCtx); err != nil {
			log.Printf("payment-worker tracing shutdown: %v", err)
		}
		log.Println("payment-worker stopped")
	}()
	listener, err := net.Listen("tcp", a.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("http listener failed: %w", err)
	}
	_, startup := provider.Tracer("payment-worker/lifecycle").Start(ctx, "service.start")
	startup.End()
	serverErrCh := make(chan error, 1)

	go func() {
		log.Printf("[Sprint %s] payment-worker listening on :%s", a.cfg.Sprint, a.cfg.Port)
		if err := a.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- fmt.Errorf("http server failed: %w", err)
		}
	}()

	select {
	case err := <-serverErrCh:
		return err
	case <-ctx.Done():
		log.Println("payment-worker shutting down gracefully...")
		return nil
	}
}
