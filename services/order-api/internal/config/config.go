package config

import (
	"os"
	"time"
)

// Config holds the application configuration parameters.
type Config struct {
	Port             string
	PaymentWorkerURL string
	PaymentTimeout   time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	ShutdownTimeout  time.Duration
	Sprint           string
}

// Load loads configuration from environment variables with sensible defaults.
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	paymentWorkerURL := os.Getenv("PAYMENT_WORKER_URL")
	if paymentWorkerURL == "" {
		paymentWorkerURL = "http://127.0.0.1:8001"
	}

	paymentTimeout := 5 * time.Second
	if val := os.Getenv("PAYMENT_TIMEOUT"); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			paymentTimeout = d
		}
	}

	return &Config{
		Port:             port,
		PaymentWorkerURL: paymentWorkerURL,
		PaymentTimeout:   paymentTimeout,
		ReadTimeout:      10 * time.Second,
		WriteTimeout:     15 * time.Second,
		ShutdownTimeout:  5 * time.Second,
		Sprint:           "1",
	}
}
