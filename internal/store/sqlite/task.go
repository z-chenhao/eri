package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	assistanttask "github.com/z-chenhao/eri/internal/task"
)

func (s *Store) ListTasks(ctx context.Context, limit int) ([]assistanttask.Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.status, COALESCE(t.wait_reason, ''), COALESCE(t.error_code, ''), t.cancel_requested,
			i.content_ref_json, t.created_at, t.updated_at
		FROM tasks t JOIN interactions i ON i.id = t.source_interaction_id
		ORDER BY t.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]assistanttask.Record, 0)
	for rows.Next() {
		record, err := scanTaskRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) LoadTask(ctx context.Context, id string) (assistanttask.Record, bool, error) {
	record, err := scanTaskRecord(s.db.QueryRowContext(ctx, `
		SELECT t.id, t.status, COALESCE(t.wait_reason, ''), COALESCE(t.error_code, ''), t.cancel_requested,
			i.content_ref_json, t.created_at, t.updated_at
		FROM tasks t JOIN interactions i ON i.id = t.source_interaction_id WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return assistanttask.Record{}, false, nil
	}
	return record, err == nil, err
}

type taskScanner interface{ Scan(...any) error }

func scanTaskRecord(scanner taskScanner) (assistanttask.Record, error) {
	var record assistanttask.Record
	var encodedRef, created, updated string
	var cancelRequested int
	if err := scanner.Scan(&record.ID, &record.Status, &record.WaitReason, &record.ErrorCode, &cancelRequested,
		&encodedRef, &created, &updated); err != nil {
		return assistanttask.Record{}, err
	}
	if err := json.Unmarshal([]byte(encodedRef), &record.ObjectiveRef); err != nil {
		return assistanttask.Record{}, err
	}
	record.CancelAsked = cancelRequested == 1
	var err error
	record.CreatedAt, err = parseTime(created)
	if err != nil {
		return assistanttask.Record{}, err
	}
	record.UpdatedAt, err = parseTime(updated)
	return record, err
}

func (s *Store) RequestTaskCancel(ctx context.Context, id string) (assistanttask.CancelResult, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assistanttask.CancelResult{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return assistanttask.CancelResult{}, fmt.Errorf("task not found")
	} else if err != nil {
		return assistanttask.CancelResult{}, err
	}
	result := assistanttask.CancelResult{TaskID: id, Status: status, Effect: "already_terminal"}
	switch status {
	case "completed", "failed", "canceled":
		return result, tx.Commit()
	case "queued", "waiting", "paused":
		result.Status = "canceled"
		result.Effect = "canceled_before_next_step"
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'canceled', terminal_status = 'canceled', cancel_requested = 1,
			wait_reason = NULL, lease_owner = NULL, lease_until = NULL, version = version + 1, updated_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
			return assistanttask.CancelResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'canceled', model_status = 'canceled', error_code = 'user_canceled', updated_at = ?, ended_at = ? WHERE task_id = ? AND status = 'active'`, formatTime(now), formatTime(now), id); err != nil {
			return assistanttask.CancelResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE internal_outbox SET status = 'done', updated_at = ? WHERE aggregate_id = ? AND kind IN ('task.wake', 'approval.resume') AND status = 'pending'`, formatTime(now), id); err != nil {
			return assistanttask.CancelResult{}, err
		}
	case "running":
		result.Effect = "cancel_requested"
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET cancel_requested = 1, version = version + 1, updated_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
			return assistanttask.CancelResult{}, err
		}
	default:
		return assistanttask.CancelResult{}, fmt.Errorf("unsupported task status %q", status)
	}
	if err := appendEvent(ctx, tx, "task", id, "task.cancel_requested", map[string]any{"previous_status": status, "effect": result.Effect}, now); err != nil {
		return assistanttask.CancelResult{}, err
	}
	return result, tx.Commit()
}

func (s *Store) RetryTask(ctx context.Context, sourceTaskID string) (assistanttask.RetryResult, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assistanttask.RetryResult{}, err
	}
	defer tx.Rollback()
	var status, conversationID, sourceInteractionID, sourceChannel string
	err = tx.QueryRowContext(ctx, `
		SELECT status, conversation_id, source_interaction_id, source_channel
		FROM tasks WHERE id = ?`, sourceTaskID).
		Scan(&status, &conversationID, &sourceInteractionID, &sourceChannel)
	if errors.Is(err, sql.ErrNoRows) {
		return assistanttask.RetryResult{}, fmt.Errorf("task not found")
	}
	if err != nil {
		return assistanttask.RetryResult{}, err
	}
	if status != "failed" && status != "canceled" {
		return assistanttask.RetryResult{}, fmt.Errorf("only failed or canceled tasks can be retried")
	}
	var unsafeEffects int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM effect_intents
		WHERE task_id = ? AND status IN ('dispatched', 'confirmed', 'unknown')`, sourceTaskID).Scan(&unsafeEffects); err != nil {
		return assistanttask.RetryResult{}, err
	}
	if unsafeEffects != 0 {
		return assistanttask.RetryResult{}, fmt.Errorf("task cannot be retried because %d external effects were dispatched, confirmed, or left unknown", unsafeEffects)
	}
	newTaskID, err := identifier.New()
	if err != nil {
		return assistanttask.RetryResult{}, err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return assistanttask.RetryResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status, version, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', 'completed', 1, ?, ?)`, newTaskID, conversationID, sourceInteractionID, sourceChannel, formatTime(now), formatTime(now)); err != nil {
		return assistanttask.RetryResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'task.wake', ?, 'pending', 0, ?, ?, ?)`, outboxID, newTaskID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return assistanttask.RetryResult{}, err
	}
	if err := appendEvent(ctx, tx, "task", sourceTaskID, "task.retry_requested", map[string]any{
		"retry_task_id": newTaskID, "checkpoint": "task_start", "confirmed_effects": 0,
	}, now); err != nil {
		return assistanttask.RetryResult{}, err
	}
	if err := appendEvent(ctx, tx, "task", newTaskID, "task.created", map[string]any{
		"source": "observatory_retry", "retry_of": sourceTaskID, "checkpoint": "task_start",
	}, now); err != nil {
		return assistanttask.RetryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return assistanttask.RetryResult{}, err
	}
	return assistanttask.RetryResult{SourceTaskID: sourceTaskID, TaskID: newTaskID, Status: "queued", Checkpoint: "task_start"}, nil
}

func (s *Store) TaskCancelRequested(ctx context.Context, id string) (bool, error) {
	var requested int
	err := s.db.QueryRowContext(ctx, `SELECT cancel_requested FROM tasks WHERE id = ?`, id).Scan(&requested)
	return requested == 1, err
}

func (s *Store) CommitTaskCancellation(ctx context.Context, taskID, runID string, traceRef content.Ref, usage agent.Usage) error {
	now := time.Now().UTC()
	encodedTrace, err := json.Marshal(traceRef)
	if err != nil {
		return err
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, traceRef, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'canceled', model_status = 'canceled', usage_json = ?, error_code = 'user_canceled', updated_at = ?, ended_at = ? WHERE id = ? AND status = 'active'`, string(usageJSON), formatTime(now), formatTime(now), runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'canceled', terminal_status = 'canceled', wait_reason = NULL,
		lease_owner = NULL, lease_until = NULL, version = version + 1, updated_at = ? WHERE id = ? AND cancel_requested = 1`, formatTime(now), taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_checkpoints SET status = 'completed', updated_at = ? WHERE task_id = ? AND status = 'active'`, formatTime(now), taskID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "run", runID, "run.canceled", map[string]any{"trace_ref": encodedTrace}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "task", taskID, "task.canceled", map[string]any{"run_id": runID}, now); err != nil {
		return err
	}
	return tx.Commit()
}
