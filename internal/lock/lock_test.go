package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestAcquirePreventsConcurrentExecution verifies that starting a second
// identical daemon instance is blocked while the first is running.
//
// This test covers the Acquire function in the lock package, which guards
// against duplicate syncer processes writing to the same output file.
//
// It acquires a lock, attempts a second acquisition for the same
// resource/output pair, and asserts ErrLocked is returned. It then releases
// the first lock and confirms a third acquisition succeeds.
func TestAcquirePreventsConcurrentExecution(t *testing.T) {
	// User perspective: starting a second identical daemon instance should be blocked.
	// System perspective: first lock acquisition wins, second fails until release.
	// Code perspective: O_CREATE|O_EXCL lock file should return ErrLocked.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	now := time.Now()
	first, err := Acquire(
		"https://example.invalid/resource",
		"/tmp/file",
		30*time.Second,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	defer func() { _ = first.Release() }()

	second, err := Acquire(
		"https://example.invalid/resource",
		"/tmp/file",
		30*time.Second,
		func() time.Time { return now.Add(time.Second) },
	)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire() err = %v, want ErrLocked", err)
	}
	if second != nil {
		t.Fatalf("second lock should be nil")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	third, err := Acquire(
		"https://example.invalid/resource",
		"/tmp/file",
		30*time.Second,
		func() time.Time { return now.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatalf("third Acquire() error = %v", err)
	}
	_ = third.Release()
}

// TestAcquireRemovesStaleLock verifies that a lock left by a crashed process
// is cleaned up automatically so the daemon can self-heal.
//
// This test covers the Acquire function in the lock package.
//
// It writes a lock file with a timestamp two hours in the past, then asserts
// that Acquire succeeds by treating the old lock as expired.
func TestAcquireRemovesStaleLock(t *testing.T) {
	// User perspective: stale crashed process lock should self-heal automatically.
	// System perspective: expired timestamp permits lock takeover.
	// Code perspective: stale lock file is deleted before re-acquiring.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	path := lockPath("https://example.invalid/resource", "/tmp/file")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	staleTs := time.Now().Add(-2 * time.Hour).Unix()
	if err := os.WriteFile(path, []byte("pid=1\ntimestamp="+strconv.FormatInt(staleTs, 10)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	l, err := Acquire("https://example.invalid/resource", "/tmp/file", time.Minute, time.Now)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	_ = l.Release()
}
