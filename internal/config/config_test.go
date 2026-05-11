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
// EnableHeartbeat is false and HeartbeatAddr is ":8081".
func TestLoadFromEnvHeartbeatDefaults(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("ENABLE_HEARTBEAT", "")
	t.Setenv("HEARTBEAT_ADDR", "")
	// HEARTBEAT_INTERVAL removed; no interval-based heartbeat logging anymore.

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
}

// TestLoadFromEnvHeartbeatOverrides verifies that users can fully configure the
// heartbeat endpoint address and interval through environment variables.
//
// This test covers the config package LoadFromEnv function.
//
// It sets ENABLE_HEARTBEAT=true with a custom address value and asserts that
// the field is reflected correctly in the returned Config.
func TestLoadFromEnvHeartbeatOverrides(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("ENABLE_HEARTBEAT", "true")
	t.Setenv("HEARTBEAT_ADDR", "127.0.0.1:9090")

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
}
