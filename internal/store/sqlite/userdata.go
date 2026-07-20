package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/userdata"
)

// exportTables is deliberately explicit. Runtime leases and outbox mechanics
// are not user data and are excluded from the portable package.
var exportTables = []string{
	"conversations", "interactions", "channel_messages", "attachments", "tasks", "runs", "steps", "invocations", "context_checkpoints", "agent_checkpoints",
	"artifacts", "eval_records", "artifact_attachments", "deliveries", "effect_intents", "subagent_runs",
	"memory_claims", "memory_evidence", "memory_beliefs", "memory_items", "memory_terms", "memory_associations", "memory_retrievals", "memory_retrieval_items", "memory_delete_jobs",
	"commitments", "commitment_fires", "model_budget_usage", "episodes", "dataset_candidates",
	"dataset_snapshots", "dataset_snapshot_items",
	"feedback_records",
	"evolution_releases", "evolution_signals",
	"approvals", "capability_grants", "events", "content_objects", "data_erasure_jobs",
}

func (s *Store) BuildUserDataSnapshot(ctx context.Context) (userdata.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return userdata.Snapshot{}, err
	}
	defer tx.Rollback()
	snapshot := userdata.Snapshot{Tables: make(map[string][]map[string]any, len(exportTables))}
	for _, table := range exportTables {
		rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table)
		if err != nil {
			return userdata.Snapshot{}, fmt.Errorf("export table %s: %w", table, err)
		}
		values, err := scanRows(rows)
		if err != nil {
			return userdata.Snapshot{}, fmt.Errorf("export table %s: %w", table, err)
		}
		snapshot.Tables[table] = values
	}
	rows, err := tx.QueryContext(ctx, `SELECT ref_json FROM content_objects WHERE deleted_at IS NULL ORDER BY object_id, version`)
	if err != nil {
		return userdata.Snapshot{}, err
	}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			rows.Close()
			return userdata.Snapshot{}, err
		}
		var ref content.Ref
		if err := json.Unmarshal([]byte(encoded), &ref); err != nil {
			rows.Close()
			return userdata.Snapshot{}, fmt.Errorf("decode exported content ref: %w", err)
		}
		snapshot.Contents = append(snapshot.Contents, ref)
	}
	if err := rows.Close(); err != nil {
		return userdata.Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return userdata.Snapshot{}, err
	}
	return snapshot, nil
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for index, column := range columns {
			if bytes, ok := values[index].([]byte); ok {
				row[column] = string(bytes)
			} else {
				row[column] = values[index]
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) ScheduleUserDataErasure(ctx context.Context, job userdata.ErasureJob) (userdata.ErasureJob, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO data_erasure_jobs(id, task_id, status, created_at, updated_at)
		VALUES(?, ?, 'awaiting_delivery', ?, ?)
		ON CONFLICT(task_id) DO NOTHING`, job.ID, job.TaskID, formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
	if err != nil {
		return userdata.ErasureJob{}, err
	}
	var created, updated, completed sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT id, task_id, status, content_objects, created_at, updated_at, completed_at
		FROM data_erasure_jobs WHERE task_id = ?`, job.TaskID).
		Scan(&job.ID, &job.TaskID, &job.Status, &job.ContentObjects, &created, &updated, &completed)
	if err != nil {
		return userdata.ErasureJob{}, err
	}
	job.CreatedAt, _ = parseOptionalTime(created)
	job.UpdatedAt, _ = parseOptionalTime(updated)
	job.CompletedAt, _ = parseOptionalTime(completed)
	return job, nil
}

