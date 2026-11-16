package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("PORT")
	os.Unsetenv("PROCESS_DELAY")

	cfg := Load()

	if cfg.Port != "8001" {
		t.Errorf("expected port 8001, got %s", cfg.Port)
	}
	if cfg.ProcessDelay != 20*time.Millisecond {
		t.Errorf("expected process delay 20ms, got %v", cfg.ProcessDelay)
	}
	if cfg.Sprint != "1" {
		t.Errorf("expected sprint 1, got %s", cfg.Sprint)
	}
}

func TestLoadCustom(t *testing.T) {
	os.Setenv("PORT", "8888")
	os.Setenv("PROCESS_DELAY", "50ms")
	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("PROCESS_DELAY")
	}()

	cfg := Load()

	if cfg.Port != "8888" {
		t.Errorf("expected port 8888, got %s", cfg.Port)
	}
	if cfg.ProcessDelay != 50*time.Millisecond {
		t.Errorf("expected process delay 50ms, got %v", cfg.ProcessDelay)
	}
}
