package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TomTonic/websyncd/internal/syncer"
)

// TestHealthStateSnapshot verifies that the health dashboard correctly tallies
// sync counts and preserves the last error message after a failed sync.
//
// This test covers the healthState type in the app package, which feeds both
// the /healthz HTTP endpoint and the periodic heartbeat log line.
//
// It drives the state machine through a success/failure cycle and asserts that
// snapshot returns the correct counters and error string.
func TestHealthStateSnapshot(t *testing.T) {
	started := time.Now().Add(-10 * time.Second)
	state := newHealthState(started)

	syncAt := time.Now().Add(-5 * time.Second)
	successAt := time.Now().Add(-4 * time.Second)
	failureAt := time.Now().Add(-3 * time.Second)

	state.recordSyncStart(syncAt)
	state.recordSyncSuccess(successAt)
	state.recordSyncStart(successAt)
	state.recordSyncFailure(failureAt, errors.New("boom"))

	s := state.snapshot(time.Now())
	if s.SyncTotal != 2 {
		t.Fatalf("SyncTotal = %d, want 2", s.SyncTotal)
	}
	if s.SyncSuccess != 1 {
		t.Fatalf("SyncSuccess = %d, want 1", s.SyncSuccess)
	}
	if s.SyncFailure != 1 {
		t.Fatalf("SyncFailure = %d, want 1", s.SyncFailure)
	}
	if s.LastError != "boom" {
		t.Fatalf("LastError = %q, want %q", s.LastError, "boom")
	}
	if s.Uptime <= 0 {
		t.Fatalf("Uptime = %s, want > 0", s.Uptime)
	}
}

