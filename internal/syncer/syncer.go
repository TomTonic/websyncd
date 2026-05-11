// Package syncer downloads a remote resource to a local file using conditional
// HTTP requests (ETag / Last-Modified) to avoid redundant transfers.
package syncer

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPDoer is the minimal HTTP interface required by Syncer. *http.Client and
// httpclient.Doer both satisfy this interface.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Syncer downloads a remote resource to a local file, using HTTP conditional
// requests (ETag / Last-Modified) to avoid redundant transfers.
//
// Client must be set before calling Sync; all other fields are optional.
// Resource is the URL of the remote resource. OutputPath is the absolute path
// where the downloaded content is written atomically.
//
// A single Syncer instance should be reused across calls so that it can
// accumulate cache validators between syncs.
type Syncer struct {
	Client              HTTPDoer
	Resource            string
	OutputPath          string
	Logf                func(format string, args ...any)
	ProgressLogInterval time.Duration
	MaxDownloadBytes    int64

	etag         string
	lastModified string
}

// SyncReport describes what happened during a sync attempt.
//
// It is intended for operational logging and troubleshooting so callers can
// explain why downloads or local file replacements were performed or skipped.
type SyncReport struct {
	DownloadPerformed       bool
	DownloadSkipReason      string
	DownloadDecision        string
	LocalReplacePerformed   bool
	LocalReplaceSkipReason  string
	Protocol                string
	TransferBytes           int64
	TransferDuration        time.Duration
	TransferRateBytesPerSec float64
	PreviousFileSize        int64
	NewFileSize             int64
	SizeDeltaBytes          int64
	RemoteLastModifiedRaw   string
	RemoteLastModified      time.Time
	FreshnessDelta          time.Duration
	FreshnessKnown          bool
}

// Sync fetches the remote resource and writes it atomically to OutputPath if the
// content has changed since the last successful sync.
//
// ctx controls the request lifetime; cancellation aborts in-flight HTTP calls
// and any partial write, leaving the existing OutputPath intact.
//
// On the first call, Sync issues a HEAD request to obtain cache validators, then
// a GET to retrieve the body. Subsequent calls send conditional headers
// (If-None-Match, If-Modified-Since) so that an unchanged resource results in a
// single round-trip with no file I/O.
//
// Returns nil on success (including when the resource is unchanged). Returns an
// error if the HTTP request fails, the server returns a non-2xx status, or the
// atomic write fails.
func (s *Syncer) Sync(ctx context.Context) error {
	report, err := s.SyncWithReport(ctx)
	if err != nil {
		return err
	}
	_ = report
	return nil
}

// SyncWithReport executes a sync and returns a detailed SyncReport describing
// the decisions taken (download vs skip, replace vs skip), transfer metrics,
// and file-version deltas useful for logs and observability.
func (s *Syncer) SyncWithReport(ctx context.Context) (SyncReport, error) {
	report := SyncReport{PreviousFileSize: -1, NewFileSize: -1}
	var previousModTime time.Time
	if st, statErr := os.Stat(s.OutputPath); statErr == nil {
		report.PreviousFileSize = st.Size()
		previousModTime = st.ModTime()
	}

	if s.Client == nil {
		return report, fmt.Errorf("http client is required")
	}

	headResp, needGET, decision, err := s.head(ctx)
	if err == nil && headResp != nil {
		defer func() { _ = headResp.Body.Close() }()
		report.Protocol = responseProtocol(headResp)
		s.logf("sync HEAD response: status=%s protocol=%s decision=%q", headResp.Status, report.Protocol, decision)
	}
	report.DownloadDecision = decision
	if err != nil {
		report.DownloadDecision = fmt.Sprintf("HEAD failed (%v), falling back to GET", err)
		s.logf("sync HEAD failed: error=%v fallback=GET", err)
		needGET = true
	}
	if !needGET {
		report.DownloadPerformed = false
		report.DownloadSkipReason = decision
		if report.PreviousFileSize >= 0 {
			report.NewFileSize = report.PreviousFileSize
			report.SizeDeltaBytes = 0
		}
		s.logf("sync download skipped after HEAD: reason=%q protocol=%s", decision, report.Protocol)
		return report, nil
	}

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Resource, nil)
	if err != nil {
		return report, err
	}
	getResp, err := s.Client.Do(getReq)
	if err != nil {
		return report, err
	}
	defer func() { _ = getResp.Body.Close() }()
	report.Protocol = responseProtocol(getResp)
	s.logf("sync GET response: status=%s protocol=%s", getResp.Status, report.Protocol)
	if getResp.StatusCode == http.StatusNotModified {
		report.DownloadPerformed = false
		report.DownloadSkipReason = "GET returned 304 Not Modified"
		if report.PreviousFileSize >= 0 {
			report.NewFileSize = report.PreviousFileSize
			report.SizeDeltaBytes = 0
		}
		s.logf("sync download skipped after GET: reason=%q protocol=%s", report.DownloadSkipReason, report.Protocol)
		return report, nil
	}
	if getResp.StatusCode < 200 || getResp.StatusCode >= 300 {
		err = fmt.Errorf("GET %s failed: %s", s.Resource, getResp.Status)
		s.logf("sync GET failed before body transfer: protocol=%s error=%v", report.Protocol, err)
		return report, err
	}

	writeResult, err := s.writeAtomically(getResp.Body)
	report.TransferBytes = writeResult.TransferredBytes
	report.TransferDuration = writeResult.TransferDuration
	if writeResult.TransferDuration > 0 {
		report.TransferRateBytesPerSec = float64(writeResult.TransferredBytes) / writeResult.TransferDuration.Seconds()
	}
	if err != nil {
		s.logf(
			"sync download failed: protocol=%s transferred=%s duration=%s avg_rate=%s error=%v",
			report.Protocol,
			formatBytes(report.TransferBytes),
			report.TransferDuration.Truncate(time.Millisecond),
			formatRate(report.TransferRateBytesPerSec),
			err,
		)
		return report, err
	}
	report.DownloadPerformed = true
	report.LocalReplacePerformed = writeResult.Replaced
	report.LocalReplaceSkipReason = writeResult.ReplaceSkipReason
	report.NewFileSize = writeResult.NewFileSize
	s.logf(
		"sync download completed: protocol=%s transferred=%s duration=%s avg_rate=%s",
		report.Protocol,
		formatBytes(report.TransferBytes),
		report.TransferDuration.Truncate(time.Millisecond),
		formatRate(report.TransferRateBytesPerSec),
	)
	if report.PreviousFileSize >= 0 {
		report.SizeDeltaBytes = writeResult.NewFileSize - report.PreviousFileSize
	} else {
		report.SizeDeltaBytes = writeResult.NewFileSize
	}
	s.etag = strings.TrimSpace(getResp.Header.Get("ETag"))
	s.lastModified = strings.TrimSpace(getResp.Header.Get("Last-Modified"))
	report.RemoteLastModifiedRaw = s.lastModified
	if report.RemoteLastModifiedRaw != "" {
		if parsed, parseErr := http.ParseTime(report.RemoteLastModifiedRaw); parseErr == nil {
			report.RemoteLastModified = parsed
			if !previousModTime.IsZero() {
				report.FreshnessKnown = true
				report.FreshnessDelta = parsed.Sub(previousModTime)
			}
		}
	}

	return report, nil
}

