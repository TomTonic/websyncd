// Package syncer downloads a remote resource to a local file using conditional
// HTTP requests (ETag / Last-Modified) to avoid redundant transfers.
package syncer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
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
	Client                  HTTPDoer
	Resource                string
	OutputPath              string
	Logf                    func(format string, args ...any)
	ProgressLogInterval     time.Duration
	MaxDownloadBytes        int64
	OutputFileAttributesSet bool
	OutputFileMode          os.FileMode
	OutputFileUID           int
	OutputFileGID           int

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
