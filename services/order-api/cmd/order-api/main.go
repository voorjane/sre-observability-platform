package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"order-api/internal/app"
	"order-api/internal/config"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := app.New(cfg)
	if err := application.Run(ctx); err != nil {
		log.Fatalf("order-api terminated with error: %v", err)
	}
}
