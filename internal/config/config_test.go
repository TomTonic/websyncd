package config

import (
	"testing"
	"time"
)

func TestLoadFromEnvHeartbeatDefaults(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("ENABLE_HEARTBEAT", "")
	t.Setenv("HEARTBEAT_ADDR", "")
	t.Setenv("HEARTBEAT_INTERVAL", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.EnableHeartbeat {
		t.Fatalf("EnableHeartbeat = true, want false")
	}
	if cfg.HeartbeatAddr != ":8081" {
		t.Fatalf("HeartbeatAddr = %q, want %q", cfg.HeartbeatAddr, ":8081")
	}
	if cfg.HeartbeatInterval != 5*time.Minute {
		t.Fatalf("HeartbeatInterval = %s, want %s", cfg.HeartbeatInterval, 5*time.Minute)
	}
}

func TestLoadFromEnvHeartbeatOverrides(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("ENABLE_HEARTBEAT", "true")
	t.Setenv("HEARTBEAT_ADDR", "127.0.0.1:9090")
	t.Setenv("HEARTBEAT_INTERVAL", "30s")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if !cfg.EnableHeartbeat {
		t.Fatalf("EnableHeartbeat = false, want true")
	}
	if cfg.HeartbeatAddr != "127.0.0.1:9090" {
		t.Fatalf("HeartbeatAddr = %q, want %q", cfg.HeartbeatAddr, "127.0.0.1:9090")
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Fatalf("HeartbeatInterval = %s, want %s", cfg.HeartbeatInterval, 30*time.Second)
	}
}

