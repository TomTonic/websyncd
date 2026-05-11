// Package config loads and validates websyncd runtime configuration from
// environment variables.
package config

import (
	"fmt"
	"net"
	urlpkg "net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration for websyncd, populated from environment
// variables via LoadFromEnv. All fields are read-only after construction; do not
// mutate a Config that has been passed to app.Run.
type Config struct {
	ResourceURL      string
	OutputPath       string
	PollInterval     time.Duration
	HTTPTimeout      time.Duration
	LockTTL          time.Duration
	WebhookAddr      string
	ResourceEventURL string
	EnableHTTP3      bool
	HeartbeatAddr    string
}

// LoadFromEnv constructs a Config by reading well-known environment variables.
//
// Required variables: RESOURCE_URL and OUTPUT_PATH. Optional variables and their
// defaults are documented in the project README. If `RESOURCE_EVENT_URL` is set
// the daemon will consume the stream for push-driven updates.
//
// Returns a fully populated Config on success, or an error describing the first
// missing or invalid required variable.
//
// Typical call site is app.LoadConfigFromEnv, which delegates here and is the
// recommended entry point from main.
func LoadFromEnv() (Config, error) {
	cfg := Config{
		ResourceURL:      os.Getenv("RESOURCE_URL"),
		OutputPath:       os.Getenv("OUTPUT_PATH"),
		PollInterval:     envDuration("POLL_INTERVAL", 30*time.Minute),
		HTTPTimeout:      envDuration("HTTP_TIMEOUT", 30*time.Second),
		LockTTL:          envDuration("LOCK_TTL", 5*time.Minute),
		WebhookAddr:      envString("WEBHOOK_ADDR", ""),
		ResourceEventURL: envString("RESOURCE_EVENT_URL", ""),
		EnableHTTP3:      envBool("ENABLE_HTTP3"),
		HeartbeatAddr:    envString("HEARTBEAT_ADDR", ""),
	}

	if cfg.ResourceURL == "" {
		return Config{}, fmt.Errorf("RESOURCE_URL is required")
	}
	if cfg.OutputPath == "" {
		return Config{}, fmt.Errorf("OUTPUT_PATH is required")
	}
	// Validate URLs and addresses.
	if err := validateURL(cfg.ResourceURL, "RESOURCE_URL"); err != nil {
		return Config{}, err
	}
	if cfg.ResourceEventURL != "" {
		if err := validateURL(cfg.ResourceEventURL, "RESOURCE_EVENT_URL"); err != nil {
			return Config{}, err
		}
	}
	if err := validateAddr(cfg.WebhookAddr, "WEBHOOK_ADDR"); err != nil {
		return Config{}, err
	}
	if err := validateAddr(cfg.HeartbeatAddr, "HEARTBEAT_ADDR"); err != nil {
		return Config{}, err
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

// validateURL ensures the provided string is an absolute HTTP(S) URL with a host.
func validateURL(s, name string) error {
	if s == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := urlpkg.ParseRequestURI(s)
	if err != nil {
		return fmt.Errorf("%s: invalid URL %q: %w", name, s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: unsupported URL scheme %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host in URL %q", name, s)
	}
	return nil
}

// validateAddr ensures the provided address is either empty or in host:port
// format where host is empty or an IP address and port is a valid numeric port.
func validateAddr(s, name string) error {
	if s == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("%s: invalid address %q: %w", name, s, err)
	}
	if port == "" {
		return fmt.Errorf("%s: missing port in address %q", name, s)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("%s: invalid port in address %q", name, s)
	}
	if host != "" {
		// Accept IP addresses or hostnames (e.g. localhost, my-host.example).
		if ip := net.ParseIP(host); ip != nil {
			return nil
		}
		if !isValidHostname(host) {
			return fmt.Errorf("%s: invalid host in address %q", name, host)
		}
	}
	return nil
}

// isValidHostname performs a conservative syntactic check for DNS hostnames.
// It accepts labels of 1-63 chars, total length <=255, letters, digits and hyphen
// and disallows labels starting/ending with a hyphen. A trailing dot is allowed.
func isValidHostname(h string) bool {
	if h == "" {
		return false
	}
	// Allow and strip a trailing dot (FQDN)
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 255 {
		return false
	}
	parts := strings.Split(h, ".")
	for _, label := range parts {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			return false
		}
	}
	return true
}
