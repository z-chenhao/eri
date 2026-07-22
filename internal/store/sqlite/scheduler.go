package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/scheduler"
)

type storedCommitmentSchedule struct {
	scheduler.Schedule
	DeliveryTarget *storedCommitmentDeliveryTarget `json:"delivery_target,omitempty"`
}

type storedCommitmentDeliveryTarget struct {
	Channel          string `json:"channel"`
	ConversationID   string `json:"conversation_id,omitempty"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	RoutingMode      string `json:"routing_mode"`
}

func encodeCommitmentSchedule(commitment scheduler.Commitment) ([]byte, error) {
	stored := storedCommitmentSchedule{Schedule: commitment.Schedule}
	if commitment.Target.Channel != "" {
		stored.DeliveryTarget = &storedCommitmentDeliveryTarget{
			Channel: commitment.Target.Channel, ConversationID: commitment.Target.ConversationID,
			ReplyToMessageID: commitment.Target.ReplyToMessageID, RoutingMode: commitment.Target.RoutingMode,
		}
	}
	return json.Marshal(stored)
}

func decodeCommitmentSchedule(body string) (scheduler.Schedule, scheduler.DeliveryTarget, error) {
	var stored storedCommitmentSchedule
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		return scheduler.Schedule{}, scheduler.DeliveryTarget{}, err
	}
	target := scheduler.DeliveryTarget{}
	if stored.DeliveryTarget != nil {
		target = scheduler.DeliveryTarget{
			Channel: stored.DeliveryTarget.Channel, ConversationID: stored.DeliveryTarget.ConversationID,
			ReplyToMessageID: stored.DeliveryTarget.ReplyToMessageID, RoutingMode: stored.DeliveryTarget.RoutingMode,
		}
	}
	if target.RoutingMode == "" {
		target.RoutingMode = scheduler.DeliveryRouteOrigin
	}
	return stored.Schedule, target, nil
}

func (s *Store) CommitmentDeliveryTarget(ctx context.Context, taskID string) (scheduler.DeliveryTarget, error) {
	var target scheduler.DeliveryTarget
	if err := s.db.QueryRowContext(ctx, `SELECT source_channel FROM tasks WHERE id = ?`, taskID).Scan(&target.Channel); err != nil {
		return scheduler.DeliveryTarget{}, err
	}
	if target.Channel != "lark" {
		return target, nil
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT cm.external_conversation_id, cm.external_message_id
		FROM channel_messages cm JOIN interactions i ON i.id = cm.interaction_id
		WHERE cm.channel = 'lark' AND cm.direction = 'inbound' AND i.task_id = ?
		ORDER BY i.sequence DESC LIMIT 1`, taskID).
		Scan(&target.ConversationID, &target.ReplyToMessageID)
	if err != nil {
		return scheduler.DeliveryTarget{}, err
	}
	return target, nil
}

func (s *Store) latestUserDeliveryTarget(ctx context.Context, tx *sql.Tx) (scheduler.DeliveryTarget, error) {
	var target scheduler.DeliveryTarget
	err := tx.QueryRowContext(ctx, `
		SELECT i.channel, COALESCE(cm.external_conversation_id, '')
		FROM interactions i
		LEFT JOIN channel_messages cm ON cm.interaction_id = i.id AND cm.direction = 'inbound'
		WHERE i.conversation_id = ? AND i.direction = 'inbound' AND i.role = 'user'
		ORDER BY i.sequence DESC LIMIT 1`, channel.ConversationID).
		Scan(&target.Channel, &target.ConversationID)
	if err != nil {
		return scheduler.DeliveryTarget{}, err
	}
	if target.Channel == "lark" && target.ConversationID == "" {
		return scheduler.DeliveryTarget{}, fmt.Errorf("latest Lark interaction has no durable conversation target")
	}
	target.RoutingMode = scheduler.DeliveryRouteRecent
	return target, nil
}