func (s *Syncer) head(ctx context.Context) (resp *http.Response, needGet bool, reason string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.Resource, nil)
	if err != nil {
		return nil, false, "failed to build HEAD request", err
	}
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	if s.lastModified != "" {
		req.Header.Set("If-Modified-Since", s.lastModified)
	}

	resp, err = s.Client.Do(req)
	if err != nil {
		return nil, false, "HEAD request failed", err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return resp, true, "server does not support HEAD, using GET", nil
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, false, "HEAD returned 304 Not Modified", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, true, fmt.Sprintf("HEAD returned %s, using GET", resp.Status), nil
	}

	headETag := strings.TrimSpace(resp.Header.Get("ETag"))
	headLM := strings.TrimSpace(resp.Header.Get("Last-Modified"))

	if s.etag != "" && headETag != "" && s.etag == headETag {
		return resp, false, "HEAD ETag matches previous sync", nil
	}
	if s.lastModified != "" && headLM != "" && s.lastModified == headLM {
		return resp, false, "HEAD Last-Modified matches previous sync", nil
	}

	return resp, true, "HEAD indicates possible resource change, downloading", nil
}

type writeResult struct {
	TransferredBytes  int64
	TransferDuration  time.Duration
	NewFileSize       int64
	Replaced          bool
	ReplaceSkipReason string
}

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

func (s *Syncer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func formatBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s/s", formatBytes(int64(bytesPerSec)))
}

func responseProtocol(resp *http.Response) string {
	if resp == nil {
		return "unknown"
	}

	// Prefer numeric proto version when available (e.g. HTTP/2.0).
	var protoLabel string
	switch {
	case resp.ProtoMajor > 0 && resp.ProtoMajor == 3:
		protoLabel = "HTTP/3"
	case resp.ProtoMajor > 0:
		protoLabel = fmt.Sprintf("HTTP/%d.%d", resp.ProtoMajor, resp.ProtoMinor)
	case resp.Proto != "":
		protoLabel = resp.Proto
	default:
		protoLabel = "HTTP"
	}

	// If TLS was negotiated, append a human-friendly TLS version.
	if resp.TLS != nil {
		tlsLabel := "TLS"
		switch resp.TLS.Version {
		case tls.VersionTLS13:
			tlsLabel = "TLS 1.3"
		case tls.VersionTLS12:
			tlsLabel = "TLS 1.2"
		case tls.VersionTLS11:
			tlsLabel = "TLS 1.1"
		case tls.VersionTLS10:
			tlsLabel = "TLS 1.0"
		}
		if resp.ProtoMajor == 3 {
			return fmt.Sprintf("%s (QUIC) with %s", protoLabel, tlsLabel)
		}
		return fmt.Sprintf("%s with %s", protoLabel, tlsLabel)
	}

	return protoLabel
}