func (s *Store) PrepareUserDataErasure(ctx context.Context, id string) ([]content.Ref, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM data_erasure_jobs WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || status == "completed" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if status != "ready" {
		return nil, true, fmt.Errorf("data erasure job %s is %s", id, status)
	}
	if err := cancelTasksForErasure(ctx, tx); err != nil {
		return nil, true, err
	}
	var activeTasks, otherHandlers int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status IN ('queued', 'running', 'waiting', 'paused')`).Scan(&activeTasks); err != nil {
		return nil, true, err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM internal_outbox
		WHERE status = 'processing' AND NOT (kind = 'data.erase' AND aggregate_id = ?)`, id).Scan(&otherHandlers); err != nil {
		return nil, true, err
	}
	if activeTasks > 0 || otherHandlers > 0 {
		if err := tx.Commit(); err != nil {
			return nil, true, err
		}
		return nil, true, fmt.Errorf("waiting for %d active tasks and %d runtime handlers to stop", activeTasks, otherHandlers)
	}
	rows, err := tx.QueryContext(ctx, `SELECT ref_json FROM content_objects WHERE deleted_at IS NULL ORDER BY object_id, version`)
	if err != nil {
		return nil, true, err
	}
	refs := make([]content.Ref, 0)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			rows.Close()
			return nil, true, err
		}
		var ref content.Ref
		if err := json.Unmarshal([]byte(encoded), &ref); err != nil {
			rows.Close()
			return nil, true, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Close(); err != nil {
		return nil, true, err
	}
	if err := tx.Commit(); err != nil {
		return nil, true, err
	}
	return refs, true, nil
}

func (s *Store) CommitUserDataErasure(ctx context.Context, jobID string, objectCount int, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Dependents precede parents so foreign-key enforcement remains enabled.
	for _, table := range []string{
		"evolution_signals", "evolution_releases", "dataset_snapshot_items", "dataset_snapshots", "dataset_candidates", "episodes", "feedback_records", "artifact_attachments", "deliveries", "eval_records", "artifacts",
		"attachments", "channel_messages", "capability_grants", "approvals", "subagent_runs", "effect_intents", "agent_checkpoints", "context_checkpoints", "memory_retrieval_items", "memory_retrievals", "invocations", "steps", "runs",
		"commitment_fires", "commitments", "memory_delete_jobs", "memory_associations", "memory_semantic_index", "memory_terms", "memory_items",
		"memory_beliefs", "memory_evidence", "memory_claims", "model_budget_usage", "events",
		"conversation_introductions", "interactions", "tasks", "conversations", "content_objects",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("erase %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM internal_outbox WHERE NOT (kind = 'data.erase' AND aggregate_id = ?)`, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM data_erasure_jobs WHERE id <> ?`, jobID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE data_erasure_jobs SET status = 'completed', content_objects = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'ready'`, objectCount, formatTime(now), formatTime(now), jobID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("data erasure job %s was not ready", jobID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Remove deleted payloads from free pages and truncate historical WAL data.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint erased data: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum erased data: %w", err)
	}
	return nil
}

func parseOptionalTime(value sql.NullString) (time.Time, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, nil
	}
	return parseTime(value.String)
}

func queueDataErasureAfterDelivery(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) error {
	var jobID string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM data_erasure_jobs WHERE task_id = ? AND status = 'awaiting_delivery'`, taskID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE data_erasure_jobs SET status = 'ready', updated_at = ?
		WHERE id = ? AND status = 'awaiting_delivery'`, formatTime(now), jobID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("data erasure job %s was not awaiting delivery", jobID)
	}
	if err := cancelTasksForErasure(ctx, tx); err != nil {
		return err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	availableAt := now.Add(2 * time.Second)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'data.erase', ?, 'pending', 0, ?, ?, ?)`, outboxID, jobID,
		formatTime(availableAt), formatTime(now), formatTime(now))
	return err
}

func cancelTasksForErasure(ctx context.Context, tx *sql.Tx) error {
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'canceled', terminal_status = 'canceled', cancel_requested = 1,
			wait_reason = NULL, lease_owner = NULL, lease_until = NULL, version = version + 1, updated_at = ?
		WHERE status IN ('queued', 'waiting', 'paused')`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET cancel_requested = 1, version = version + 1, updated_at = ?
		WHERE status = 'running'`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE internal_outbox SET status = 'done', lease_owner = NULL, lease_until = NULL, updated_at = ?
		WHERE status = 'pending' AND kind = 'task.wake'
		  AND aggregate_id IN (SELECT id FROM tasks WHERE status = 'canceled')`, now); err != nil {
		return err
	}
	return nil
}
