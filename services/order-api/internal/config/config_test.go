package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("PAYMENT_WORKER_URL", "")
	t.Setenv("PAYMENT_TIMEOUT", "")

	cfg := Load()

	if cfg.Port != "8000" {
		t.Errorf("expected port 8000, got %s", cfg.Port)
	}
	if cfg.PaymentWorkerURL != "http://127.0.0.1:8001" {
		t.Errorf("expected payment worker URL http://127.0.0.1:8001, got %s", cfg.PaymentWorkerURL)
	}
	if cfg.PaymentTimeout != 5*time.Second {
		t.Errorf("expected payment timeout 5s, got %v", cfg.PaymentTimeout)
	}
	if cfg.Sprint != "1" {
		t.Errorf("expected sprint 1, got %s", cfg.Sprint)
	}
}

func TestLoadCustom(t *testing.T) {
	t.Setenv("PORT", "9000")
	t.Setenv("PAYMENT_WORKER_URL", "http://custom-payment:9001")
	t.Setenv("PAYMENT_TIMEOUT", "3s")

	cfg := Load()

	if cfg.Port != "9000" {
		t.Errorf("expected port 9000, got %s", cfg.Port)
	}
	if cfg.PaymentWorkerURL != "http://custom-payment:9001" {
		t.Errorf("expected payment worker URL http://custom-payment:9001, got %s", cfg.PaymentWorkerURL)
	}
	if cfg.PaymentTimeout != 3*time.Second {
		t.Errorf("expected payment timeout 3s, got %v", cfg.PaymentTimeout)
	}
}