func (s *Store) CreateCommitment(ctx context.Context, commitment scheduler.Commitment) error {
	taskRef, err := json.Marshal(commitment.TaskRef)
	if err != nil {
		return err
	}
	scheduleJSON, err := encodeCommitmentSchedule(commitment)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, commitment.TaskRef, commitment.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO commitments(id, source_task_id, task_ref_json, schedule_json, importance, status, next_run_at,
			version, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)`, commitment.ID, commitment.SourceTaskID, string(taskRef), string(scheduleJSON),
		commitment.Importance, formatTime(commitment.NextRunAt), commitment.Version,
		formatTime(commitment.CreatedAt), formatTime(commitment.UpdatedAt)); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "commitment", commitment.ID, "commitment.created", map[string]any{
		"schedule_type": commitment.Schedule.Type, "next_run_at": formatTime(commitment.NextRunAt),
		"importance": commitment.Importance, "routing_mode": commitment.Target.RoutingMode,
		"target_channel": commitment.Target.Channel, "source_task_id": commitment.SourceTaskID,
	}, commitment.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateCommitment(ctx context.Context, replacement scheduler.Commitment) (scheduler.Commitment, error) {
	taskRef, err := json.Marshal(replacement.TaskRef)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	defer tx.Rollback()
	var current scheduler.Commitment
	var currentTaskRef, currentSchedule, lastRun, createdAt string
	err = tx.QueryRowContext(ctx, `
		SELECT source_task_id, task_ref_json, schedule_json, importance, status, COALESCE(last_run_at, ''),
			version, created_at
		FROM commitments WHERE id = ?`, replacement.ID).
		Scan(&current.SourceTaskID, &currentTaskRef, &currentSchedule, &current.Importance, &current.Status, &lastRun, &current.Version, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return scheduler.Commitment{}, fmt.Errorf("commitment not found")
	}
	if err != nil {
		return scheduler.Commitment{}, err
	}
	if current.Status == "completed" || current.Status == "canceled" {
		return scheduler.Commitment{}, fmt.Errorf("terminal commitment cannot be updated")
	}
	if err := json.Unmarshal([]byte(currentTaskRef), &current.TaskRef); err != nil {
		return scheduler.Commitment{}, err
	}
	current.Schedule, current.Target, err = decodeCommitmentSchedule(currentSchedule)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	// Updating the objective or cadence must not silently move an
	// origin_channel commitment to whichever channel supplied the clarification.
	// Preserve the trusted external target and change only the model-selectable
	// routing intent.
	replacement.Target.Channel = current.Target.Channel
	replacement.Target.ConversationID = current.Target.ConversationID
	replacement.Target.ReplyToMessageID = current.Target.ReplyToMessageID
	scheduleJSON, err := encodeCommitmentSchedule(replacement)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	current.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	if lastRun != "" {
		current.LastRunAt, err = parseTime(lastRun)
		if err != nil {
			return scheduler.Commitment{}, err
		}
	}
	if err := insertContentRef(ctx, tx, replacement.TaskRef, replacement.UpdatedAt); err != nil {
		return scheduler.Commitment{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE commitments SET source_task_id = ?, task_ref_json = ?, schedule_json = ?, importance = ?, next_run_at = ?,
			version = version + 1, updated_at = ?
		WHERE id = ? AND version = ?`, replacement.SourceTaskID, string(taskRef), string(scheduleJSON), replacement.Importance,
		formatTime(replacement.NextRunAt), formatTime(replacement.UpdatedAt), replacement.ID, current.Version)
	if err != nil {
		return scheduler.Commitment{}, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return scheduler.Commitment{}, fmt.Errorf("commitment changed concurrently")
	}
	if err := appendEvent(ctx, tx, "commitment", replacement.ID, "commitment.updated", map[string]any{
		"previous_version": current.Version, "schedule_type": replacement.Schedule.Type,
		"next_run_at": formatTime(replacement.NextRunAt), "importance": replacement.Importance,
		"routing_mode": replacement.Target.RoutingMode, "target_channel": replacement.Target.Channel,
		"source_task_id": replacement.SourceTaskID,
	}, replacement.UpdatedAt); err != nil {
		return scheduler.Commitment{}, err
	}
	if err := tx.Commit(); err != nil {
		return scheduler.Commitment{}, err
	}
	replacement.Status = current.Status
	replacement.Version = current.Version + 1
	replacement.CreatedAt = current.CreatedAt
	replacement.LastRunAt = current.LastRunAt
	return replacement, nil
}

func (s *Store) ListCommitments(ctx context.Context, limit int) ([]scheduler.Commitment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_task_id, task_ref_json, schedule_json, importance, status, COALESCE(next_run_at, ''),
			COALESCE(last_run_at, ''), version, created_at, updated_at
		FROM commitments ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commitments := make([]scheduler.Commitment, 0)
	for rows.Next() {
		var commitment scheduler.Commitment
		var taskRef, scheduleJSON, next, last, created, updated string
		if err := rows.Scan(&commitment.ID, &commitment.SourceTaskID, &taskRef, &scheduleJSON, &commitment.Importance, &commitment.Status,
			&next, &last, &commitment.Version, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(taskRef), &commitment.TaskRef); err != nil {
			return nil, err
		}
		commitment.Schedule, commitment.Target, err = decodeCommitmentSchedule(scheduleJSON)
		if err != nil {
			return nil, err
		}
		if next != "" {
			commitment.NextRunAt, err = parseTime(next)
			if err != nil {
				return nil, err
			}
		}
		if last != "" {
			commitment.LastRunAt, err = parseTime(last)
			if err != nil {
				return nil, err
			}
		}
		commitment.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		commitment.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		commitments = append(commitments, commitment)
	}
	return commitments, rows.Err()
}

