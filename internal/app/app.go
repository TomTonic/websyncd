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

func LoadConfigFromEnv() (config.Config, error) {
	return config.LoadFromEnv()
}

func Run(ctx context.Context, cfg config.Config, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"starting websyncd: resource=%s output=%s poll_interval=%s webhook=%t sse=%t heartbeat_endpoint=%t heartbeat_interval=%s http3=%t",
		cfg.ResourceURL, cfg.OutputPath, cfg.PollInterval, cfg.EnableWebhook, cfg.EnableSSE, cfg.EnableHeartbeat, cfg.HeartbeatInterval, cfg.EnableHTTP3,
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
	go startHeartbeatLogger(signalCtx, cfg.HeartbeatInterval, health, logger)

	if cfg.EnableWebhook {
		go startWebhook(signalCtx, cfg.WebhookAddr, trigger, logger)
	}
	if cfg.EnableSSE {
		go startSSE(signalCtx, doer, cfg.SSEURL, trigger, logger)
	}
	if cfg.EnableHeartbeat {
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

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
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
	srv := &http.Server{Addr: addr, Handler: heartbeatHandler(state)}
	go func() {
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

func startHeartbeatLogger(ctx context.Context, interval time.Duration, state *healthState, logger *log.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := state.snapshot(time.Now())
			logger.Printf(
				"heartbeat: uptime=%s sync_total=%d success=%d failure=%d last_success=%s",
				s.Uptime.Truncate(time.Second), s.SyncTotal, s.SyncSuccess, s.SyncFailure, formatAge(s.LastSuccessAt, s.Now),
			)
		}
	}
}

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
