// Package runtime advances durable work from a transactional outbox.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/z-chenhao/eri/internal/observability"
)

// OutboxItem is a dispatch request committed with domain state.
type OutboxItem struct {
	ID          string
	Kind        string
	AggregateID string
	Attempts    int
}

// Queue is defined by the worker that consumes durable dispatch requests.
type Queue interface {
	ClaimOutbox(context.Context, string, time.Duration) (OutboxItem, bool, error)
	CompleteOutbox(context.Context, string) error
	RetryOutbox(context.Context, string, string, time.Time) error
	RenewOutboxLease(context.Context, string, string, time.Duration) error
}

// Handler performs one idempotent dispatch.
type Handler func(context.Context, OutboxItem) error

// Worker polls and advances outbox items. It never treats an unknown handler
// as success and applies bounded exponential retry delays.
type Worker struct {
	queue        Queue
	handlers     map[string]Handler
	owner        string
	pollInterval time.Duration
	lease        time.Duration
	concurrency  int
	logger       *slog.Logger
}

func NewWorker(queue Queue, handlers map[string]Handler, owner string, pollInterval time.Duration, logger *slog.Logger) *Worker {
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		queue:        queue,
		handlers:     handlers,
		owner:        owner,
		pollInterval: pollInterval,
		lease:        2 * time.Minute,
		concurrency:  4,
		logger:       logger,
	}
}

func (w *Worker) SetConcurrency(limit int) {
	if limit > 0 {
		w.concurrency = limit
	}
}

func (w *Worker) SetLease(lease time.Duration) {
	if lease > 0 {
		w.lease = lease
	}
}

// Run blocks until the context is canceled or the durable queue fails.
func (w *Worker) Run(ctx context.Context) error {
	workers := w.concurrency
	if workers <= 0 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errors := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func() { errors <- w.runLane(ctx) }()
	}
	for index := 0; index < workers; index++ {
		if err := <-errors; err != nil {
			cancel()
			return err
		}
	}
	return nil
}

func (w *Worker) runLane(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		processed, err := w.processOne(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) processOne(ctx context.Context) (bool, error) {
	item, ok, err := w.queue.ClaimOutbox(ctx, w.owner, w.lease)
	if err != nil {
		return false, fmt.Errorf("claim outbox: %w", err)
	}
	if !ok {
		return false, nil
	}
	started := time.Now()
	w.logger.Info("outbox dispatch started", "component", "runtime", "outbox_id", item.ID, "kind", item.Kind, "aggregate_id", item.AggregateID, "attempt", item.Attempts+1)
	handler, exists := w.handlers[item.Kind]
	if !exists {
		err = fmt.Errorf("no handler for outbox kind %q", item.Kind)
	} else {
		err = w.handleWithLease(ctx, item, handler)
	}
	if err == nil {
		if completeErr := w.queue.CompleteOutbox(ctx, item.ID); completeErr != nil {
			return true, fmt.Errorf("complete outbox %s: %w", item.ID, completeErr)
		}
		w.logger.Info("outbox dispatch completed", "component", "runtime", "outbox_id", item.ID, "kind", item.Kind, "aggregate_id", item.AggregateID, "attempt", item.Attempts+1, "duration_ms", time.Since(started).Milliseconds())
		return true, nil
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return true, nil
	}
	delay := retryDelay(item.Attempts + 1)
	w.logger.Warn("outbox dispatch failed", "component", "runtime", "outbox_id", item.ID, "kind", item.Kind, "aggregate_id", item.AggregateID, "attempt", item.Attempts+1, "retry_in", delay, "duration_ms", time.Since(started).Milliseconds(), "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
	if retryErr := w.queue.RetryOutbox(ctx, item.ID, safeError(err), time.Now().UTC().Add(delay)); retryErr != nil {
		return true, fmt.Errorf("retry outbox %s: %w", item.ID, retryErr)
	}
	return true, nil
}

func (w *Worker) handleWithLease(ctx context.Context, item OutboxItem, handler Handler) error {
	handlerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- handler(handlerCtx, item) }()
	interval := w.lease / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			cancel()
			return ctx.Err()
		case <-ticker.C:
			if err := w.queue.RenewOutboxLease(ctx, item.ID, w.owner, w.lease); err != nil {
				cancel()
				select {
				case <-result:
				case <-time.After(time.Second):
				}
				return fmt.Errorf("renew outbox lease: %w", err)
			}
		}
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * 250 * time.Millisecond
}

func safeError(err error) string {
	return observability.SafeError(err)
}
