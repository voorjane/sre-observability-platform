package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"payment-worker/internal/app"
	"payment-worker/internal/config"
	"payment-worker/internal/telemetry"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := telemetry.NewLogger("payment-worker")
	defer func() { _ = logger.Sync() }()
	telemetry.InstallErrorHandler(logger)
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := app.NewWithLogger(cfg, logger)
	if err := application.Run(ctx); err != nil {
		logger.Error("application terminated", zap.String("event", "application_failed"), zap.String("error.type", "application_failed"))
		return 1
	}
	return 0
}
