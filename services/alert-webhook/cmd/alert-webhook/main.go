package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"alert-webhook/internal/receiver"
)

func main() {
	address := flag.String("listen", ":8080", "HTTP listen address")
	data := flag.String("data-dir", "/data", "Notification journal directory")
	runbooks := flag.String("runbooks-dir", "/runbooks", "Read-only runbook directory")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	store, err := receiver.Open(*data)
	if err != nil {
		slog.Error("open notification journal", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	server := &http.Server{Addr: *address, Handler: receiver.Handler(store, *runbooks),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Error("HTTP shutdown", "error", err)
		}
	}()
	slog.Info("notification receiver started", "address", *address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server", "error", err)
		os.Exit(1)
	}
	<-stopped
}
