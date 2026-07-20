package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/z-chenhao/eri/internal/feedback"
)

// CaptureFeedback resolves the referenced delivery inside the feedback task's
// conversation, then records one immutable causal link. A correction or
// rejection invalidates derived data from the old answer; a later episode for
// the feedback task becomes the replacement dataset candidate.
func (s *Store) CaptureFeedback(ctx context.Context, request feedback.CaptureRequest) (feedback.Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return feedback.Record{}, err
	}
	defer tx.Rollback()

	var conversationID string
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM tasks WHERE id = ?`, request.FeedbackTaskID).Scan(&conversationID); err != nil {
		return feedback.Record{}, fmt.Errorf("resolve feedback task: %w", err)
	}
	query := `
		SELECT d.id, d.task_id, d.artifact_id
		FROM deliveries d JOIN tasks t ON t.id = d.task_id
		WHERE t.conversation_id = ? AND d.task_id <> ? AND d.status IN ('sent', 'acknowledged')`
	args := []any{conversationID, request.FeedbackTaskID}
	if request.RequestedDeliveryID != "" {
		query += ` AND d.id = ?`
		args = append(args, request.RequestedDeliveryID)
	} else {
		query += ` ORDER BY d.created_at DESC, d.id DESC LIMIT 1`
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&request.DeliveryID, &request.SourceTaskID, &request.ArtifactID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return feedback.Record{}, fmt.Errorf("no prior delivered answer is available for feedback")
		}
		return feedback.Record{}, err
	}
	encodedRef, err := json.Marshal(request.StatementRef)
	if err != nil {
		return feedback.Record{}, err
	}
	if err := insertContentRef(ctx, tx, request.StatementRef, request.CreatedAt); err != nil {
		return feedback.Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO feedback_records(id, feedback_task_id, source_task_id, artifact_id, delivery_id, kind, outcome, statement_ref_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, request.ID, request.FeedbackTaskID, request.SourceTaskID,
		request.ArtifactID, request.DeliveryID, request.Kind, request.Outcome, string(encodedRef), formatTime(request.CreatedAt)); err != nil {
		return feedback.Record{}, err
	}
	if request.Kind == feedback.Correction || request.Kind == feedback.Rejected {
		if _, err := tx.ExecContext(ctx, `
			UPDATE dataset_snapshots SET status = 'invalidated'
			WHERE id IN (SELECT snapshot_id FROM dataset_snapshot_items WHERE task_id = ?)`, request.SourceTaskID); err != nil {
			return feedback.Record{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE dataset_candidates SET status = 'invalidated'
			WHERE episode_id IN (SELECT id FROM episodes WHERE task_id = ?)`, request.SourceTaskID); err != nil {
			return feedback.Record{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE episodes SET status = 'invalidated' WHERE task_id = ?`, request.SourceTaskID); err != nil {
			return feedback.Record{}, err
		}
	}
	if err := appendEvent(ctx, tx, "feedback", request.ID, "feedback.recorded", map[string]any{
		"feedback_task_id": request.FeedbackTaskID, "source_task_id": request.SourceTaskID,
		"artifact_id": request.ArtifactID, "delivery_id": request.DeliveryID,
		"kind": request.Kind, "outcome": request.Outcome,
	}, request.CreatedAt); err != nil {
		return feedback.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return feedback.Record{}, err
	}
	return request.Record, nil
}
