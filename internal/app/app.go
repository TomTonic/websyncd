// Package app wires together the websyncd components and runs the daemon loop.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
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

	s := &syncer.Syncer{
		Client:              doer,
		Resource:            cfg.ResourceURL,
		OutputPath:          cfg.OutputPath,
		Logf:                logger.Printf,
		ProgressLogInterval: cfg.DownloadProgressInterval,
		MaxDownloadBytes:    cfg.MaxDownloadBytes,
	}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	health := newHealthState(time.Now())

	triggers := make(chan string, 1)
	trigger := func(source string) {
		select {
		case triggers <- source:
			logger.Printf("trigger queued: source=%s", source)
		default:
			logger.Printf("trigger coalesced: source=%s reason=another trigger already pending", source)
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
			logger.Printf("sync starting: trigger_source=%s", source)
			started := time.Now()
			health.recordSyncStart(started)
			report, err := s.SyncWithReport(signalCtx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				logger.Printf("sync failed after %s: %v", time.Since(started), err)
				health.recordSyncFailure(time.Now(), err)
				continue
			}
			health.recordSyncSuccess(time.Now())
			logSyncReport(logger, source, time.Since(started), &report)
		}
	}
}

// healthState tracks sync execution metrics for operational health checks.
// All methods are safe for concurrent use.
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

// healthSnapshot is a point-in-time view of health metrics.
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
