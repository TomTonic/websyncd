package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	ResourceURL       string
	OutputPath        string
	PollInterval      time.Duration
	HTTPTimeout       time.Duration
	LockTTL           time.Duration
	HeartbeatInterval time.Duration
	EnableWebhook     bool
	WebhookAddr       string
	EnableSSE         bool
	SSEURL            string
	EnableHTTP3       bool
	EnableHeartbeat   bool
	HeartbeatAddr     string
}

func LoadFromEnv() (Config, error) {
	cfg := Config{
		ResourceURL:       os.Getenv("RESOURCE_URL"),
		OutputPath:        os.Getenv("OUTPUT_PATH"),
		PollInterval:      envDuration("POLL_INTERVAL", time.Minute),
		HTTPTimeout:       envDuration("HTTP_TIMEOUT", 30*time.Second),
		LockTTL:           envDuration("LOCK_TTL", 5*time.Minute),
		HeartbeatInterval: envDuration("HEARTBEAT_INTERVAL", 5*time.Minute),
		EnableWebhook:     envBool("ENABLE_WEBHOOK", false),
		WebhookAddr:       envString("WEBHOOK_ADDR", ":8080"),
		EnableSSE:         envBool("ENABLE_SSE", false),
		SSEURL:            os.Getenv("SSE_URL"),
		EnableHTTP3:       envBool("ENABLE_HTTP3", false),
		EnableHeartbeat:   envBool("ENABLE_HEARTBEAT", false),
		HeartbeatAddr:     envString("HEARTBEAT_ADDR", ":8081"),
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

func envBool(name string, fallback bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
