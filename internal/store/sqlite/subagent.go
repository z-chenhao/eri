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
	"github.com/z-chenhao/eri/internal/subagent"
)

func (s *Store) QueueSubagentRun(ctx context.Context, job subagent.Run) (subagent.Run, bool, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.Run{}, false, err
	}
	defer tx.Rollback()
	if existing, found, err := loadSubagentRun(ctx, tx, job.ID); err != nil {
		return subagent.Run{}, false, err
	} else if found {
		return existing, false, tx.Commit()
	}
	if job.RoleID == "" || job.ProviderID == "" {
		return subagent.Run{}, false, fmt.Errorf("subagent role and provider are required")
	}
	encodedRequest, err := json.Marshal(job.RequestRef)
	if err != nil {
		return subagent.Run{}, false, err
	}
	if err := insertContentRef(ctx, tx, job.RequestRef, now); err != nil {
		return subagent.Run{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subagent_runs(id, role_id, provider_id, parent_task_id, parent_run_id, access_mode, status, request_ref_json, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
		job.ID, job.RoleID, job.ProviderID, job.ParentTaskID, job.ParentRunID, string(job.Access), string(encodedRequest), formatTime(now), formatTime(now)); err != nil {
		return subagent.Run{}, false, err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return subagent.Run{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, ?, ?, 'pending', 0, ?, ?, ?)`, outboxID, "subagent."+job.ProviderID+".run", job.ID,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return subagent.Run{}, false, err
	}
	if err := appendEvent(ctx, tx, "subagent", job.ID, "subagent.queued", map[string]any{
		"role_id": job.RoleID, "provider_id": job.ProviderID, "parent_task_id": job.ParentTaskID, "parent_run_id": job.ParentRunID,
		"parent_intent_id": job.ID, "access": job.Access,
	}, now); err != nil {
		return subagent.Run{}, false, err
	}
	job.Status = "queued"
	job.CreatedAt = now
	job.UpdatedAt = now
	return job, true, tx.Commit()
}

func (s *Store) LoadSubagentRun(ctx context.Context, id string) (subagent.Run, bool, error) {
	return loadSubagentRun(ctx, s.db, id)
}

type subagentQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadSubagentRun(ctx context.Context, queryer subagentQueryer, id string) (subagent.Run, bool, error) {
	var job subagent.Run
	var access, requestJSON string
	var resultJSON, runtimeStateJSON, continuationJSON, errorCode, started, created, updated sql.NullString
	var runtimeID sql.NullString
	err := queryer.QueryRowContext(ctx, `
		SELECT id, role_id, provider_id, parent_task_id, parent_run_id, access_mode, status, request_ref_json,
			result_ref_json, runtime_state_ref_json, continuation_ref_json, runtime_id, error_code, started_at, created_at, updated_at
		FROM subagent_runs WHERE id = ?`, id).
		Scan(&job.ID, &job.RoleID, &job.ProviderID, &job.ParentTaskID, &job.ParentRunID, &access, &job.Status, &requestJSON,
			&resultJSON, &runtimeStateJSON, &continuationJSON, &runtimeID, &errorCode, &started, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return subagent.Run{}, false, nil
	}
	if err != nil {
		return subagent.Run{}, false, err
	}
	job.Access = subagent.AccessMode(access)
	job.RuntimeID = runtimeID.String
	job.ErrorCode = errorCode.String
	if err := json.Unmarshal([]byte(requestJSON), &job.RequestRef); err != nil {
		return subagent.Run{}, false, err
	}
	if resultJSON.Valid {
		if err := json.Unmarshal([]byte(resultJSON.String), &job.ResultRef); err != nil {
			return subagent.Run{}, false, err
		}
	}
	if runtimeStateJSON.Valid {
		if err := json.Unmarshal([]byte(runtimeStateJSON.String), &job.RuntimeStateRef); err != nil {
			return subagent.Run{}, false, err
		}
	}
	if continuationJSON.Valid {
		if err := json.Unmarshal([]byte(continuationJSON.String), &job.ContinuationRef); err != nil {
			return subagent.Run{}, false, err
		}
	}
	var parseErr error
	job.StartedAt, parseErr = parseOptionalTime(started)
	if parseErr != nil {
		return subagent.Run{}, false, parseErr
	}
	job.CreatedAt, parseErr = parseOptionalTime(created)
	if parseErr != nil {
		return subagent.Run{}, false, parseErr
	}
	job.UpdatedAt, parseErr = parseOptionalTime(updated)
	return job, parseErr == nil, parseErr
}

func (s *Store) MarkSubagentRunStarting(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE subagent_runs SET status = 'starting', updated_at = ? WHERE id = ? AND status = 'queued'`, formatTime(now), id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("subagent run %s is not queued", id)
	}
	return nil
}

func (s *Store) MarkSubagentRunRunning(ctx context.Context, id, runtimeID string) error {
	if runtimeID == "" {
		return fmt.Errorf("subagent runtime id is required")
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE subagent_runs SET status = 'running', runtime_id = ?, started_at = ?, updated_at = ?
		WHERE id = ? AND status = 'starting'`, runtimeID, formatTime(now), formatTime(now), id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("subagent run %s cannot start", id)
	}
	var roleID, providerID string
	if err := tx.QueryRowContext(ctx, `SELECT role_id, provider_id FROM subagent_runs WHERE id = ?`, id).Scan(&roleID, &providerID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "subagent", id, "subagent.started", map[string]any{"role_id": roleID, "provider_id": providerID}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SubagentRunCancellationRequested(ctx context.Context, id string) (bool, error) {
	var requested int
	err := s.db.QueryRowContext(ctx, `
		SELECT t.cancel_requested FROM subagent_runs d JOIN tasks t ON t.id = d.parent_task_id WHERE d.id = ?`, id).Scan(&requested)
	return requested == 1, err
}

// SaveSubagentRuntimeState persists the restricted Agent Loop continuation.
// It is intentionally separate from continuation_ref_json, which resumes the
// paused primary Eri only after the subagent reaches a terminal state.
func (s *Store) SaveSubagentRuntimeState(ctx context.Context, id string, stateRef content.Ref) error {
	encoded, err := json.Marshal(stateRef)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, stateRef, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE subagent_runs SET runtime_state_ref_json = ?, updated_at = ?
		WHERE id = ? AND status IN ('starting', 'running')`, string(encoded), formatTime(now), id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("subagent run %s cannot save runtime state", id)
	}
	var roleID, providerID string
	if err := tx.QueryRowContext(ctx, `SELECT role_id, provider_id FROM subagent_runs WHERE id = ?`, id).Scan(&roleID, &providerID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "subagent", id, "subagent.checkpoint.saved", map[string]any{"role_id": roleID, "provider_id": providerID}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteSubagentRun(ctx context.Context, id, status, errorCode string, resultRef content.Ref) (bool, error) {
	if status != "completed" && status != "failed" && status != "unknown" && status != "canceled" {
		return false, fmt.Errorf("invalid subagent terminal status %q", status)
	}
	now := time.Now().UTC()
	encodedResult, err := json.Marshal(resultRef)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var taskID, roleID, providerID, current string
	var continuationJSON sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id, role_id, provider_id, status, continuation_ref_json FROM subagent_runs WHERE id = ?`, id).Scan(&taskID, &roleID, &providerID, &current, &continuationJSON); err != nil {
		return false, err
	}
	if current == "completed" || current == "failed" || current == "unknown" || current == "canceled" {
		return false, tx.Commit()
	}
	if err := insertContentRef(ctx, tx, resultRef, now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subagent_runs SET status = ?, result_ref_json = ?, error_code = NULLIF(?, ''),
			completed_at = ?, updated_at = ? WHERE id = ?`,
		status, string(encodedResult), errorCode, formatTime(now), formatTime(now), id); err != nil {
		return false, err
	}
	// The subagent can finish before the parent commits its progress delivery and
	// continuation. Wake the parent only when both halves are durable; the
	// pause transaction performs the symmetric check for the opposite order.
	if continuationJSON.Valid {
		if err := ensureSubagentResumeOutbox(ctx, tx, id, now); err != nil {
			return false, err
		}
	}
	if err := appendEvent(ctx, tx, "subagent", id, "subagent."+status, map[string]any{
		"role_id": roleID, "provider_id": providerID, "parent_task_id": taskID, "parent_intent_id": id, "error_code": errorCode,
	}, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) PauseForSubagent(ctx context.Context, commit agent.SubagentWaitCommit) error {
	now := time.Now().UTC()
	artifactRef, err := json.Marshal(commit.ArtifactRef)
	if err != nil {
		return err
	}
	traceRef, err := json.Marshal(commit.TraceRef)
	if err != nil {
		return err
	}
	continuationRef, err := json.Marshal(commit.ContinuationRef)
	if err != nil {
		return err
	}
	findingsRef, err := json.Marshal(commit.EvalFindingsRef)
	if err != nil {
		return err
	}
	if commit.EvalTier == "" {
		commit.EvalTier = "routine"
	}
	if commit.EvalEvaluator == "" {
		commit.EvalEvaluator = "llm_judge"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var roleID, providerID, parentTaskID, parentRunID, delegationStatus string
	if err := tx.QueryRowContext(ctx, `SELECT role_id, provider_id, parent_task_id, parent_run_id, status FROM subagent_runs WHERE id = ?`, commit.DelegationID).Scan(&roleID, &providerID, &parentTaskID, &parentRunID, &delegationStatus); err != nil {
		return err
	}
	if roleID != commit.RoleID || providerID != commit.ProviderID || parentTaskID != commit.TaskID || parentRunID != commit.RunID {
		return fmt.Errorf("subagent delegation %s does not match the active task/run/binding", commit.DelegationID)
	}
	for _, ref := range []content.Ref{commit.ArtifactRef, commit.TraceRef, commit.ContinuationRef, commit.EvalFindingsRef} {
		if err := insertContentRef(ctx, tx, ref, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subagent_runs SET continuation_ref_json = ?, progress_delivery_id = ?, updated_at = ? WHERE id = ?`,
		string(continuationRef), commit.DeliveryID, formatTime(now), commit.DelegationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_checkpoints SET status = 'superseded', updated_at = ?
		WHERE task_id = ? AND status = 'active'`, formatTime(now), commit.TaskID); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts WHERE task_id = ?`, commit.TaskID).Scan(&version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts(id, task_id, run_id, version, kind, content_ref_json, status, trace_ref_json, created_at)
		VALUES(?, ?, ?, ?, 'subagent_progress', ?, 'approved', ?, ?)`,
		commit.ArtifactID, commit.TaskID, commit.RunID, version, string(artifactRef), string(traceRef), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO eval_records(id, artifact_id, tier, evaluator, result, findings_ref_json, finding_count, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, commit.EvalID, commit.ArtifactID, commit.EvalTier, commit.EvalEvaluator,
		string(commit.EvalResult), string(findingsRef), len(commit.EvalFindings), formatTime(now)); err != nil {
		return err
	}
	var sourceChannel string
	if err := tx.QueryRowContext(ctx, `SELECT source_channel FROM tasks WHERE id = ?`, commit.TaskID).Scan(&sourceChannel); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries(id, task_id, artifact_id, target_channel, status, receipt, idempotency_key,
			terminal_status, continue_task, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'queued', '', ?, 'completed', 1, ?, ?)`, commit.DeliveryID, commit.TaskID,
		commit.ArtifactID, sourceChannel, commit.ArtifactID+":"+sourceChannel, formatTime(now), formatTime(now)); err != nil {
		return err
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'delivery.send', ?, 'pending', 0, ?, ?, ?)`, outboxID, commit.DeliveryID,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'waiting', wait_reason = ?, lease_owner = NULL, lease_until = NULL,
			version = version + 1, updated_at = ? WHERE id = ? AND status = 'running'`,
		"subagent:"+commit.DelegationID, formatTime(now), commit.TaskID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("task %s cannot wait for subagent", commit.TaskID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'waiting', updated_at = ? WHERE run_id = ? AND status = 'running'`, formatTime(now), commit.RunID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "task", commit.TaskID, "task.waiting", map[string]any{
		"reason": "subagent", "delegation_id": commit.DelegationID, "role_id": roleID, "provider_id": providerID,
	}, now); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "delivery", commit.DeliveryID, "delivery.queued", map[string]any{
		"artifact_id": commit.ArtifactID, "channel": sourceChannel, "continue_task": true,
	}, now); err != nil {
		return err
	}
	if delegationStatus == "completed" || delegationStatus == "failed" || delegationStatus == "unknown" || delegationStatus == "canceled" {
		if err := ensureSubagentResumeOutbox(ctx, tx, commit.DelegationID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureSubagentResumeOutbox(ctx context.Context, tx *sql.Tx, delegationID string, now time.Time) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM internal_outbox
			WHERE kind = 'subagent.resume' AND aggregate_id = ? AND status IN ('pending', 'processing')
		)`, delegationID).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	outboxID, err := identifier.New()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
		VALUES(?, 'subagent.resume', ?, 'pending', 0, ?, ?, ?)`, outboxID, delegationID,
		formatTime(now), formatTime(now), formatTime(now))
	return err
}

func (s *Store) ClaimSubagentResume(ctx context.Context, delegationID, owner string, lease time.Duration) (agent.SubagentResume, bool, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.SubagentResume{}, false, err
	}
	defer tx.Rollback()
	var resume agent.SubagentResume
	var resultJSON, continuationJSON, progressDeliveryID string
	err = tx.QueryRowContext(ctx, `
		SELECT d.parent_task_id, d.parent_run_id, d.id, d.role_id, d.provider_id, d.status, d.result_ref_json, d.continuation_ref_json,
			d.progress_delivery_id, i.id
		FROM subagent_runs d
		JOIN invocations i ON i.run_id = d.parent_run_id AND i.kind = 'model'
		WHERE d.id = ? AND d.status IN ('completed', 'failed', 'unknown', 'canceled')
			AND d.result_ref_json IS NOT NULL AND d.continuation_ref_json IS NOT NULL`, delegationID).
		Scan(&resume.Task.TaskID, &resume.Task.RunID, &resume.DelegationID, &resume.RoleID, &resume.ProviderID, &resume.Status,
			&resultJSON, &continuationJSON, &progressDeliveryID, &resume.Task.InvocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.SubagentResume{}, false, nil
	}
	if err != nil {
		return agent.SubagentResume{}, false, err
	}
	if err := json.Unmarshal([]byte(resultJSON), &resume.ResultRef); err != nil {
		return agent.SubagentResume{}, false, err
	}
	if err := json.Unmarshal([]byte(continuationJSON), &resume.ContinuationRef); err != nil {
		return agent.SubagentResume{}, false, err
	}
	var progressSent int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM deliveries d
			JOIN artifacts a ON a.id = d.artifact_id
			WHERE d.id = ? AND d.task_id = ? AND a.kind = 'subagent_progress' AND d.status = 'sent'
		)`, progressDeliveryID, resume.Task.TaskID).Scan(&progressSent); err != nil {
		return agent.SubagentResume{}, false, err
	}
	if progressSent != 1 {
		return agent.SubagentResume{}, false, agent.ErrSubagentProgressPending
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'running', wait_reason = NULL, lease_owner = ?, lease_until = ?,
			version = version + 1, updated_at = ?
		WHERE id = ? AND ((status = 'waiting' AND wait_reason = ?) OR (status = 'running' AND lease_until < ?))`,
		owner, formatTime(now.Add(lease)), formatTime(now), resume.Task.TaskID,
		"subagent:"+delegationID, formatTime(now))
	if err != nil {
		return agent.SubagentResume{}, false, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return agent.SubagentResume{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = 'running', updated_at = ? WHERE run_id = ? AND status = 'waiting'`, formatTime(now), resume.Task.RunID); err != nil {
		return agent.SubagentResume{}, false, err
	}
	if err := appendEvent(ctx, tx, "task", resume.Task.TaskID, "task.resumed", map[string]any{
		"delegation_id": delegationID, "role_id": resume.RoleID, "provider_id": resume.ProviderID, "subagent_status": resume.Status,
	}, now); err != nil {
		return agent.SubagentResume{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return agent.SubagentResume{}, false, err
	}
	return resume, true, nil
}
