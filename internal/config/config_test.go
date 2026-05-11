package config

import (
	"strings"
	"testing"
	"time"
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

// TestEnvHelpers verifies env helper functions behave correctly for common inputs.
//
// This test covers the small helper functions used by LoadFromEnv: `envString`,
// `envDuration` and `envBool`. It asserts fallback behavior, correct parsing of
// durations (including invalid and negative values) and boolean parsing rules.
func TestEnvHelpers(t *testing.T) {
	t.Run("envString fallback", func(t *testing.T) {
		t.Setenv("FOO_BAR", "")
		if v := envString("FOO_BAR", "fallback"); v != "fallback" {
			t.Fatalf("envString returned %q, want %q", v, "fallback")
		}
		t.Setenv("FOO_BAR", "value")
		if v := envString("FOO_BAR", "fallback"); v != "value" {
			t.Fatalf("envString returned %q, want %q", v, "value")
		}
	})

	t.Run("envDuration parsing and fallback", func(t *testing.T) {
		t.Setenv("POLL_INTERVAL", "1h")
		if d := envDuration("POLL_INTERVAL", 30*time.Minute); d != time.Hour {
			t.Fatalf("envDuration returned %v, want %v", d, time.Hour)
		}
		t.Setenv("POLL_INTERVAL", "not-a-duration")
		if d := envDuration("POLL_INTERVAL", 30*time.Minute); d != 30*time.Minute {
			t.Fatalf("envDuration returned %v, want fallback %v", d, 30*time.Minute)
		}
		t.Setenv("POLL_INTERVAL", "-1s")
		if d := envDuration("POLL_INTERVAL", 30*time.Minute); d != 30*time.Minute {
			t.Fatalf("envDuration returned %v for negative value, want fallback %v", d, 30*time.Minute)
		}
	})

	t.Run("envBool parsing", func(t *testing.T) {
		t.Setenv("ENABLE_HTTP3", "true")
		if !envBool("ENABLE_HTTP3") {
			t.Fatalf("envBool returned false for true value")
		}
		t.Setenv("ENABLE_HTTP3", "notbool")
		if envBool("ENABLE_HTTP3") {
			t.Fatalf("envBool returned true for invalid value")
		}
		t.Setenv("ENABLE_HTTP3", "")
		if envBool("ENABLE_HTTP3") {
			t.Fatalf("envBool returned true for empty value")
		}
	})
}

// TestValidateURLAndAddr exercises URL and address validation logic including
// expected failure modes that users may encounter when misconfiguring env
// variables.
func TestValidateURLAndAddr(t *testing.T) {
	// URL tests
	urlCases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid-http", "http://example.invalid/path", false},
		{"valid-https", "https://example.invalid/", false},
		{"missing-scheme", "//example.invalid", true},
		{"unsupported-scheme", "ftp://example.invalid/", true},
		{"missing-host", "http:///nohost", true},
		{"empty", "", true},
	}
	for _, tc := range urlCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateURL(tc.in, "TEST_URL")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateURL(%q) error = %v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}

	// Addr tests
	addrCases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", false},
		{"port-only", ":8080", false},
		{"ip-port", "127.0.0.1:8000", false},
		{"hostname-port", "localhost:9000", false},
		{"fqdn-port", "sub.example.invalid:9000", false},
		{"missing-port", "127.0.0.1", true},
		{"bad-port-number", "host:99999", true},
		{"non-numeric-port", "host:abc", true},
		{"invalid-hostname", "bad_host$:8080", true},
	}
	for _, tc := range addrCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAddr(tc.in, "TEST_ADDR")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAddr(%q) error = %v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestIsValidHostname verifies the syntactic hostname checker handles common
// valid and invalid hostnames, including FQDN trailing dots and length limits.
func TestIsValidHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"localhost", true},
		{"example.com", true},
		{"sub.domain.example.", true},
		{"a", true},
		{"-startdash", false},
		{"enddash-", false},
		{"has$sign", false},
		{"", false},
		{strings.Repeat("a", 64) + ".com", false},
		{strings.Repeat("a", 256), false},
	}
	for _, tc := range cases {
		got := isValidHostname(tc.in)
		if got != tc.want {
			t.Fatalf("isValidHostname(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestLoadFromEnvErrors ensures LoadFromEnv returns helpful errors when
// required variables are missing or invalid.
func TestLoadFromEnvErrors(t *testing.T) {
	// Missing RESOURCE_URL
	t.Setenv("RESOURCE_URL", "")
	t.Setenv("OUTPUT_PATH", "/tmp/x")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatalf("LoadFromEnv() succeeded with missing RESOURCE_URL")
	}

	// Missing OUTPUT_PATH
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatalf("LoadFromEnv() succeeded with missing OUTPUT_PATH")
	}

	// Invalid RESOURCE_URL
	t.Setenv("RESOURCE_URL", "ftp://example.invalid/")
	t.Setenv("OUTPUT_PATH", "/tmp/x")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatalf("LoadFromEnv() succeeded with invalid RESOURCE_URL scheme")
	}

	// Invalid RESOURCE_EVENT_URL when set
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/x")
	t.Setenv("RESOURCE_EVENT_URL", "ftp://example.invalid/stream")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatalf("LoadFromEnv() succeeded with invalid RESOURCE_EVENT_URL")
	}
}

// TestLoadFromEnvSuccess covers valid environment configurations and ensures
// values are parsed and returned correctly (poll interval, enable http3).
func TestLoadFromEnvSuccess(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.invalid/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("POLL_INTERVAL", "45m")
	t.Setenv("ENABLE_HTTP3", "true")
	t.Setenv("WEBHOOK_ADDR", "localhost:8080")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.PollInterval != 45*time.Minute {
		t.Fatalf("PollInterval = %v, want %v", cfg.PollInterval, 45*time.Minute)
	}
	if !cfg.EnableHTTP3 {
		t.Fatalf("EnableHTTP3 = %v, want true", cfg.EnableHTTP3)
	}
	if cfg.WebhookAddr != "localhost:8080" {
		t.Fatalf("WebhookAddr = %q, want %q", cfg.WebhookAddr, "localhost:8080")
	}
}
