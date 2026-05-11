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

	if releaseErr := first.Release(); releaseErr != nil {
		t.Fatalf("Release() error = %v", releaseErr)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
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

// TestReleaseNil confirms Release is safe to call on a nil Lock.
//
// This test covers the idempotency of Release: calling Release on a nil
// receiver must not return an error so callers can safely defer Release.
func TestReleaseNil(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("Release() on nil returned error: %v", err)
	}
}

// TestReleaseAfterManualRemove checks Release handles a missing lock file gracefully.
//
// This test covers the scenario where an external agent removes the lock file
// before Release is called. Release should succeed (be a no-op) and return nil.
func TestReleaseAfterManualRemove(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	l, err := Acquire("https://example.invalid/res", "/tmp/out", time.Minute, time.Now)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	// Remove the file ourselves and then call Release; Release should return nil.
	if err := os.Remove(l.path); err != nil {
		t.Fatalf("Remove(lockfile) error = %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release() after manual remove returned error: %v", err)
	}
}

// TestIsStaleCoversMissingAndMalformedAndFresh exercises isStale for missing,
// malformed, fresh and expired timestamps.
//
// This test covers the parsing and TTL logic of isStale by creating files with
// no timestamp, a non-numeric timestamp, a recent timestamp (not stale) and an
// old timestamp (stale). It asserts the boolean result is correct in each case.
func TestIsStaleCoversMissingAndMalformedAndFresh(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	path := lockPath("https://example.invalid/res", "/tmp/out")
	// Ensure missing file returns true (considered stale)
	if !isStale(path, time.Minute, time.Now) {
		t.Fatalf("isStale(missing file) = false, want true")
	}

	// Malformed timestamp -> stale
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("pid=1\ntimestamp=notanint\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if !isStale(path, time.Minute, time.Now) {
		t.Fatalf("isStale(malformed timestamp) = false, want true")
	}

	// Fresh timestamp -> not stale
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	if err := os.WriteFile(path, []byte("pid=1\ntimestamp="+ts+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if isStale(path, 5*time.Minute, func() time.Time { return now }) {
		t.Fatalf("isStale(fresh timestamp) = true, want false")
	}

	// Old timestamp -> stale
	old := now.Add(-10 * time.Minute)
	tsOld := strconv.FormatInt(old.Unix(), 10)
	if err := os.WriteFile(path, []byte("pid=1\ntimestamp="+tsOld+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if !isStale(path, 5*time.Minute, func() time.Time { return now }) {
		t.Fatalf("isStale(old timestamp) = false, want true")
	}
}

// TestAcquireRemoveFails simulates a failure to remove a stale lock file by
// making the parent directory non-writable. Acquire should return ErrLocked
// when it cannot delete the stale lock.
func TestAcquireRemoveFails(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "no_rm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	t.Setenv("TMPDIR", dir)

	path := lockPath("https://example.invalid/res", "/tmp/out")
	staleTs := time.Now().Add(-10 * time.Minute).Unix()
	if err := os.WriteFile(path, []byte("pid=1\ntimestamp="+strconv.FormatInt(staleTs, 10)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Make directory non-writable so os.Remove(path) will fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	// Restore permissions & cleanup afterwards.
	defer func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.Remove(path)
	}()

	if _, err := Acquire("https://example.invalid/res", "/tmp/out", time.Minute, time.Now); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire() error = %v, want ErrLocked", err)
	}
}
