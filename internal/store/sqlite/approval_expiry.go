package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/identifier"
)

func (s *Store) ExpireApprovals(ctx context.Context, now time.Time, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM approvals WHERE status = 'pending' AND expires_at <= ? ORDER BY expires_at LIMIT ?`, formatTime(now), limit)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	expired := 0
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return expired, err
		}
		var taskID, intentID string
		err = tx.QueryRowContext(ctx, `SELECT task_id, effect_intent_id FROM approvals WHERE id = ? AND status = 'pending' AND expires_at <= ?`, id, formatTime(now)).Scan(&taskID, &intentID)
		if err == sql.ErrNoRows {
			tx.Rollback()
			continue
		}
		if err != nil {
			tx.Rollback()
			return expired, err
		}
		if err := expireApprovalTx(ctx, tx, id, taskID, intentID, now); err != nil {
			tx.Rollback()
			return expired, err
		}
		if err := tx.Commit(); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

func expireApprovalTx(ctx context.Context, tx *sql.Tx, approvalID, taskID, intentID string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'expired', decided_at = ? WHERE id = ? AND status = 'pending'`, formatTime(now), approvalID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("approval is no longer pending")
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'approval.resume', ?, 'pending', 0, ?, ?, ?)`, outboxID, approvalID,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	return appendEvent(ctx, tx, "approval", approvalID, "approval.expired", map[string]any{
		"task_id": taskID, "effect_intent_id": intentID,
	}, now)
}
