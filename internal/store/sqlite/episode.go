package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/identifier"
)

func (s *Store) BuildEpisodeManifest(ctx context.Context, taskID string) (episode.Manifest, error) {
	manifest := episode.Manifest{TaskID: taskID}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&manifest.Status); err != nil {
		return episode.Manifest{}, err
	}
	if manifest.Status != "completed" && manifest.Status != "failed" && manifest.Status != "canceled" {
		return episode.Manifest{}, fmt.Errorf("task is not terminal")
	}
	ids := []string{taskID}
	runRows, err := s.db.QueryContext(ctx, `
		SELECT id, status, model_status, soul_version, target, context_manifest_json, COALESCE(usage_json, '{}'),
			COALESCE(error_code, ''), started_at, COALESCE(NULLIF(updated_at, ''), started_at), COALESCE(ended_at, '')
		FROM runs WHERE task_id = ? ORDER BY started_at`, taskID)
	if err != nil {
		return episode.Manifest{}, err
	}
	for runRows.Next() {
		var id, status, modelStatus, soul, target, contextJSON, usageJSON, errorCode, started, updated, ended string
		if err := runRows.Scan(&id, &status, &modelStatus, &soul, &target, &contextJSON, &usageJSON, &errorCode, &started, &updated, &ended); err != nil {
			runRows.Close()
			return episode.Manifest{}, err
		}
		manifest.Runs = append(manifest.Runs, map[string]any{
			"id": id, "status": status, "model_status": modelStatus, "soul_version": soul, "target": target,
			"context_manifest": decodeObject(contextJSON), "usage": decodeObject(usageJSON), "error_code": errorCode,
			"started_at": started, "updated_at": updated, "ended_at": ended, "replay": "simulate",
		})
		ids = append(ids, id)
	}
	if err := runRows.Close(); err != nil {
		return episode.Manifest{}, err
	}
	artifactRows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.version, a.kind, a.status, a.created_at,
			COALESCE(e.id, ''), COALESCE(e.tier, ''), COALESCE(e.evaluator, ''), COALESCE(e.result, ''), COALESCE(e.finding_count, 0),
			COALESCE(d.id, ''), COALESCE(d.status, ''), COALESCE(d.receipt, '')
		FROM artifacts a LEFT JOIN eval_records e ON e.artifact_id = a.id LEFT JOIN deliveries d ON d.artifact_id = a.id
		WHERE a.task_id = ? ORDER BY a.version`, taskID)
	if err != nil {
		return episode.Manifest{}, err
	}
	for artifactRows.Next() {
		var id, runID, kind, status, created, evalID, tier, evaluator, result, deliveryID, deliveryStatus, receipt string
		var version, findingCount int
		if err := artifactRows.Scan(&id, &runID, &version, &kind, &status, &created, &evalID, &tier, &evaluator, &result, &findingCount, &deliveryID, &deliveryStatus, &receipt); err != nil {
			artifactRows.Close()
			return episode.Manifest{}, err
		}
		manifest.Artifacts = append(manifest.Artifacts, map[string]any{
			"id": id, "run_id": runID, "version": version, "kind": kind, "status": status, "created_at": created,
			"eval":     map[string]any{"id": evalID, "tier": tier, "evaluator": evaluator, "result": result, "finding_count": findingCount},
			"delivery": map[string]any{"id": deliveryID, "status": deliveryStatus, "receipt": receipt}, "replay": "recorded_only",
		})
		ids = append(ids, id, evalID, deliveryID)
	}
	if err := artifactRows.Close(); err != nil {
		return episode.Manifest{}, err
	}
	effectRows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, invocation_id, tool_call_id, tool_id, tool_version, effect_class, target, parameters_hash, control_level, status, reconciliation_strategy, created_at, updated_at
		FROM effect_intents WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return episode.Manifest{}, err
	}
	for effectRows.Next() {
		var id, runID, invocationID, toolCallID, toolID, version, effect, target, hash, control, status, reconciliation, created, updated string
		if err := effectRows.Scan(&id, &runID, &invocationID, &toolCallID, &toolID, &version, &effect, &target, &hash, &control, &status, &reconciliation, &created, &updated); err != nil {
			effectRows.Close()
			return episode.Manifest{}, err
		}
		replay := "forbidden"
		if effect == "read_only" {
			replay = "safe_to_reinvoke"
		}
		manifest.Effects = append(manifest.Effects, map[string]any{
			"id": id, "run_id": runID, "invocation_id": invocationID, "tool_call_id": toolCallID,
			"tool_id": toolID, "tool_version": version, "effect": effect,
			"target": target, "parameters_hash": hash, "control": control, "status": status,
			"reconciliation": reconciliation, "created_at": created, "updated_at": updated, "replay": replay,
		})
		ids = append(ids, id)
	}
	if err := effectRows.Close(); err != nil {
		return episode.Manifest{}, err
	}
	ids = compactIDs(ids)
	if len(ids) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
		arguments := make([]any, len(ids))
		for index := range ids {
			arguments[index] = ids[index]
		}
		rows, err := s.db.QueryContext(ctx, `SELECT sequence, aggregate_type, aggregate_id, type, payload_json, created_at FROM events WHERE aggregate_id IN (`+placeholders+`) ORDER BY sequence`, arguments...)
		if err != nil {
			return episode.Manifest{}, err
		}
		for rows.Next() {
			var sequence int64
			var aggregateType, aggregateID, eventType, payload, created string
			if err := rows.Scan(&sequence, &aggregateType, &aggregateID, &eventType, &payload, &created); err != nil {
				rows.Close()
				return episode.Manifest{}, err
			}
			manifest.Events = append(manifest.Events, map[string]any{"sequence": sequence, "aggregate_type": aggregateType, "aggregate_id": aggregateID, "type": eventType, "payload": decodeObject(payload), "created_at": created})
		}
		if err := rows.Close(); err != nil {
			return episode.Manifest{}, err
		}
	}
	manifest.Privacy = map[string]any{"contains_message_bodies": false, "contains_credentials": false, "authorization_required_for_dataset": true}
	manifest.ReplayPolicy = map[string]any{"model": "simulate", "read_only_tools": "safe_to_reinvoke", "side_effects": "forbidden", "delivery": "forbidden"}
	return manifest, nil
}

func (s *Store) SaveEpisode(ctx context.Context, record episode.Record) (episode.Record, error) {
	encodedRef, err := json.Marshal(record.ManifestRef)
	if err != nil {
		return episode.Record{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return episode.Record{}, err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, record.ManifestRef, record.CreatedAt); err != nil {
		return episode.Record{}, err
	}
	var sourceCorrectiveFeedback int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM feedback_records
		WHERE source_task_id = ? AND kind IN ('correction', 'rejected')`, record.TaskID).Scan(&sourceCorrectiveFeedback); err != nil {
		return episode.Record{}, err
	}
	record.Status = "ready"
	if sourceCorrectiveFeedback > 0 {
		record.Status = "invalidated"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO episodes(id, task_id, manifest_ref_json, status, created_at) VALUES(?, ?, ?, ?, ?)`, record.ID, record.TaskID, string(encodedRef), record.Status, formatTime(record.CreatedAt)); err != nil {
		return episode.Record{}, err
	}
	if err := appendEvent(ctx, tx, "episode", record.ID, "episode.built", map[string]any{"task_id": record.TaskID, "status": record.Status}, record.CreatedAt); err != nil {
		return episode.Record{}, err
	}
	var correctiveFeedback int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM feedback_records
		WHERE feedback_task_id = ? AND kind IN ('correction', 'rejected')`, record.TaskID).Scan(&correctiveFeedback); err != nil {
		return episode.Record{}, err
	}
	if record.Status == "ready" && correctiveFeedback > 0 {
		candidateID, err := identifier.New()
		if err != nil {
			return episode.Record{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dataset_candidates(id, episode_id, status, created_at)
			VALUES(?, ?, 'candidate', ?)`, candidateID, record.ID, formatTime(record.CreatedAt)); err != nil {
			return episode.Record{}, err
		}
		if err := appendEvent(ctx, tx, "dataset_candidate", candidateID, "dataset_candidate.created", map[string]any{
			"episode_id": record.ID, "reason": "explicit_user_feedback",
			"requires_authorization": true, "formal_eval_set": false,
		}, record.CreatedAt); err != nil {
			return episode.Record{}, err
		}
	}
	return record, tx.Commit()
}

func (s *Store) LoadEpisodeForTask(ctx context.Context, taskID string) (episode.Record, bool, error) {
	return s.loadEpisode(ctx, `SELECT id, task_id, manifest_ref_json, status, created_at FROM episodes WHERE task_id = ?`, taskID)
}

func (s *Store) LoadEpisode(ctx context.Context, id string) (episode.Record, bool, error) {
	return s.loadEpisode(ctx, `SELECT id, task_id, manifest_ref_json, status, created_at FROM episodes WHERE id = ?`, id)
}

func (s *Store) RecordEpisodeExport(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM episodes WHERE id = ?`, id).Scan(&status); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, "episode", id, "episode.exported", map[string]any{
		"status": status, "contains_message_bodies": false, "format": "application/json",
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadEpisode(ctx context.Context, query, value string) (episode.Record, bool, error) {
	var record episode.Record
	var encodedRef, created string
	err := s.db.QueryRowContext(ctx, query, value).Scan(&record.ID, &record.TaskID, &encodedRef, &record.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return episode.Record{}, false, nil
	}
	if err != nil {
		return episode.Record{}, false, err
	}
	if err := json.Unmarshal([]byte(encodedRef), &record.ManifestRef); err != nil {
		return episode.Record{}, false, err
	}
	record.CreatedAt, err = parseTime(created)
	return record, true, err
}

func (s *Store) ListEpisodes(ctx context.Context, limit int) ([]episode.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, manifest_ref_json, status, created_at FROM episodes ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]episode.Record, 0)
	for rows.Next() {
		var record episode.Record
		var encodedRef, created string
		if err := rows.Scan(&record.ID, &record.TaskID, &encodedRef, &record.Status, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &record.ManifestRef); err != nil {
			return nil, err
		}
		record.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) PromoteEpisodeCandidate(ctx context.Context, episodeID string) (episode.DatasetCandidate, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return episode.DatasetCandidate{}, err
	}
	defer tx.Rollback()
	var episodeStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM episodes WHERE id = ?`, episodeID).Scan(&episodeStatus); err != nil {
		return episode.DatasetCandidate{}, err
	}
	if episodeStatus != "ready" {
		return episode.DatasetCandidate{}, fmt.Errorf("only ready episodes can become candidates")
	}
	var existing episode.DatasetCandidate
	var created string
	err = tx.QueryRowContext(ctx, `SELECT id, episode_id, status, created_at FROM dataset_candidates WHERE episode_id = ?`, episodeID).
		Scan(&existing.ID, &existing.EpisodeID, &existing.Status, &created)
	if err == nil {
		existing.CreatedAt, _ = parseTime(created)
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return episode.DatasetCandidate{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return episode.DatasetCandidate{}, err
	}
	candidate := episode.DatasetCandidate{ID: id, EpisodeID: episodeID, Status: "candidate", CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dataset_candidates(id, episode_id, status, created_at) VALUES(?, ?, 'candidate', ?)`, id, episodeID, formatTime(now)); err != nil {
		return episode.DatasetCandidate{}, err
	}
	if err := appendEvent(ctx, tx, "dataset_candidate", id, "dataset_candidate.created", map[string]any{
		"episode_id": episodeID, "requires_authorization": true, "formal_eval_set": false,
	}, now); err != nil {
		return episode.DatasetCandidate{}, err
	}
	return candidate, tx.Commit()
}

