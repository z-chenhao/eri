package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/budget"
	"github.com/z-chenhao/eri/internal/identifier"
)

func (s *Store) ReserveModelTokens(ctx context.Context, taskID string, estimated int, limits budget.Limits) (string, error) {
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	sum := func(query string, arguments ...any) (int, error) {
		var value int
		return value, tx.QueryRowContext(ctx, query, arguments...).Scan(&value)
	}
	taskUsed, err := sum(`SELECT COALESCE(SUM(charged_tokens), 0) FROM model_budget_usage WHERE task_id = ?`, taskID)
	if err != nil {
		return "", err
	}
	dayUsed, err := sum(`SELECT COALESCE(SUM(charged_tokens), 0) FROM model_budget_usage WHERE created_at >= ?`, formatTime(dayStart))
	if err != nil {
		return "", err
	}
	monthUsed, err := sum(`SELECT COALESCE(SUM(charged_tokens), 0) FROM model_budget_usage WHERE created_at >= ?`, formatTime(monthStart))
	if err != nil {
		return "", err
	}
	if taskUsed+estimated > limits.PerTask || dayUsed+estimated > limits.PerDay || monthUsed+estimated > limits.PerMonth {
		return "", budget.ErrExhausted
	}
	id, err := identifier.New()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_budget_usage(id, task_id, estimated_tokens, charged_tokens, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'reserved', ?, ?)`, id, taskID, estimated, estimated, formatTime(now), formatTime(now)); err != nil {
		return "", err
	}
	if err := appendEvent(ctx, tx, "budget", id, "budget.tokens_reserved", map[string]any{
		"task_id": taskID, "estimated_tokens": estimated,
		"task_remaining":  limits.PerTask - taskUsed - estimated,
		"day_remaining":   limits.PerDay - dayUsed - estimated,
		"month_remaining": limits.PerMonth - monthUsed - estimated,
	}, now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) SettleModelTokens(ctx context.Context, id string, actual int, confirmed bool) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "unknown"
	if confirmed {
		status = "settled"
	}
	var estimated int
	err = tx.QueryRowContext(ctx, `SELECT estimated_tokens FROM model_budget_usage WHERE id = ? AND status = 'reserved'`, id).Scan(&estimated)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("model budget reservation not found or already settled")
	}
	if err != nil {
		return err
	}
	charged := estimated
	if confirmed {
		charged = actual
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE model_budget_usage SET actual_tokens = ?, charged_tokens = ?, status = ?, updated_at = ?
		WHERE id = ? AND status = 'reserved'`, actual, charged, status, formatTime(now), id); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "budget", id, "budget.tokens_"+status, map[string]any{
		"actual_tokens": actual, "charged_tokens": charged,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}
