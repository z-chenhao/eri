package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/observability"
)

func (s *Store) ListRuns(ctx context.Context, limit int) ([]observability.RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.task_id, r.status, r.soul_version, r.started_at, COALESCE(r.ended_at, ''),
			COALESCE(SUM(CAST(json_extract(i.usage_json, '$.model_calls') AS INTEGER)), 0),
			(SELECT COUNT(*) FROM effect_intents e WHERE e.run_id = r.id),
			COALESCE(SUM(CAST(json_extract(i.usage_json, '$.input_tokens') AS INTEGER)), 0),
			COALESCE(SUM(CAST(json_extract(i.usage_json, '$.output_tokens') AS INTEGER)), 0),
			SUM(CASE WHEN i.status IN ('failed', 'unknown') THEN 1 ELSE 0 END)
		FROM runs r LEFT JOIN invocations i ON i.run_id = r.id
		GROUP BY r.id ORDER BY r.started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]observability.RunSummary, 0)
	for rows.Next() {
		var run observability.RunSummary
		var started, ended string
		if err := rows.Scan(&run.ID, &run.TaskID, &run.Status, &run.SoulVersion, &started, &ended,
			&run.ModelCalls, &run.ToolCalls, &run.InputTokens, &run.OutputTokens, &run.Errors); err != nil {
			return nil, err
		}
		var err error
		run.StartedAt, err = parseTime(started)
		if err != nil {
			return nil, err
		}
		if ended != "" {
			run.EndedAt, err = parseTime(ended)
			if err != nil {
				return nil, err
			}
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) LoadRun(ctx context.Context, id string) (observability.RunDetail, bool, error) {
	var detail observability.RunDetail
	var started, ended string
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, status, soul_version, started_at, COALESCE(ended_at, '') FROM runs WHERE id = ?`, id).
		Scan(&detail.Run.ID, &detail.Run.TaskID, &detail.Run.Status, &detail.Run.SoulVersion, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return observability.RunDetail{}, false, nil
	}
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	detail.Run.StartedAt, err = parseTime(started)
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	if ended != "" {
		detail.Run.EndedAt, err = parseTime(ended)
		if err != nil {
			return observability.RunDetail{}, false, err
		}
	}
	ids := []string{id, detail.Run.TaskID}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, status, target, context_manifest_json, COALESCE(usage_json, '{}'), COALESCE(error_code, ''), created_at, updated_at FROM invocations WHERE run_id = ? ORDER BY created_at`, id)
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	for rows.Next() {
		var invocation observability.Invocation
		var contextJSON, usageJSON, created, updated string
		if err := rows.Scan(&invocation.ID, &invocation.Kind, &invocation.Status, &invocation.Target, &contextJSON, &usageJSON, &invocation.ErrorCode, &created, &updated); err != nil {
			rows.Close()
			return observability.RunDetail{}, false, err
		}
		if err := json.Unmarshal([]byte(contextJSON), &invocation.ContextManifest); err != nil {
			rows.Close()
			return observability.RunDetail{}, false, fmt.Errorf("decode invocation %s context manifest: %w", invocation.ID, err)
		}
		if invocation.ContextManifest.MemoryRetrievalID != "" {
			usedRows, err := s.db.QueryContext(ctx, `
				SELECT memory_id FROM memory_retrieval_items
				WHERE retrieval_id = ? AND used = 1 ORDER BY rank`, invocation.ContextManifest.MemoryRetrievalID)
			if err != nil {
				rows.Close()
				return observability.RunDetail{}, false, err
			}
			for usedRows.Next() {
				var memoryID string
				if err := usedRows.Scan(&memoryID); err != nil {
					usedRows.Close()
					rows.Close()
					return observability.RunDetail{}, false, err
				}
				invocation.ContextManifest.AppliedMemoryIDs = append(invocation.ContextManifest.AppliedMemoryIDs, memoryID)
			}
			if err := usedRows.Close(); err != nil {
				rows.Close()
				return observability.RunDetail{}, false, err
			}
		}
		invocation.Usage = decodeObject(usageJSON)
		invocation.CreatedAt, _ = parseTime(created)
		invocation.UpdatedAt, _ = parseTime(updated)
		detail.Invocations = append(detail.Invocations, invocation)
		detail.Run.ModelCalls += intFromMap(invocation.Usage, "model_calls")
		detail.Run.InputTokens += intFromMap(invocation.Usage, "input_tokens")
		detail.Run.OutputTokens += intFromMap(invocation.Usage, "output_tokens")
		if invocation.Status == "failed" || invocation.Status == "unknown" {
			detail.Run.Errors++
		}
		ids = append(ids, invocation.ID)
	}
	if err := rows.Close(); err != nil {
		return observability.RunDetail{}, false, err
	}
	effectRows, err := s.db.QueryContext(ctx, `SELECT id, invocation_id, tool_call_id, COALESCE(parent_intent_id, ''), tool_id, effect_class, target, control_level, COALESCE(approval_id, ''), status, COALESCE(error_code, ''), payload_ref_json, COALESCE(result_ref_json, '{}'), created_at, updated_at FROM effect_intents WHERE run_id = ? ORDER BY created_at`, id)
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	for effectRows.Next() {
		var effect observability.Effect
		var payloadRef, resultRef, created, updated string
		if err := effectRows.Scan(&effect.ID, &effect.InvocationID, &effect.ToolCallID, &effect.ParentIntentID, &effect.ToolID, &effect.Effect, &effect.Target, &effect.Control, &effect.ApprovalID, &effect.Status, &effect.ErrorCode, &payloadRef, &resultRef, &created, &updated); err != nil {
			effectRows.Close()
			return observability.RunDetail{}, false, err
		}
		effect.CreatedAt, _ = parseTime(created)
		effect.UpdatedAt, _ = parseTime(updated)
		_ = json.Unmarshal([]byte(payloadRef), &effect.PayloadRef)
		_ = json.Unmarshal([]byte(resultRef), &effect.ResultRef)
		detail.Effects = append(detail.Effects, effect)
		detail.Run.ToolCalls++
		if effect.Status == "failed" || effect.Status == "unknown" {
			detail.Run.Errors++
		}
		ids = append(ids, effect.ID)
	}
	if err := effectRows.Close(); err != nil {
		return observability.RunDetail{}, false, err
	}
	artifactRows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.version, a.kind, a.status, a.trace_ref_json, COALESCE(e.id, ''), COALESCE(e.result, ''), COALESCE(e.tier, ''),
			COALESCE(e.evaluator, ''), COALESCE(e.findings_ref_json, '{}'),
			COALESCE(e.finding_count, 0), COALESCE(d.id, ''), COALESCE(d.status, ''), COALESCE(d.receipt, '')
		FROM artifacts a LEFT JOIN eval_records e ON e.artifact_id = a.id LEFT JOIN deliveries d ON d.artifact_id = a.id
		WHERE a.run_id = ? ORDER BY a.version`, id)
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	for artifactRows.Next() {
		var artifact observability.Artifact
		var traceRef, findingsRef string
		if err := artifactRows.Scan(&artifact.ID, &artifact.Version, &artifact.Kind, &artifact.Status, &traceRef, &artifact.EvalID, &artifact.Eval,
			&artifact.EvalTier, &artifact.EvalEvaluator, &findingsRef, &artifact.EvalFindingCount,
			&artifact.DeliveryID, &artifact.Delivery, &artifact.Receipt); err != nil {
			artifactRows.Close()
			return observability.RunDetail{}, false, err
		}
		_ = json.Unmarshal([]byte(traceRef), &artifact.TraceRef)
		_ = json.Unmarshal([]byte(findingsRef), &artifact.EvalFindingsRef)
		detail.Artifacts = append(detail.Artifacts, artifact)
		ids = append(ids, artifact.ID, artifact.EvalID, artifact.DeliveryID)
	}
	if err := artifactRows.Close(); err != nil {
		return observability.RunDetail{}, false, err
	}
	ids = compactIDs(ids)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, len(ids))
	for index := range ids {
		arguments[index] = ids[index]
	}
	eventRows, err := s.db.QueryContext(ctx, `SELECT sequence, id, aggregate_type, aggregate_id, type, payload_json, created_at FROM events WHERE aggregate_id IN (`+placeholders+`) ORDER BY sequence`, arguments...)
	if err != nil {
		return observability.RunDetail{}, false, err
	}
	for eventRows.Next() {
		var event eventlog.Event
		var payload, created string
		if err := eventRows.Scan(&event.Sequence, &event.ID, &event.AggregateType, &event.AggregateID, &event.Type, &payload, &created); err != nil {
			eventRows.Close()
			return observability.RunDetail{}, false, err
		}
		_ = json.Unmarshal([]byte(payload), &event.Data)
		event.Time, _ = parseTime(created)
		eventlog.Normalize(&event)
		detail.Events = append(detail.Events, event)
	}
	if err := eventRows.Close(); err != nil {
		return observability.RunDetail{}, false, err
	}
	return detail, true, nil
}

func (s *Store) LoadActiveRunTrace(ctx context.Context, runID string) (content.Ref, bool, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `
		SELECT state_ref_json FROM agent_checkpoints
		WHERE run_id = ? AND status = 'active'
		ORDER BY updated_at DESC LIMIT 1`, runID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return content.Ref{}, false, nil
	}
	if err != nil {
		return content.Ref{}, false, err
	}
	var ref content.Ref
	if err := json.Unmarshal([]byte(encoded), &ref); err != nil {
		return content.Ref{}, false, err
	}
	return ref, ref.ObjectID != "", nil
}

func intFromMap(value map[string]any, key string) int {
	number, _ := value[key].(float64)
	return int(number)
}
