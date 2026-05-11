package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TomTonic/websyncd/internal/httpclient"
	"github.com/TomTonic/websyncd/internal/syncer"
)

func startWebhook(ctx context.Context, addr string, trigger func(string), logger *log.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		logger.Printf("webhook trigger received from %s", r.RemoteAddr)
		trigger("webhook")
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	go func() { //nolint:gosec // G118: shutdown timeout must be independent of the cancelled parent context
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Printf("webhook server listening on %s", addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("webhook server error: %v", err)
	}
}

func startSSE(ctx context.Context, doer httpclient.Doer, url string, trigger func(string), logger *log.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		logger.Printf("sse connecting to %s", url)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			logger.Printf("sse request build failed: %v", err)
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := doer.Do(req)
		if err != nil {
			logger.Printf("sse connection failed: %v", err)
			if !sleepOrDone(ctx, 3*time.Second) {
				return
			}
			continue
		}
		logger.Printf("sse connected")

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				trigger("sse")
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			logger.Printf("sse read error: %v", err)
		}
		_ = resp.Body.Close()
		if ctx.Err() == nil {
			logger.Printf("sse disconnected, retrying")
		}

		if !sleepOrDone(ctx, 2*time.Second) {
			return
		}
	}
}

func startHeartbeat(ctx context.Context, addr string, state *healthState, logger *log.Logger) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           heartbeatHandler(state),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	go func() { //nolint:gosec // G118: shutdown timeout must be independent of the cancelled parent context
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Printf("heartbeat endpoint listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("heartbeat server error: %v", err)
	}
}

func heartbeatHandler(state *healthState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s := state.snapshot(time.Now())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "status=ok\n")
		_, _ = io.WriteString(w, "uptime_seconds="+strconv.FormatInt(int64(s.Uptime.Seconds()), 10)+"\n")
		_, _ = io.WriteString(w, "sync_total="+strconv.FormatUint(s.SyncTotal, 10)+"\n")
		_, _ = io.WriteString(w, "sync_success="+strconv.FormatUint(s.SyncSuccess, 10)+"\n")
		_, _ = io.WriteString(w, "sync_failure="+strconv.FormatUint(s.SyncFailure, 10)+"\n")
		_, _ = io.WriteString(w, "last_sync_age="+formatAge(s.LastSyncAt, s.Now)+"\n")
		_, _ = io.WriteString(w, "last_success_age="+formatAge(s.LastSuccessAt, s.Now)+"\n")
		_, _ = io.WriteString(w, "last_failure_age="+formatAge(s.LastFailureAt, s.Now)+"\n")
		if s.LastError != "" {
			// Sanitise the error string: server-controlled content may contain
			// newlines that would inject fake key=value lines into the response.
			safeErr := strings.NewReplacer("\n", " ", "\r", " ").Replace(s.LastError)
			_, _ = io.WriteString(w, "last_error="+safeErr+"\n")
		}
	})
	return mux
}

func logSyncReport(logger *log.Logger, trigger string, elapsed time.Duration, report *syncer.SyncReport) {
	if report.DownloadPerformed {
		logger.Printf(
			"sync download: trigger=%s decision=%q protocol=%s bytes=%s duration=%s rate=%s",
			trigger,
			report.DownloadDecision,
			report.Protocol,
			formatBytes(report.TransferBytes),
			report.TransferDuration.Truncate(time.Millisecond),
			formatRate(report.TransferRateBytesPerSec),
		)
	} else {
		skipReason := report.DownloadSkipReason
		if skipReason == "" {
			skipReason = report.DownloadDecision
		}
		logger.Printf(
			"sync download skipped: trigger=%s protocol=%s reason=%q",
			trigger,
			report.Protocol,
			skipReason,
		)
	}

	action := "replaced"
	reason := "new content differs from local file"
	if !report.LocalReplacePerformed {
		action = "skipped"
		if report.LocalReplaceSkipReason != "" {
			reason = report.LocalReplaceSkipReason
		} else if !report.DownloadPerformed {
			reason = "no new download was required"
		}
	}

	prevSize := "none"
	if report.PreviousFileSize >= 0 {
		prevSize = formatBytes(report.PreviousFileSize)
	}

	freshness := "unknown"
	if report.FreshnessKnown {
		switch {
		case report.FreshnessDelta > 0:
			freshness = report.FreshnessDelta.Truncate(time.Second).String() + " newer"
		case report.FreshnessDelta < 0:
			freshness = (-report.FreshnessDelta).Truncate(time.Second).String() + " older"
		default:
			freshness = "same age"
		}
	}

	logger.Printf(
		"sync file result: action=%s reason=%q previous_size=%s new_size=%s size_delta=%+s freshness=%s total_elapsed=%s",
		action,
		reason,
		prevSize,
		formatBytes(report.NewFileSize),
		formatBytesSigned(report.SizeDeltaBytes),
		freshness,
		elapsed.Truncate(time.Millisecond),
	)
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

func formatBytesSigned(n int64) string {
	if n < 0 {
		return "-" + formatBytes(-n)
	}
	return "+" + formatBytes(n)
}

func formatRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s/s", formatBytes(int64(bytesPerSec)))
}

func formatAge(at, now time.Time) string {
	if at.IsZero() {
		return "never"
	}
	age := now.Sub(at)
	if age < 0 {
		age = 0
	}
	return age.Truncate(time.Second).String()
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