func (s *Store) ResolveDatasetCandidates(ctx context.Context, ids []string) ([]episode.DatasetSource, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, len(ids))
	for index := range ids {
		arguments[index] = ids[index]
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, e.id, e.task_id, e.manifest_ref_json
		FROM dataset_candidates c JOIN episodes e ON e.id = c.episode_id
		WHERE c.status = 'candidate' AND e.status = 'ready' AND c.id IN (`+placeholders+`)
		ORDER BY c.id`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]episode.DatasetSource, 0, len(ids))
	for rows.Next() {
		var source episode.DatasetSource
		var encodedRef string
		if err := rows.Scan(&source.CandidateID, &source.EpisodeID, &source.TaskID, &encodedRef); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &source.ManifestRef); err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

func (s *Store) SaveDatasetSnapshot(ctx context.Context, snapshot episode.DatasetSnapshot, items []episode.DatasetItem) (episode.DatasetSnapshot, error) {
	encodedRef, err := json.Marshal(snapshot.ManifestRef)
	if err != nil {
		return episode.DatasetSnapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return episode.DatasetSnapshot{}, err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, snapshot.ManifestRef, snapshot.CreatedAt); err != nil {
		return episode.DatasetSnapshot{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM dataset_snapshots`).Scan(&snapshot.Version); err != nil {
		return episode.DatasetSnapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dataset_snapshots(id, version, purpose, manifest_ref_json, status, item_count, created_at)
		VALUES(?, ?, ?, ?, 'frozen', ?, ?)`, snapshot.ID, snapshot.Version, snapshot.Purpose, string(encodedRef), len(items), formatTime(snapshot.CreatedAt)); err != nil {
		return episode.DatasetSnapshot{}, err
	}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dataset_snapshot_items(snapshot_id, candidate_id, episode_id, task_id, split)
			VALUES(?, ?, ?, ?, ?)`, snapshot.ID, item.CandidateID, item.EpisodeID, item.TaskID, item.Split); err != nil {
			return episode.DatasetSnapshot{}, err
		}
	}
	if err := appendEvent(ctx, tx, "dataset_snapshot", snapshot.ID, "dataset_snapshot.frozen", map[string]any{
		"version": snapshot.Version, "purpose": snapshot.Purpose, "item_count": len(items),
	}, snapshot.CreatedAt); err != nil {
		return episode.DatasetSnapshot{}, err
	}
	snapshot.ItemCount = len(items)
	snapshot.Status = "frozen"
	return snapshot, tx.Commit()
}

