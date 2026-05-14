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
	ResourceURL              string
	OutputPath               string
	PollInterval             time.Duration
	HTTPTimeout              time.Duration
	LockTTL                  time.Duration
	WebhookAddr              string
	ResourceEventURL         string
	EnableHTTP3              bool
	HeartbeatAddr            string
	DownloadProgressInterval time.Duration
	MaxDownloadBytes         int64
	OutputFileAttributesSet  bool
	OutputFileMode           os.FileMode
	OutputFileUID            int
	OutputFileGID            int
}

// LoadFromEnv constructs a Config by reading well-known environment variables.
//
// Required variables: RESOURCE_URL and OUTPUT_PATH. Optional variables and their
// defaults are documented in the project README. If `RESOURCE_EVENT_URL` is set
// the daemon will consume the stream for push-driven updates.
//
// HTTP/3 Auto-Upgrade is enabled by default; set ENABLE_HTTP3=false to opt out
// (useful if QUIC is blocked on the network or causes compatibility issues).
//
// Returns a fully populated Config on success, or an error describing the first
// missing or invalid required variable.
//
// Typical call site is app.LoadConfigFromEnv, which delegates here and is the
// recommended entry point from main.
func LoadFromEnv() (Config, error) {
	cfg := Config{
		ResourceURL:              os.Getenv("RESOURCE_URL"),
		OutputPath:               os.Getenv("OUTPUT_PATH"),
		PollInterval:             envDuration("POLL_INTERVAL", 1*time.Hour),
		HTTPTimeout:              envDuration("HTTP_TIMEOUT", 30*time.Second),
		LockTTL:                  envDuration("LOCK_TTL", 5*time.Minute),
		WebhookAddr:              envString("WEBHOOK_ADDR", ""),
		ResourceEventURL:         envString("RESOURCE_EVENT_URL", ""),
		EnableHTTP3:              envEnableHTTP3(),
		HeartbeatAddr:            envString("HEARTBEAT_ADDR", ""),
		DownloadProgressInterval: envDuration("DOWNLOAD_PROGRESS_INTERVAL", 5*time.Second),
		MaxDownloadBytes:         envInt64("MAX_DOWNLOAD_BYTES", 0),
		OutputFileMode:           0o644,
	}

	minPoll := 5 * time.Second
	if cfg.PollInterval < minPoll {
		return Config{}, fmt.Errorf("POLL_INTERVAL: value %s is too small; minimum is %s", cfg.PollInterval, minPoll)
	}

	if attrsRaw := strings.TrimSpace(os.Getenv("OUTPUT_FILE_ATTRIBUTES")); attrsRaw != "" {
		uid, gid, mode, parseErr := parseOutputFileAttributes(attrsRaw)
		if parseErr != nil {
			return Config{}, parseErr
		}
		cfg.OutputFileAttributesSet = true
		cfg.OutputFileUID = uid
		cfg.OutputFileGID = gid
		cfg.OutputFileMode = mode
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

// envInt64 parses name as a non-negative integer. Returns fallback when the
// variable is absent, non-numeric, or negative.
func envInt64(name string, fallback int64) int64 { //nolint:unparam // name is a generic parameter; additional callers may be added later
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// envEnableHTTP3 returns true by default (HTTP/3 Auto-Upgrade is enabled unless
// the caller explicitly opts out by setting ENABLE_HTTP3=false).
func envEnableHTTP3() bool {
	v := os.Getenv("ENABLE_HTTP3")
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return b
}

// parseOutputFileAttributes parses OUTPUT_FILE_ATTRIBUTES in the format
// "uid:gid:mode", where mode is an octal permission value like 0644.
func parseOutputFileAttributes(v string) (uid int, gid int, mode os.FileMode, err error) {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("OUTPUT_FILE_ATTRIBUTES must be in format uid:gid:mode")
	}

	uid, err = strconv.Atoi(parts[0])
	if err != nil || uid < 0 {
		return 0, 0, 0, fmt.Errorf("OUTPUT_FILE_ATTRIBUTES: invalid uid %q", parts[0])
	}

	gid, err = strconv.Atoi(parts[1])
	if err != nil || gid < 0 {
		return 0, 0, 0, fmt.Errorf("OUTPUT_FILE_ATTRIBUTES: invalid gid %q", parts[1])
	}

	parsedMode, err := strconv.ParseUint(parts[2], 8, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("OUTPUT_FILE_ATTRIBUTES: invalid mode %q", parts[2])
	}
	if parsedMode > 0o7777 {
		return 0, 0, 0, fmt.Errorf("OUTPUT_FILE_ATTRIBUTES: mode %q out of range", parts[2])
	}

	return uid, gid, os.FileMode(parsedMode), nil
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
