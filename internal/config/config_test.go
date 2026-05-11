package config

import (
	"testing"
)

// TestLoadFromEnvHeartbeatDefaults verifies that heartbeat monitoring is
// disabled by default so that users who have not opted in get no extra port
// bindings or log noise.
//
// This test covers the config package LoadFromEnv function.
//
// It clears all heartbeat-related environment variables and asserts that
// HeartbeatAddr is empty by default (no HTTP heartbeat endpoint started).
func TestLoadFromEnvHeartbeatDefaults(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("HEARTBEAT_ADDR", "")
	// HEARTBEAT_INTERVAL removed; no interval-based heartbeat logging anymore.

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.HeartbeatAddr != "" {
		t.Fatalf("HeartbeatAddr = %q, want empty", cfg.HeartbeatAddr)
	}
}

// TestLoadFromEnvHeartbeatOverrides verifies that users can fully configure the
// heartbeat endpoint address and interval through environment variables.
//
// This test covers the config package LoadFromEnv function.
//
// It sets HEARTBEAT_ADDR and asserts that the field is reflected correctly
// in the returned Config.
func TestLoadFromEnvHeartbeatOverrides(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("HEARTBEAT_ADDR", "127.0.0.1:9090")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.HeartbeatAddr != "127.0.0.1:9090" {
		t.Fatalf("HeartbeatAddr = %q, want %q", cfg.HeartbeatAddr, "127.0.0.1:9090")
	}
}
