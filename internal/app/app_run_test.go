package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomTonic/websyncd/internal/config"
	"github.com/TomTonic/websyncd/internal/lock"
	"github.com/TomTonic/websyncd/internal/syncer"
)

type mockDoer struct {
	do func(req *http.Request) (*http.Response, error)
}

func (m mockDoer) Do(req *http.Request) (*http.Response, error) {
	return m.do(req)
}

type mockReportSyncer struct {
	report syncer.SyncReport
	err    error
	calls  int
}

func (m *mockReportSyncer) SyncWithReport(context.Context) (syncer.SyncReport, error) {
	m.calls++
	return m.report, m.err
}

// TestHandleTriggerSuccess verifies that a successful sync trigger execution
// updates health metrics and keeps the loop running.
//
// This test covers trigger handling in the app package after refactoring,
// where sync execution is isolated behind a narrow interface.
//
// It simulates a successful sync and asserts counters and return value.
func TestHandleTriggerSuccess(t *testing.T) {
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)
	health := newHealthState(time.Now())
	syncerMock := &mockReportSyncer{report: syncer.SyncReport{DownloadPerformed: true}}

	stop := handleTrigger(context.Background(), syncerMock, health, logger, "test")
	if stop {
		t.Fatalf("handleTrigger returned stop=true, want false")
	}
	if syncerMock.calls != 1 {
		t.Fatalf("SyncWithReport calls = %d, want 1", syncerMock.calls)
	}
	s := health.snapshot(time.Now())
	if s.SyncTotal != 1 || s.SyncSuccess != 1 || s.SyncFailure != 0 {
		t.Fatalf("unexpected counters: total=%d success=%d failure=%d", s.SyncTotal, s.SyncSuccess, s.SyncFailure)
	}
}

// TestHandleTriggerFailure verifies that a failed sync trigger execution is
// recorded as failure while the loop remains active.
//
// This test covers the recoverable error path for sync processing.
//
// It simulates a non-cancellation error and asserts failure metrics are updated.
func TestHandleTriggerFailure(t *testing.T) {
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)
	health := newHealthState(time.Now())
	syncerMock := &mockReportSyncer{err: errors.New("boom")}

	stop := handleTrigger(context.Background(), syncerMock, health, logger, "test")
	if stop {
		t.Fatalf("handleTrigger returned stop=true, want false")
	}
	s := health.snapshot(time.Now())
	if s.SyncTotal != 1 || s.SyncSuccess != 0 || s.SyncFailure != 1 {
		t.Fatalf("unexpected counters: total=%d success=%d failure=%d", s.SyncTotal, s.SyncSuccess, s.SyncFailure)
	}
	if s.LastError == "" {
		t.Fatalf("LastError should be set on failure")
	}
}

// TestHandleTriggerCanceled verifies that cancellation errors stop the run loop
// without recording a failed sync.
//
// This test covers shutdown semantics for in-flight sync operations.
//
// It simulates context cancellation from the sync layer and asserts stop=true.
func TestHandleTriggerCanceled(t *testing.T) {
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)
	health := newHealthState(time.Now())
	syncerMock := &mockReportSyncer{err: context.Canceled}

	stop := handleTrigger(context.Background(), syncerMock, health, logger, "test")
	if !stop {
		t.Fatalf("handleTrigger returned stop=false, want true")
	}
	s := health.snapshot(time.Now())
	if s.SyncFailure != 0 {
		t.Fatalf("SyncFailure = %d, want 0 for cancellation", s.SyncFailure)
	}
}

// TestStartSSEHandlesConnectionFailure verifies that SSE reconnect handling
// exits quickly when context is cancelled after a connection error.
//
// This test covers the startSSE reconnect path in the app package.
//
// It forces Do to fail and cancels context from the mock, asserting clean exit.
func TestStartSSEHandlesConnectionFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)

	doer := mockDoer{do: func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("dial error")
	}}

	startSSE(ctx, doer, "http://example.test/events", func(string) {}, logger)
}

