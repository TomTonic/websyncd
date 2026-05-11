package lock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrLocked is returned by Acquire when a live (non-stale) lock file already
// exists for the given resource/output combination.
var ErrLocked = errors.New("lock already held")

// Clock is a function that returns the current time. Passing a custom Clock to
// Acquire allows tests to control lock expiry without real-time delays.
type Clock func() time.Time

// Lock represents an exclusive file-system lock acquired by Acquire. It must be
// released via Release when the work it guards is complete.
type Lock struct {
	path string
}

// Acquire creates an exclusive lock for the given url+outputPath pair.
//
// url and outputPath are hashed together to produce a unique lock-file name
// inside os.TempDir(), so two Acquire calls with different combinations do not
// interfere.
//
// ttl is the maximum age of an existing lock before it is considered stale and
// removed automatically; pass 0 to use the default of 5 minutes.
//
// now is a Clock used to compare timestamps; pass time.Now for production use.
// Passing a custom Clock is useful in tests to simulate stale locks.
//
// Returns a *Lock on success. Returns ErrLocked if a live lock already exists.
// Returns another error if the file system operation fails.
//
// The caller must call Release on the returned Lock when done.
func Acquire(url, outputPath string, ttl time.Duration, now Clock) (*Lock, error) {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	path := lockPath(url, outputPath)
	content := fmt.Sprintf("pid=%d\ntimestamp=%d\n", os.Getpid(), now().Unix())

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, writeErr := f.WriteString(content)
			closeErr := f.Close()
			if writeErr != nil {
				_ = os.Remove(path)
				return nil, writeErr
			}
			if closeErr != nil {
				_ = os.Remove(path)
				return nil, closeErr
			}
			return &Lock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		if !isStale(path, ttl, now) {
			return nil, ErrLocked
		}

		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, ErrLocked
		}
	}

	return nil, ErrLocked
}

// Release deletes the lock file, allowing another process to acquire it.
//
// Release is idempotent: calling it on a nil receiver or after the file has
// already been removed returns nil. It is safe to call via defer.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func lockPath(url, outputPath string) string {
	sum := sha256.Sum256([]byte(url + "|" + outputPath))
	name := "websyncd-" + hex.EncodeToString(sum[:]) + ".lock"
	return filepath.Join(os.TempDir(), name)
}

func isStale(path string, ttl time.Duration, now Clock) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "timestamp=") {
			continue
		}
		ts := strings.TrimPrefix(line, "timestamp=")
		unix, convErr := strconv.ParseInt(ts, 10, 64)
		if convErr != nil {
			return true
		}
		return now().Sub(time.Unix(unix, 0)) > ttl
	}
	return true
}
