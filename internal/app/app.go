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

const (
	daemonHeartbeatInterval = 5 * time.Second
)

type reportSyncer interface {
	SyncWithReport(ctx context.Context) (syncer.SyncReport, error)
}

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
		Client:                  doer,
		Resource:                cfg.ResourceURL,
		OutputPath:              cfg.OutputPath,
		Logf:                    logger.Printf,
		ProgressLogInterval:     cfg.DownloadProgressInterval,
		MaxDownloadBytes:        cfg.MaxDownloadBytes,
		OutputFileAttributesSet: cfg.OutputFileAttributesSet,
		OutputFileMode:          cfg.OutputFileMode,
		OutputFileUID:           cfg.OutputFileUID,
		OutputFileGID:           cfg.OutputFileGID,
	}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	health := newHealthState(time.Now())

	triggers := make(chan string, 1)
	trigger := makeTriggerSender(triggers, logger)
	trigger("startup")

	heartbeatTicker := time.NewTicker(daemonHeartbeatInterval)
	defer heartbeatTicker.Stop()
	go startHeartbeatAndPollTrigger(signalCtx, heartbeatTicker.C, health, cfg.PollInterval, trigger, logger)

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

	runSyncLoop(signalCtx, triggers, s, health, logger)
	return nil
}

// makeTriggerSender builds a coalescing trigger sender for sync events.
func makeTriggerSender(triggers chan<- string, logger *log.Logger) func(string) {
	return func(source string) {
		select {
		case triggers <- source:
			logger.Printf("trigger queued: source=%s", source)
		default:
			logger.Printf("trigger coalesced: source=%s reason=another trigger already pending", source)
		}
	}
}

// startHeartbeatAndPollTrigger emits a fast heartbeat every daemonHeartbeatInterval
// and subsamples it to trigger poll at the configured poll interval. This decouples
// liveness monitoring (fast heartbeat) from polling frequency (possibly long interval).
func startHeartbeatAndPollTrigger(ctx context.Context, ticks <-chan time.Time, health *healthState, pollInterval time.Duration, trigger func(string), _ *log.Logger) {
	ticksSincePoll := int64(0)
	pollEveryTicks := pollInterval / daemonHeartbeatInterval
	if pollInterval%daemonHeartbeatInterval != 0 {
		pollEveryTicks++
	}
	if pollEveryTicks < 1 {
		pollEveryTicks = 1
	}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			health.recordLoopBeat(now)
			ticksSincePoll++
			if ticksSincePoll >= int64(pollEveryTicks) {
				ticksSincePoll = 0
				trigger("poll")
			}
		}
	}
}

// runSyncLoop processes incoming trigger events until shutdown.
func runSyncLoop(ctx context.Context, triggers <-chan string, s reportSyncer, health *healthState, logger *log.Logger) {
	for {
		select {
		case <-ctx.Done():
			logger.Printf("shutdown signal received")
			return
		case source := <-triggers:
			if handleTrigger(ctx, s, health, logger, source) {
				return
			}
		}
	}
}

// handleTrigger processes a sync trigger by executing a sync operation and
// recording results in the health state. It runs synchronously to completion
// and logs diagnostic information via the logger.
//
// ctx is the context that should be passed to SyncWithReport; if cancelled
// during sync, this function returns early after reporting cancellation.
//
// source is the trigger source for logging (e.g., "webhook", "poll").
// Returns true if the sync succeeded or was cancelled (caller should exit);
// returns false if a recoverable error occurred and processing should continue.
func handleTrigger(ctx context.Context, s reportSyncer, health *healthState, logger *log.Logger, source string) bool {
	logger.Printf("sync starting: trigger_source=%s", source)
	started := time.Now()
	health.recordSyncStart(started)
	report, err := s.SyncWithReport(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return true // caller should exit
		}
		logger.Printf("sync failed after %s: %v", time.Since(started), err)
		health.recordSyncFailure(time.Now(), err)
		return false // recoverable error
	}
	health.recordSyncSuccess(time.Now())
	logSyncReport(logger, source, time.Since(started), &report)
	return false // continue processing
}

// healthState tracks sync execution metrics for operational health checks.
// All methods are safe for concurrent use.
type healthState struct {
	mu            sync.RWMutex
	startedAt     time.Time
	lastLoopBeat  time.Time
	lastSyncAt    time.Time
	lastSuccessAt time.Time
	lastFailureAt time.Time
	lastError     string
	syncTotal     uint64
	syncSuccess   uint64
	syncFailure   uint64
}

func newHealthState(now time.Time) *healthState {
	return &healthState{startedAt: now, lastLoopBeat: now}
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

func (s *healthState) recordLoopBeat(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastLoopBeat = now
}

// healthSnapshot is a point-in-time view of health metrics.
type healthSnapshot struct {
	Now           time.Time
	Uptime        time.Duration
	LastLoopBeat  time.Time
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
		LastLoopBeat:  s.lastLoopBeat,
		LastSyncAt:    s.lastSyncAt,
		LastSuccessAt: s.lastSuccessAt,
		LastFailureAt: s.lastFailureAt,
		LastError:     s.lastError,
		SyncTotal:     s.syncTotal,
		SyncSuccess:   s.syncSuccess,
		SyncFailure:   s.syncFailure,
	}
}