// TestStartSSETriggersOnEventBoundary verifies that empty SSE delimiter lines
// trigger sync events.
//
// This test covers the scanner/event loop in startSSE.
//
// It serves a small SSE payload and asserts exactly one trigger is emitted.
func TestStartSSETriggersOnEventBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)

	body := io.NopCloser(strings.NewReader("data: ping\n\n"))
	doer := mockDoer{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}}

	triggered := 0
	startSSE(ctx, doer, "http://example.test/events", func(string) {
		triggered++
		cancel()
	}, logger)

	if triggered != 1 {
		t.Fatalf("triggered = %d, want 1", triggered)
	}
}

// TestStartWebhookInvalidAddr verifies webhook server startup reports and exits
// cleanly when configured with an invalid listen address.
//
// This test covers the lifecycle plumbing in startWebhook.
//
// It passes an invalid address and asserts the function returns.
func TestStartWebhookInvalidAddr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)
	startWebhook(ctx, "invalid-addr", func(string) {}, logger)
}

// TestStartHeartbeatInvalidAddr verifies heartbeat server startup reports and
// exits cleanly when configured with an invalid listen address.
//
// This test covers the lifecycle plumbing in startHeartbeat.
//
// It passes an invalid address and asserts the function returns.
func TestStartHeartbeatInvalidAddr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines []string
	logger := log.New(&testLogWriter{lines: &lines}, "", 0)
	startHeartbeat(ctx, "invalid-addr", newHealthState(time.Now()), logger)
}

// TestRunStartupSyncAndShutdown verifies the daemon performs an initial startup
// sync and exits cleanly when its parent context is cancelled.
//
// This test covers the end-to-end Run orchestration path in the app package,
// including trigger setup, sync execution, and graceful shutdown.
//
// It serves static content, runs the daemon briefly, and asserts output is written.
func TestRunStartupSyncAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "hello from run test")
	}))
	defer server.Close()

	out := filepath.Join(t.TempDir(), "synced.txt")
	cfg := &config.Config{
		ResourceURL:              server.URL,
		OutputPath:               out,
		PollInterval:             time.Hour,
		HTTPTimeout:              2 * time.Second,
		LockTTL:                  time.Minute,
		DownloadProgressInterval: 10 * time.Millisecond,
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, cfg, logger)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		//nolint:gosec // Test uses t.TempDir()-scoped path controlled within the test.
		if data, err := os.ReadFile(out); err == nil && strings.Contains(string(data), "hello from run test") {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("output file was not written in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not stop after context cancellation")
	}
}

// TestRunReturnsLockConflict verifies the daemon fails fast with a readable
// error when another process already holds the same sync lock.
//
// This test covers lock conflict handling in Run.
//
// It acquires the lock first and asserts Run returns a conflict error.
func TestRunReturnsLockConflict(t *testing.T) {
	out := filepath.Join(t.TempDir(), "locked.txt")
	resource := "https://example.invalid/resource"
	lease, err := lockAcquireForTest(resource, out)
	if err != nil {
		t.Fatalf("failed to pre-acquire lock: %v", err)
	}
	defer func() { _ = lease.release() }()

	cfg := &config.Config{
		ResourceURL:              resource,
		OutputPath:               out,
		PollInterval:             time.Hour,
		HTTPTimeout:              time.Second,
		LockTTL:                  time.Minute,
		DownloadProgressInterval: 10 * time.Millisecond,
	}
	err = Run(context.Background(), cfg, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatalf("Run error = nil, want lock conflict error")
	}
	if !strings.Contains(err.Error(), "another instance") {
		t.Fatalf("unexpected lock conflict error: %v", err)
	}
}

type testLease interface {
	release() error
}

type lockLease struct {
	releaseFn func() error
}

func (l lockLease) release() error {
	return l.releaseFn()
}

func lockAcquireForTest(resource, output string) (testLease, error) {
	l, err := lock.Acquire(resource, output, time.Minute, time.Now)
	if err != nil {
		return nil, err
	}
	return lockLease{releaseFn: l.Release}, nil
}
