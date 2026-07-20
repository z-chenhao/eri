package sqlite

import (
	"context"
	"time"

	"github.com/z-chenhao/eri/internal/identifier"
)

type RecoveryStats struct {
	RunningTasks     int64
	OutboxItems      int64
	AmbiguousEffects int
}

// RecoverRuntime is called only after the daemon has acquired all of its
// single-process listeners. It makes leases owned by the previous process
// immediately reclaimable without opening a new Run.
func (s *Store) RecoverRuntime(ctx context.Context, now time.Time) (RecoveryStats, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryStats{}, err
	}
	defer tx.Rollback()
	tasks, err := tx.ExecContext(ctx, `UPDATE tasks SET lease_until = ?, updated_at = ? WHERE status = 'running'`, formatTime(now.Add(-time.Second)), formatTime(now))
	if err != nil {
		return RecoveryStats{}, err
	}
	outbox, err := tx.ExecContext(ctx, `
		UPDATE internal_outbox SET status = 'pending', lease_owner = NULL, lease_until = NULL, available_at = ?, updated_at = ?
		WHERE status = 'processing'`, formatTime(now), formatTime(now))
	if err != nil {
		return RecoveryStats{}, err
	}
	taskCount, _ := tasks.RowsAffected()
	outboxCount, _ := outbox.RowsAffected()
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM effect_intents e
		WHERE e.status IN ('dispatched', 'unknown')
		  AND NOT EXISTS (
			SELECT 1 FROM internal_outbox o
			WHERE o.kind = 'effect.reconcile' AND o.aggregate_id = e.id AND o.status IN ('pending', 'processing')
		  )`)
	if err != nil {
		return RecoveryStats{}, err
	}
	var ambiguousEffects []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return RecoveryStats{}, err
		}
		ambiguousEffects = append(ambiguousEffects, id)
	}
	if err := rows.Close(); err != nil {
		return RecoveryStats{}, err
	}
	for _, effectID := range ambiguousEffects {
		outboxID, err := identifier.New()
		if err != nil {
			return RecoveryStats{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
			VALUES(?, 'effect.reconcile', ?, 'pending', 0, ?, ?, ?)`, outboxID, effectID,
			formatTime(now), formatTime(now), formatTime(now)); err != nil {
			return RecoveryStats{}, err
		}
	}
	if taskCount > 0 || outboxCount > 0 || len(ambiguousEffects) > 0 {
		if err := appendEvent(ctx, tx, "runtime", "daemon", "runtime.recovered", map[string]any{
			"running_tasks": taskCount, "outbox_items": outboxCount, "ambiguous_effects": len(ambiguousEffects),
		}, now); err != nil {
			return RecoveryStats{}, err
		}
	}
	stats := RecoveryStats{RunningTasks: taskCount, OutboxItems: outboxCount, AmbiguousEffects: len(ambiguousEffects)}
	if err := tx.Commit(); err != nil {
		return RecoveryStats{}, err
	}
	return stats, nil
}
