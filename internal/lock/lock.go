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

var ErrLocked = errors.New("lock already held")

type Clock func() time.Time

type Lock struct {
	path string
}

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
