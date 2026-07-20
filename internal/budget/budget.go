// Package budget enforces durable resource ceilings before paid model calls.
package budget

import (
	"context"
	"errors"
	"fmt"
)

var ErrExhausted = errors.New("cloud model token budget exhausted")

type Limits struct {
	PerTask  int
	PerDay   int
	PerMonth int
}

type Repository interface {
	ReserveModelTokens(context.Context, string, int, Limits) (string, error)
	SettleModelTokens(context.Context, string, int, bool) error
}

type Service struct {
	repository Repository
	limits     Limits
}

func NewService(repository Repository, limits Limits) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("budget repository is required")
	}
	if limits.PerTask <= 0 || limits.PerDay <= 0 || limits.PerMonth <= 0 {
		return nil, fmt.Errorf("all cloud model token limits must be positive")
	}
	if limits.PerTask > limits.PerDay || limits.PerDay > limits.PerMonth {
		return nil, fmt.Errorf("cloud token limits must satisfy task <= day <= month")
	}
	return &Service{repository: repository, limits: limits}, nil
}

func (s *Service) Reserve(ctx context.Context, taskID string, estimatedTokens int) (string, error) {
	if estimatedTokens <= 0 {
		return "", fmt.Errorf("estimated tokens must be positive")
	}
	return s.repository.ReserveModelTokens(ctx, taskID, estimatedTokens, s.limits)
}

func (s *Service) Settle(ctx context.Context, reservationID string, actualTokens int, confirmed bool) error {
	if reservationID == "" {
		return fmt.Errorf("reservation id is required")
	}
	if actualTokens < 0 {
		return fmt.Errorf("actual tokens cannot be negative")
	}
	return s.repository.SettleModelTokens(ctx, reservationID, actualTokens, confirmed)
}