func (s *Store) ListDatasetSnapshots(ctx context.Context, limit int) ([]episode.DatasetSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, version, purpose, manifest_ref_json, status, item_count, created_at
		FROM dataset_snapshots ORDER BY version DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]episode.DatasetSnapshot, 0)
	for rows.Next() {
		var snapshot episode.DatasetSnapshot
		var encodedRef, created string
		if err := rows.Scan(&snapshot.ID, &snapshot.Version, &snapshot.Purpose, &encodedRef, &snapshot.Status, &snapshot.ItemCount, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encodedRef), &snapshot.ManifestRef); err != nil {
			return nil, err
		}
		snapshot.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	return result, rows.Err()
}

func (s *Store) LoadDatasetSnapshot(ctx context.Context, id string) (episode.DatasetSnapshot, bool, error) {
	var snapshot episode.DatasetSnapshot
	var encodedRef, created string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, version, purpose, manifest_ref_json, status, item_count, created_at
		FROM dataset_snapshots WHERE id = ?`, id).
		Scan(&snapshot.ID, &snapshot.Version, &snapshot.Purpose, &encodedRef, &snapshot.Status, &snapshot.ItemCount, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return episode.DatasetSnapshot{}, false, nil
	}
	if err != nil {
		return episode.DatasetSnapshot{}, false, err
	}
	if err := json.Unmarshal([]byte(encodedRef), &snapshot.ManifestRef); err != nil {
		return episode.DatasetSnapshot{}, false, err
	}
	snapshot.CreatedAt, err = parseTime(created)
	return snapshot, err == nil, err
}

func decodeObject(body string) map[string]any {
	value := map[string]any{}
	_ = json.Unmarshal([]byte(body), &value)
	return value
}

func compactIDs(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