func (s *Store) SetCommitmentStatus(ctx context.Context, id, status string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, scheduleJSON, next string
	err = tx.QueryRowContext(ctx, `SELECT status, schedule_json, COALESCE(next_run_at, '') FROM commitments WHERE id = ?`, id).
		Scan(&current, &scheduleJSON, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("commitment not found")
	}
	if err != nil {
		return err
	}
	if current == "completed" || current == "canceled" {
		return fmt.Errorf("terminal commitment cannot change status")
	}
	if status == "active" {
		schedule, _, err := decodeCommitmentSchedule(scheduleJSON)
		if err != nil {
			return err
		}
		nextAt, err := scheduler.FirstRun(schedule, now)
		if err != nil {
			return err
		}
		next = formatTime(nextAt)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE commitments SET status = ?, next_run_at = NULLIF(?, ''), version = version + 1, updated_at = ?
		WHERE id = ? AND status = ?`, status, next, formatTime(now), id, current)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("commitment changed concurrently")
	}
	if err := appendEvent(ctx, tx, "commitment", id, "commitment."+status, map[string]any{"previous_status": current}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TriggerDueCommitments(ctx context.Context, now time.Time, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id FROM commitments c
		JOIN tasks source ON source.id = c.source_task_id
		WHERE c.status = 'active' AND c.next_run_at <= ?
			AND source.status IN ('completed', 'failed')
		ORDER BY c.next_run_at LIMIT ?`, formatTime(now), limit)
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
	triggered := 0
	for _, id := range ids {
		created, err := s.triggerCommitment(ctx, id, now)
		if err != nil {
			return triggered, err
		}
		if created {
			triggered++
		}
	}
	return triggered, nil
}

func (s *Store) triggerCommitment(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var taskRefJSON, scheduleJSON, scheduledFor string
	err = tx.QueryRowContext(ctx, `
		SELECT c.task_ref_json, c.schedule_json, c.next_run_at
		FROM commitments c JOIN tasks source ON source.id = c.source_task_id
		WHERE c.id = ? AND c.status = 'active' AND c.next_run_at <= ?
			AND source.status IN ('completed', 'failed')`, id, formatTime(now)).
		Scan(&taskRefJSON, &scheduleJSON, &scheduledFor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	schedule, target, err := decodeCommitmentSchedule(scheduleJSON)
	if err != nil {
		return false, err
	}
	if target.RoutingMode == scheduler.DeliveryRouteRecent {
		target, err = s.latestUserDeliveryTarget(ctx, tx)
		if err != nil {
			return false, fmt.Errorf("resolve latest commitment delivery target: %w", err)
		}
	}
	if target.Channel == "" {
		target.Channel = "conversation_web"
	}
	interactionID, err := identifier.New()
	if err != nil {
		return false, err
	}
	taskID, err := identifier.New()
	if err != nil {
		return false, err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, channel.ConversationID, formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interactions(id, conversation_id, task_id, direction, role, kind, channel, content_ref_json, created_at)
		VALUES(?, ?, ?, 'inbound', 'user', 'internal_trigger', 'scheduler', ?, ?)`,
		interactionID, channel.ConversationID, taskID, taskRefJSON, formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks(id, conversation_id, source_interaction_id, source_channel, status, terminal_status,
			version, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', 1, ?, ?)`,
		taskID, channel.ConversationID, interactionID, target.Channel, formatTime(now), formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO commitment_fires(
			commitment_id, scheduled_for, task_id, target_channel, target_conversation_id,
			reply_to_message_id, routing_mode, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		id, scheduledFor, taskID, target.Channel, target.ConversationID,
		target.ReplyToMessageID, target.RoutingMode, formatTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'task.wake', ?, 'pending', 0, ?, ?, ?)`,
		outboxID, taskID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return false, err
	}
	next, recurring, err := scheduler.NextRun(schedule, now)
	if err != nil {
		return false, err
	}
	if recurring {
		if _, err := tx.ExecContext(ctx, `
			UPDATE commitments SET next_run_at = ?, last_run_at = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND next_run_at = ?`, formatTime(next), formatTime(now), formatTime(now), id, scheduledFor); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE commitments SET status = 'completed', next_run_at = NULL, last_run_at = ?,
				version = version + 1, updated_at = ? WHERE id = ? AND next_run_at = ?`,
			formatTime(now), formatTime(now), id, scheduledFor); err != nil {
			return false, err
		}
	}
	if err := appendEvent(ctx, tx, "commitment", id, "commitment.triggered", map[string]any{
		"task_id": taskID, "scheduled_for": scheduledFor, "routing_mode": target.RoutingMode,
		"target_channel": target.Channel,
	}, now); err != nil {
		return false, err
	}
	if err := appendEvent(ctx, tx, "task", taskID, "task.created", map[string]any{
		"status": "queued", "commitment_id": id, "source_channel": target.Channel,
	}, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