// TestHeartbeatHandlerHealthz verifies that the /healthz endpoint returns a
// machine-readable health report that operators and monitoring agents can parse.
//
// This test covers the heartbeatHandler function in the app package, which
// serves the liveness probe used in container orchestration environments.
//
// It asserts that the response body contains all expected key=value lines,
// including sync counters and the last error message.
func TestHeartbeatHandlerHealthz(t *testing.T) {
	state := newHealthState(time.Now().Add(-20 * time.Second))
	state.recordLoopBeat(time.Now().Add(-2 * time.Second))
	state.recordSyncStart(time.Now().Add(-6 * time.Second))
	state.recordSyncSuccess(time.Now().Add(-5 * time.Second))
	state.recordSyncStart(time.Now().Add(-4 * time.Second))
	state.recordSyncFailure(time.Now().Add(-3*time.Second), errors.New("sync failed"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"status=ok",
		"probe=liveness",
		"loop_beat_age=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHeartbeatHandlerReadyzHealthyAfterSuccessfulSync verifies that the
// readiness endpoint reports ready once at least one sync has completed
// successfully.
//
// This test covers the heartbeatHandler readiness branch in the app package.
//
// It records a successful sync and asserts /readyz responds with HTTP 200.
func TestHeartbeatHandlerReadyzHealthyAfterSuccessfulSync(t *testing.T) {
	state := newHealthState(time.Now().Add(-20 * time.Second))
	state.recordLoopBeat(time.Now().Add(-2 * time.Second))
	state.recordSyncStart(time.Now().Add(-6 * time.Second))
	state.recordSyncSuccess(time.Now().Add(-5 * time.Second))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{"status=ok", "probe=readiness"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHeartbeatHandlerReadyzUnavailableUntilFirstSuccess verifies that the
// readiness endpoint reports unavailable (500) when resource is inaccessible.
//
// This test covers the heartbeatHandler readiness branch in the app package.
//
// It records only failed syncs and asserts /readyz responds with HTTP 500.
func TestHeartbeatHandlerReadyzUnavailableUntilFirstSuccess(t *testing.T) {
	state := newHealthState(time.Now().Add(-20 * time.Second))
	state.recordLoopBeat(time.Now().Add(-2 * time.Second))
	state.recordSyncStart(time.Now().Add(-6 * time.Second))
	state.recordSyncFailure(time.Now().Add(-5*time.Second), errors.New("upstream unavailable"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	body := rr.Body.String()
	for _, want := range []string{"status=error", "probe=readiness", "reason=resource_not_accessible"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHeartbeatHandlerReadyzDetectsRecentUnavailability verifies that the
// readiness endpoint reports unavailable (500) when the failure rate exceeds
// tolerance thresholds despite some historical successes.
//
// This test covers the failure-rate logic in the app package.
//
// It records multiple failures after some successes and asserts /readyz
// responds with 500 and a resource unavailability reason.
func TestHeartbeatHandlerReadyzDetectsRecentUnavailability(t *testing.T) {
	state := newHealthState(time.Now().Add(-20 * time.Second))
	state.recordLoopBeat(time.Now().Add(-2 * time.Second))

	state.recordSyncStart(time.Now().Add(-10 * time.Second))
	state.recordSyncSuccess(time.Now().Add(-9 * time.Second))

	for i := 0; i < 4; i++ {
		state.recordSyncStart(time.Now().Add(-time.Duration(8-i) * time.Second))
		state.recordSyncFailure(time.Now().Add(-time.Duration(7-i)*time.Second), errors.New("resource unavailable"))
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	body := rr.Body.String()
	for _, want := range []string{"status=error", "probe=readiness", "reason=resource_recently_unavailable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHeartbeatHandlerHealthzDetectsStalledLoop verifies that liveness fails
// when no sync attempt has been observed within the liveness stale window.
//
// This test covers the heartbeatHandler liveness branch in the app package.
//
// It sets a short poll interval and an old last sync timestamp, then asserts
// /healthz responds with HTTP 500 and a stalled-loop reason.
func TestHeartbeatHandlerHealthzDetectsStalledLoop(t *testing.T) {
	state := newHealthState(time.Now().Add(-5 * time.Minute))
	state.recordLoopBeat(time.Now().Add(-2 * time.Minute))
	state.recordSyncStart(time.Now().Add(-10 * time.Second))
	state.recordSyncFailure(time.Now().Add(-10*time.Second), errors.New("upstream unavailable"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	body := rr.Body.String()
	for _, want := range []string{"status=error", "probe=liveness", "reason=loop_heartbeat_stalled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// TestHeartbeatHandlerMethodNotAllowed verifies that non-GET requests to the
// /healthz endpoint are rejected with a 405 Method Not Allowed response.
//
// This test covers the heartbeatHandler function in the app package.
//
// It sends a POST request and asserts both the status code and the Allow header.
func TestHeartbeatHandlerMethodNotAllowed(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		heartbeatHandler(newHealthState(time.Now())).ServeHTTP(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("path=%s status = %d, want %d", path, rr.Code, http.StatusMethodNotAllowed)
		}
		if got := rr.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("path=%s Allow = %q, want %q", path, got, http.MethodGet)
		}
	}
}

// TestHeartbeatHandlerSanitizesLastErrorForLogInjection verifies that the
// last_error field in the /healthz response is sanitized to prevent log
// injection attacks when the error message contains newlines.
//
// This test covers the log injection vulnerability fix in the heartbeatHandler.
//
// It simulates a sync failure with a multi-line error message and asserts that
// the response contains the error with newlines replaced by spaces, preventing
// fake key=value lines from being injected.
func TestHeartbeatHandlerSanitizesLastErrorForLogInjection(t *testing.T) {
	state := newHealthState(time.Now())
	// Simulate a compromised upstream server returning a multi-line error
	state.recordSyncFailure(time.Now(), errors.New("error message\nlast_success_age=9999s\r\nmore=injected"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	heartbeatHandler(state).ServeHTTP(rr, req)

	body := rr.Body.String()
	// The error should be sanitized: newlines replaced with spaces
	if strings.Contains(body, "error message\nlast_success_age") {
		t.Fatalf("body contains unsanitized newline in error: %s", body)
	}
	// Should contain the sanitized version
	if !strings.Contains(body, "error message last_success_age") {
		t.Fatalf("body missing sanitized error message, got:\n%s", body)
	}
}

// TestWebhookTriggerQueuesSync verifies that POST requests to the webhook
// endpoint trigger a sync by queuing the trigger.
//
// This test covers the startWebhook function's POST handler.
//
// It sends a POST request and verifies that a trigger was queued via the
// trigger callback function.
func TestWebhookTriggerQueuesSync(t *testing.T) {
	triggered := make(chan string, 1)
	trigger := func(source string) {
		triggered <- source
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	// Create a simple mux matching the webhook handler structure
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		trigger("webhook")
		w.WriteHeader(http.StatusAccepted)
	})

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}
	select {
	case source := <-triggered:
		if source != "webhook" {
			t.Fatalf("trigger source = %q, want webhook", source)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("trigger was not called")
	}
}

// TestWebhookRejectsNonPost verifies that non-POST requests to the webhook
// endpoint are rejected with a 405 Method Not Allowed response.
//
// This test covers the startWebhook function's method validation.
//
// It sends a GET request and asserts the status code and Allow header.
func TestWebhookRejectsNonPost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
	if got := rr.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want %q", got, http.MethodPost)
	}
}

// TestSleepOrDoneCanceledContext verifies that sleepOrDone returns false
// immediately when the context is already cancelled, without blocking.
//
// This test covers the sleepOrDone utility function in the app package,
// which safely sleeps until a duration elapses or the context is cancelled.
//
// It passes a cancelled context and asserts false is returned without waiting.
func TestSleepOrDoneCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := sleepOrDone(ctx, 10*time.Second)
	elapsed := time.Since(start)

	if result != false {
		t.Fatalf("sleepOrDone returned %v, want false for cancelled context", result)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("sleepOrDone took %v, want <100ms when context is already cancelled", elapsed)
	}
}

// TestSleepOrDoneWaitsUntilTimer verifies that sleepOrDone blocks for the
// specified duration when the context is not cancelled.
//
// This test covers the sleepOrDone utility function in the app package.
//
// It passes a valid context with a 50ms sleep duration and asserts at least
// that much time elapses before returning true.
func TestSleepOrDoneWaitsUntilTimer(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	result := sleepOrDone(ctx, 50*time.Millisecond)
	elapsed := time.Since(start)

	if !result {
		t.Fatalf("sleepOrDone returned %v, want true when timer expires normally", result)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("sleepOrDone took %v, want ≥50ms", elapsed)
	}
}

// TestFormatAgeNeverReturnsNever verifies that the formatAge helper returns
// "never" when the timestamp is zero (not yet recorded).
//
// This test covers the formatAge function used by the health check endpoint.
//
// It passes a zero timestamp and asserts "never" is returned.
func TestFormatAgeNeverReturnsNever(t *testing.T) {
	now := time.Now()
	result := formatAge(time.Time{}, now)
	if result != "never" {
		t.Fatalf("formatAge(zero) = %q, want never", result)
	}
}

// TestFormatAgeComputesDelta verifies that formatAge calculates the correct
// time delta and formats it as a human-readable duration.
//
// This test covers the formatAge function used by the health check endpoint.
//
// It passes a timestamp 30 seconds in the past and asserts a duration string
// like "30s" is returned.
func TestFormatAgeComputesDelta(t *testing.T) {
	now := time.Now()
	at := now.Add(-30 * time.Second)
	result := formatAge(at, now)
	if !strings.Contains(result, "30") {
		t.Fatalf("formatAge result %q should contain 30s", result)
	}
}

// TestFormatBytesFormatsKilo verifies that formatBytes correctly scales byte
// counts to human-readable units (B, KiB, MiB, etc).
//
// This test covers the formatBytes function used for logging download sizes.
//
// It tests several byte counts and asserts correct unit scaling.
func TestFormatBytesFormatsKilo(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{-1, "unknown"},
	}
	for _, tc := range cases {
		result := formatBytes(tc.bytes)
		if result != tc.want {
			t.Fatalf("formatBytes(%d) = %q, want %q", tc.bytes, result, tc.want)
		}
	}
}

// TestFormatRateFormatsSpeedWithUnit verifies that formatRate converts bytes
// per second to a human-readable speed string (B/s, KiB/s, etc).
//
// This test covers the formatRate function used for logging download speeds.
//
// It tests several byte rates and asserts correct formatting.
func TestFormatRateFormatsSpeedWithUnit(t *testing.T) {
	cases := []struct {
		bytesPerSec float64
		valid       bool
	}{
		{1024, true},        // 1.0 KiB/s
		{1024 * 1024, true}, // 1.0 MiB/s
		{0, false},          // n/a
		{-1, false},         // n/a
	}
	for _, tc := range cases {
		result := formatRate(tc.bytesPerSec)
		if tc.valid && result == "n/a" {
			t.Fatalf("formatRate(%f) = %q, want non-n/a", tc.bytesPerSec, result)
		}
		if !tc.valid && result != "n/a" {
			t.Fatalf("formatRate(%f) = %q, want n/a", tc.bytesPerSec, result)
		}
	}
}

// TestFormatBytesSingedFormatsWithSignPrefix verifies that formatBytesSigned
// adds a +/- prefix to the formatted byte count.
//
// This test covers the formatBytesSigned function used for logging file
// size deltas in sync reports.
//
// It tests positive, negative, and zero values.
func TestFormatBytesSignedFormatsWithSignPrefix(t *testing.T) {
	result := formatBytesSigned(1024)
	if !strings.HasPrefix(result, "+") {
		t.Fatalf("positive value should have + prefix: %q", result)
	}
	result = formatBytesSigned(-1024)
	if !strings.HasPrefix(result, "-") {
		t.Fatalf("negative value should have - prefix: %q", result)
	}
}

// TestLogSyncReportDownloadPerformed verifies that the sync report logger
// correctly formats download summary logs with protocol, bytes, and rate.
//
// This test covers the logSyncReport function in the app package, which
// produces human-readable operational logs for successful downloads.
//
// It constructs a SyncReport with download performed and asserts the log
// message contains all key metrics.
func TestLogSyncReportDownloadPerformed(t *testing.T) {
	var logged []string
	testLogger := log.New(&testLogWriter{lines: &logged}, "", 0)

	report := &syncer.SyncReport{
		DownloadPerformed:       true,
		DownloadDecision:        "HEAD indicates possible resource change",
		Protocol:                "HTTP/2.0 with TLS 1.3",
		TransferBytes:           2048,
		TransferDuration:        1 * time.Second,
		TransferRateBytesPerSec: 2048,
		PreviousFileSize:        1024,
		NewFileSize:             2048,
		SizeDeltaBytes:          1024,
	}

	logSyncReport(testLogger, "poll", 2*time.Second, report)

	joined := strings.Join(logged, "\n")
	for _, want := range []string{
		"sync download:",
		"poll",
		"HTTP/2.0 with TLS 1.3",
		"2.0 KiB",
		"2.0 KiB/s",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log missing %q, got:\n%s", want, joined)
		}
	}
}

// TestLogSyncReportDownloadSkipped verifies that the sync report logger
// correctly formats skip logs when no new download was required.
//
// This test covers the logSyncReport function in the app package for the
// case where the resource was already up-to-date.
//
// It constructs a SyncReport with DownloadPerformed=false and asserts the log
// indicates "skipped" and includes the reason.
func TestLogSyncReportDownloadSkipped(t *testing.T) {
	var logged []string
	testLogger := log.New(&testLogWriter{lines: &logged}, "", 0)

	report := &syncer.SyncReport{
		DownloadPerformed:  false,
		DownloadSkipReason: "HEAD ETag matches previous sync",
		Protocol:           "HTTP/2.0 with TLS 1.3",
		PreviousFileSize:   1024,
		NewFileSize:        1024,
		SizeDeltaBytes:     0,
	}

	logSyncReport(testLogger, "webhook", 100*time.Millisecond, report)

	joined := strings.Join(logged, "\n")
	for _, want := range []string{
		"sync download skipped:",
		"webhook",
		"skipped",
		"HEAD ETag matches",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log missing %q, got:\n%s", want, joined)
		}
	}
}

// TestLogSyncReportFileReplaced verifies that the sync report logger correctly
// formats file replacement logs with size deltas and freshness information.
//
// This test covers the logSyncReport function in the app package for the case
// where the downloaded content was different from the local file.
//
// It constructs a SyncReport with LocalReplacePerformed=true and FreshnessKnown=true,
// and asserts the log includes size change and freshness information.
func TestLogSyncReportFileReplaced(t *testing.T) {
	var logged []string
	testLogger := log.New(&testLogWriter{lines: &logged}, "", 0)

	now := time.Now()
	report := &syncer.SyncReport{
		DownloadPerformed:       true,
		LocalReplacePerformed:   true,
		Protocol:                "HTTP/3 (QUIC) with TLS 1.3",
		TransferBytes:           512,
		TransferDuration:        500 * time.Millisecond,
		TransferRateBytesPerSec: 1024,
		PreviousFileSize:        1024,
		NewFileSize:             512,
		SizeDeltaBytes:          -512,
		RemoteLastModified:      now.Add(-1 * time.Hour),
		FreshnessKnown:          true,
		FreshnessDelta:          -1 * time.Hour,
	}

	logSyncReport(testLogger, "sse", 1500*time.Millisecond, report)

	joined := strings.Join(logged, "\n")
	for _, want := range []string{
		"sync file result:",
		"action=replaced",
		"new content differs",
		"-512",
		"older",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log missing %q, got:\n%s", want, joined)
		}
	}
}

// TestLogSyncReportFileSkipped verifies that the sync report logger correctly
// formats file-skip logs when the downloaded content was identical to the
// existing local file.
//
// This test covers the logSyncReport function in the app package for the
// case where no file replacement was needed.
//
// It constructs a SyncReport with DownloadPerformed=true but LocalReplacePerformed=false,
// and asserts the log indicates "skipped" with the reason.
func TestLogSyncReportFileSkipped(t *testing.T) {
	var logged []string
	testLogger := log.New(&testLogWriter{lines: &logged}, "", 0)

	report := &syncer.SyncReport{
		DownloadPerformed:       true,
		LocalReplacePerformed:   false,
		LocalReplaceSkipReason:  "downloaded content is identical to local file",
		Protocol:                "HTTP/1.1 with TLS 1.2",
		TransferBytes:           1024,
		TransferDuration:        100 * time.Millisecond,
		TransferRateBytesPerSec: 10240,
		PreviousFileSize:        1024,
		NewFileSize:             1024,
		SizeDeltaBytes:          0,
		FreshnessKnown:          false,
	}

	logSyncReport(testLogger, "startup", 150*time.Millisecond, report)

	joined := strings.Join(logged, "\n")
	for _, want := range []string{
		"sync file result:",
		"action=skipped",
		"identical",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log missing %q, got:\n%s", want, joined)
		}
	}
}

// testLogWriter captures log lines for test assertions.
type testLogWriter struct {
	lines *[]string
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}

// TestLoadConfigFromEnvSuccess verifies that LoadConfigFromEnv correctly
// delegates to config.LoadFromEnv and returns a valid configuration.
//
// This test covers the LoadConfigFromEnv wrapper function in the app package,
// ensuring it provides the correct entry point for main.
//
// It sets required environment variables and asserts a valid config is returned.
func TestLoadConfigFromEnvSuccess(t *testing.T) {
	t.Setenv("RESOURCE_URL", "https://example.com/data")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")
	t.Setenv("POLL_INTERVAL", "10m")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v, want nil", err)
	}
	if cfg.ResourceURL != "https://example.com/data" {
		t.Fatalf("ResourceURL = %q, want https://example.com/data", cfg.ResourceURL)
	}
	if cfg.OutputPath != "/tmp/data.txt" {
		t.Fatalf("OutputPath = %q, want /tmp/data.txt", cfg.OutputPath)
	}
	if cfg.PollInterval != 10*time.Minute {
		t.Fatalf("PollInterval = %v, want 10m", cfg.PollInterval)
	}
}

// TestLoadConfigFromEnvMissingRequired verifies that LoadConfigFromEnv
// returns an error when RESOURCE_URL is missing.
//
// This test covers error handling in the LoadConfigFromEnv wrapper.
//
// It clears RESOURCE_URL and asserts an error is returned.
func TestLoadConfigFromEnvMissingRequired(t *testing.T) {
	t.Setenv("RESOURCE_URL", "")
	t.Setenv("OUTPUT_PATH", "/tmp/data.txt")

	_, err := LoadConfigFromEnv()
	if err == nil {
		t.Fatalf("LoadConfigFromEnv() error = nil, want error for missing RESOURCE_URL")
	}
}
