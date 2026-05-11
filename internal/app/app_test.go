package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		"sync_total=2",
		"sync_success=1",
		"sync_failure=1",
		"last_error=sync failed",
	} {
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
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	heartbeatHandler(newHealthState(time.Now())).ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
	if got := rr.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want %q", got, http.MethodGet)
	}
}
