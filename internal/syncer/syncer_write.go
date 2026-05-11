package syncer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// writeResult describes the outcome of writing a downloaded response body to
// disk atomically, including metrics on transfer size, duration, and whether
// the local file was replaced.
type writeResult struct {
	TransferredBytes  int64
	TransferDuration  time.Duration
	NewFileSize       int64
	Replaced          bool
	ReplaceSkipReason string
}

// writeAtomically copies a response body to a temporary file, compares it with
// the existing target (if present), and atomically replaces the target if the
// content differs.
//
// The method enforces s.MaxDownloadBytes if configured, uses periodic
// progress logging via s.Logf, and emits detailed diagnostics for operational
// observability.
//
// Returns a writeResult describing the transfer metrics and replacement status,
// or an error if I/O operations fail.
func (s *Syncer) writeAtomically(body io.Reader) (writeResult, error) {
	result := writeResult{}
	dir := filepath.Dir(s.OutputPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return result, err
	}

	// Cap the response body to detect runaway responses before disk exhaustion.
	// We request MaxDownloadBytes+1 so that reading exactly at the limit still
	// succeeds; the post-loop check below surfaces the error if exceeded.
	if s.MaxDownloadBytes > 0 {
		limit := s.MaxDownloadBytes + 1
		if limit <= 0 { // overflow guard for MaxDownloadBytes == math.MaxInt64
			limit = s.MaxDownloadBytes
		}
		body = io.LimitReader(body, limit)
	}

	tmp, err := os.CreateTemp(dir, ".websyncd-*")
	if err != nil {
		return result, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	copyStarted := time.Now()
	lastProgress := copyStarted
	lastProgressBytes := int64(0)
	progressInterval := s.ProgressLogInterval
	if progressInterval <= 0 {
		progressInterval = 5 * time.Second
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			written, writeErr := tmp.Write(buf[:n])
			result.TransferredBytes += int64(written)
			if writeErr != nil {
				result.TransferDuration = time.Since(copyStarted)
				return result, writeErr
			}
			if written != n {
				result.TransferDuration = time.Since(copyStarted)
				return result, io.ErrShortWrite
			}
		}

		now := time.Now()
		if now.Sub(lastProgress) >= progressInterval {
			intervalDuration := now.Sub(lastProgress)
			intervalBytes := result.TransferredBytes - lastProgressBytes
			intervalRate := float64(intervalBytes) / intervalDuration.Seconds()
			avgRate := float64(result.TransferredBytes) / now.Sub(copyStarted).Seconds()
			s.logf(
				"sync download progress: transferred=%s interval_rate=%s avg_rate=%s elapsed=%s",
				formatBytes(result.TransferredBytes),
				formatRate(intervalRate),
				formatRate(avgRate),
				now.Sub(copyStarted).Truncate(time.Millisecond),
			)
			lastProgress = now
			lastProgressBytes = result.TransferredBytes
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.TransferDuration = time.Since(copyStarted)
			return result, readErr
		}
	}

	result.TransferDuration = time.Since(copyStarted)

	// Detect if the server sent more data than permitted.
	if s.MaxDownloadBytes > 0 && result.TransferredBytes > s.MaxDownloadBytes {
		return result, fmt.Errorf("response body exceeds maximum download size %s", formatBytes(s.MaxDownloadBytes))
	}

	if syncErr := tmp.Sync(); syncErr != nil {
		return result, syncErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return result, closeErr
	}

	newStat, err := os.Stat(tmpPath)
	if err != nil {
		return result, err
	}
	result.NewFileSize = newStat.Size()

	if equal, eqErr := filesEqualIfPresent(tmpPath, s.OutputPath); eqErr != nil {
		return result, eqErr
	} else if equal {
		result.Replaced = false
		result.ReplaceSkipReason = "downloaded content is identical to local file"
		s.logf("sync file replace skipped: reason=%q size=%s", result.ReplaceSkipReason, formatBytes(result.NewFileSize))
		cleanup = true
		return result, nil
	}

	if err := os.Rename(tmpPath, s.OutputPath); err != nil {
		return result, err
	}
	result.Replaced = true
	s.logf("sync file replaced atomically: new_size=%s", formatBytes(result.NewFileSize))
	cleanup = false
	return result, nil
}

// filesEqualIfPresent returns true if two files exist and have identical
// content. Returns false if the old file does not exist or differs.
func filesEqualIfPresent(newPath, oldPath string) (bool, error) {
	old, err := os.Open(oldPath) //nolint:gosec // G304: path comes from validated config and fixed output target
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = old.Close() }()

	newFile, err := os.Open(newPath) //nolint:gosec // G304: path is generated internally via CreateTemp
	if err != nil {
		return false, err
	}
	defer func() { _ = newFile.Close() }()

	newStat, err := newFile.Stat()
	if err != nil {
		return false, err
	}
	oldStat, err := old.Stat()
	if err != nil {
		return false, err
	}
	if newStat.Size() != oldStat.Size() {
		return false, nil
	}

	bufNew := make([]byte, 256*1024)
	bufOld := make([]byte, 256*1024)
	for {
		nNew, errNew := newFile.Read(bufNew)
		nOld, errOld := old.Read(bufOld)

		if nNew != nOld {
			return false, nil
		}
		if nNew > 0 && !bytes.Equal(bufNew[:nNew], bufOld[:nOld]) {
			return false, nil
		}

		if errNew == io.EOF && errOld == io.EOF {
			return true, nil
		}
		if errNew != nil && errNew != io.EOF {
			return false, errNew
		}
		if errOld != nil && errOld != io.EOF {
			return false, errOld
		}
		if errNew == io.EOF || errOld == io.EOF {
			return false, nil
		}
	}
}
