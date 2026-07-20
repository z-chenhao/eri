// Package approval owns user decisions that are bound to one exact effect
// intent. An approval authorizes a durable continuation; it never executes an
// effect directly.
package approval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/z-chenhao/eri/internal/observability"
)

type Decision string

const (
	Approve Decision = "approve"
	Deny    Decision = "deny"
)

type Result struct {
	ApprovalID string `json:"approval_id"`
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	GrantID    string `json:"grant_id,omitempty"`
}

type ExpiryRepository interface {
	ExpireApprovals(context.Context, time.Time, int) (int, error)
}

type ExpiryWorker struct {
	repository ExpiryRepository
	interval   time.Duration
	logger     *slog.Logger
}

func NewExpiryWorker(repository ExpiryRepository, interval time.Duration, logger *slog.Logger) *ExpiryWorker {
	if interval <= 0 {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ExpiryWorker{repository: repository, interval: interval, logger: logger}
}

func (w *ExpiryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if expired, err := w.repository.ExpireApprovals(ctx, time.Now().UTC(), 50); err != nil && ctx.Err() == nil {
			w.logger.Error("approval expiry scan failed", "component", "approval", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		} else if expired > 0 {
			w.logger.Info("approvals expired", "component", "approval", "count", expired)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type Repository interface {
	ResolveApproval(context.Context, string, Decision) (Result, error)
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Decide(ctx context.Context, id string, decision Decision) (Result, error) {
	if id == "" {
		return Result{}, fmt.Errorf("approval id is required")
	}
	if decision != Approve && decision != Deny {
		return Result{}, fmt.Errorf("decision must be approve or deny")
	}
	return s.repository.ResolveApproval(ctx, id, decision)
}
