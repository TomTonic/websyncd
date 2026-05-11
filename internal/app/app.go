// Package app wires together the websyncd components and runs the daemon loop.
package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/TomTonic/websyncd/internal/config"
	"github.com/TomTonic/websyncd/internal/httpclient"
	"github.com/TomTonic/websyncd/internal/lock"
	"github.com/TomTonic/websyncd/internal/syncer"
)

// LoadConfigFromEnv reads the runtime configuration from environment variables.
//
// It is a thin wrapper around config.LoadFromEnv provided so that cmd/websyncd
// only needs to import the app package.
//
// Returns a populated config.Config, or an error if any required variable is
// absent or invalid.
func LoadConfigFromEnv() (config.Config, error) {
	return config.LoadFromEnv()
}

// Run starts the websyncd daemon and blocks until ctx is cancelled or a shutdown
// signal (SIGINT, SIGTERM) is received.
//
// cfg is a pointer to a fully validated Config, typically obtained from
// LoadConfigFromEnv. logger receives all diagnostic output; pass nil to use
// log.Default().
//
// Run acquires an exclusive lock for the given resource/output combination and
// returns an error immediately if another instance already holds it. Once
// running, it polls cfg.ResourceURL on cfg.PollInterval and optionally accepts
// additional sync triggers via an HTTP webhook or Server-Sent Events stream.
//
// Returns nil on clean shutdown. Returns an error if the lock cannot be
// acquired or if a fatal initialisation step fails.
func Run(ctx context.Context, cfg *config.Config, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"starting websyncd: resource=%s output=%s poll_interval=%s webhook_addr=%q resource_event_url=%q heartbeat_addr=%q http3=%t",
		cfg.ResourceURL, cfg.OutputPath, cfg.PollInterval, cfg.WebhookAddr, cfg.ResourceEventURL, cfg.HeartbeatAddr, cfg.EnableHTTP3,
	)

	doer, closeClient := httpclient.New(cfg.HTTPTimeout, cfg.EnableHTTP3)
	defer func() { _ = closeClient() }()

	l, err := lock.Acquire(cfg.ResourceURL, cfg.OutputPath, cfg.LockTTL, time.Now)
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			return fmt.Errorf("another instance is already syncing this resource")
		}
		return err
	}
	defer func() { _ = l.Release() }()
	logger.Printf("lock acquired")

	s := &syncer.Syncer{Client: doer, Resource: cfg.ResourceURL, OutputPath: cfg.OutputPath}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	health := newHealthState(time.Now())

	triggers := make(chan string, 1)
	trigger := func(source string) {
		select {
		case triggers <- source:
		default:
		}
	}
	trigger("startup")

	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()

	go func() {
		for {
			select {
			case <-signalCtx.Done():
				return
			case <-pollTicker.C:
				trigger("poll")
			}
		}
	}()
	// heartbeat log messages removed; only heartbeat endpoint (if enabled)

	// Start webhook server only when WEBHOOK_ADDR is explicitly set.
	if cfg.WebhookAddr != "" {
		go startWebhook(signalCtx, cfg.WebhookAddr, trigger, logger)
	}
	if cfg.ResourceEventURL != "" {
		go startSSE(signalCtx, doer, cfg.ResourceEventURL, trigger, logger)
	}
	// Start heartbeat endpoint only when HEARTBEAT_ADDR is explicitly set.
	if cfg.HeartbeatAddr != "" {
		go startHeartbeat(signalCtx, cfg.HeartbeatAddr, health, logger)
	}

	for {
		select {
		case <-signalCtx.Done():
			logger.Printf("shutdown signal received")
			return nil
		case source := <-triggers:
			logger.Printf("sync triggered by %s", source)
			started := time.Now()
			health.recordSyncStart(started)
			if err := s.Sync(signalCtx); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				logger.Printf("sync failed after %s: %v", time.Since(started), err)
				health.recordSyncFailure(time.Now(), err)
				continue
			}
			health.recordSyncSuccess(time.Now())
			logger.Printf("sync completed in %s", time.Since(started))
		}
	}
}

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

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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
	srv := &http.Server{Addr: addr, Handler: heartbeatHandler(state), ReadHeaderTimeout: 5 * time.Second}
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
			_, _ = io.WriteString(w, "last_error="+s.LastError+"\n")
		}
	})
	return mux
}

// heartbeat logging removed; liveness should be provided via the configured
// heartbeat endpoint or an external socket-based liveness check.

type healthState struct {
	mu            sync.RWMutex
	startedAt     time.Time
	lastSyncAt    time.Time
	lastSuccessAt time.Time
	lastFailureAt time.Time
	lastError     string
	syncTotal     uint64
	syncSuccess   uint64
	syncFailure   uint64
}

func newHealthState(now time.Time) *healthState {
	return &healthState{startedAt: now}
}

func (s *healthState) recordSyncStart(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncTotal++
	s.lastSyncAt = now
}

func (s *healthState) recordSyncSuccess(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncSuccess++
	s.lastSuccessAt = now
	s.lastError = ""
}

func (s *healthState) recordSyncFailure(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncFailure++
	s.lastFailureAt = now
	if err != nil {
		s.lastError = err.Error()
	}
}

type healthSnapshot struct {
	Now           time.Time
	Uptime        time.Duration
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	LastFailureAt time.Time
	LastError     string
	SyncTotal     uint64
	SyncSuccess   uint64
	SyncFailure   uint64
}

func (s *healthState) snapshot(now time.Time) healthSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return healthSnapshot{
		Now:           now,
		Uptime:        now.Sub(s.startedAt),
		LastSyncAt:    s.lastSyncAt,
		LastSuccessAt: s.lastSuccessAt,
		LastFailureAt: s.lastFailureAt,
		LastError:     s.lastError,
		SyncTotal:     s.syncTotal,
		SyncSuccess:   s.syncSuccess,
		SyncFailure:   s.syncFailure,
	}
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
