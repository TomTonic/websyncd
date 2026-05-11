// Package config loads and validates websyncd runtime configuration from
// environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds the runtime configuration for websyncd, populated from environment
// variables via LoadFromEnv. All fields are read-only after construction; do not
// mutate a Config that has been passed to app.Run.
type Config struct {
	ResourceURL     string
	OutputPath      string
	PollInterval    time.Duration
	HTTPTimeout     time.Duration
	LockTTL         time.Duration
	EnableWebhook   bool
	WebhookAddr     string
	EnableSSE       bool
	SSEURL          string
	EnableHTTP3     bool
	EnableHeartbeat bool
	HeartbeatAddr   string
}

// LoadFromEnv constructs a Config by reading well-known environment variables.
//
// Required variables: RESOURCE_URL and OUTPUT_PATH. Optional variables and their
// defaults are documented in the project README. When ENABLE_SSE=true, SSE_URL
// must also be set.
//
// Returns a fully populated Config on success, or an error describing the first
// missing or invalid required variable.
//
// Typical call site is app.LoadConfigFromEnv, which delegates here and is the
// recommended entry point from main.
func LoadFromEnv() (Config, error) {
	cfg := Config{
		ResourceURL:     os.Getenv("RESOURCE_URL"),
		OutputPath:      os.Getenv("OUTPUT_PATH"),
		PollInterval:    envDuration("POLL_INTERVAL", 30*time.Minute),
		HTTPTimeout:     envDuration("HTTP_TIMEOUT", 30*time.Second),
		LockTTL:         envDuration("LOCK_TTL", 5*time.Minute),
		EnableWebhook:   envBool("ENABLE_WEBHOOK"),
		WebhookAddr:     envString("WEBHOOK_ADDR", ":8080"),
		EnableSSE:       envBool("ENABLE_SSE"),
		SSEURL:          os.Getenv("SSE_URL"),
		EnableHTTP3:     envBool("ENABLE_HTTP3"),
		EnableHeartbeat: envBool("ENABLE_HEARTBEAT"),
		HeartbeatAddr:   envString("HEARTBEAT_ADDR", ":8081"),
	}

	if cfg.ResourceURL == "" {
		return Config{}, fmt.Errorf("RESOURCE_URL is required")
	}
	if cfg.OutputPath == "" {
		return Config{}, fmt.Errorf("OUTPUT_PATH is required")
	}
	if cfg.EnableSSE && cfg.SSEURL == "" {
		return Config{}, fmt.Errorf("SSE_URL is required when ENABLE_SSE=true")
	}

	return cfg, nil
}

func envString(name, fallback string) string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	if d <= 0 {
		return fallback
	}
	return d
}

func envBool(name string) bool {
	v := os.Getenv(name)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}
