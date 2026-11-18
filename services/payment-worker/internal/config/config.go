package config

import (
	"os"
	"time"
)

// Config holds the configuration parameters for payment-worker.
type Config struct {
	Port            string
	ProcessDelay    time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	Sprint          string
	Tracing         TracingConfig
}

// Load loads configuration from environment variables with sensible defaults.
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8001"
	}

	processDelay := 20 * time.Millisecond
	if val := os.Getenv("PROCESS_DELAY"); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			processDelay = d
		}
	}

	return &Config{
		Port:            port,
		ProcessDelay:    processDelay,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    15 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		Sprint:          "1",
		Tracing:         loadTracing(),
	}
}
