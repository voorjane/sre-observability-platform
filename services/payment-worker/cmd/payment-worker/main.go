package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"payment-worker/internal/app"
	"payment-worker/internal/config"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := app.New(cfg)
	if err := application.Run(ctx); err != nil {
		log.Fatalf("payment-worker terminated with error: %v", err)
	}
}
