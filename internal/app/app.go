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

	s := &syncer.Syncer{Client: doer, Resource: cfg.ResourceURL, OutputPath: cfg.OutputPath}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	triggers := make(chan struct{}, 1)
	trigger := func() {
		select {
		case triggers <- struct{}{}:
		default:
		}
	}
	trigger()

	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()

	go func() {
		for {
			select {
			case <-signalCtx.Done():
				return
			case <-pollTicker.C:
				trigger()
			}
		}
	}()

	if cfg.EnableWebhook {
		go startWebhook(signalCtx, cfg.WebhookAddr, trigger, logger)
	}
	if cfg.EnableSSE {
		go startSSE(signalCtx, doer, cfg.SSEURL, trigger, logger)
	}

	for {
		select {
		case <-signalCtx.Done():
			return nil
		case <-triggers:
			if err := s.Sync(signalCtx); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				logger.Printf("sync failed: %v", err)
			}
		}
	}
}

func startWebhook(ctx context.Context, addr string, trigger func(), logger *log.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		trigger()
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("webhook server error: %v", err)
	}
}

func startSSE(ctx context.Context, doer httpclient.Doer, url string, trigger func(), logger *log.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
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

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				trigger()
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			logger.Printf("sse read error: %v", err)
		}
		_ = resp.Body.Close()

		if !sleepOrDone(ctx, 2*time.Second) {
			return
		}
	}
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
