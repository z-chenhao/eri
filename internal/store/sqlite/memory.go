package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/memory"
)

func (s *Store) CaptureEvidence(ctx context.Context, record memory.CaptureRecord) (memory.CaptureOutcome, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.CaptureOutcome{}, err
	}
	defer tx.Rollback()
	outcome := memory.CaptureOutcome{}
	claimID := record.ClaimID
	var statementRefJSON string
	err = tx.QueryRowContext(ctx, `SELECT id, statement_ref_json FROM memory_claims WHERE statement_key = ?`, record.ClaimKey).Scan(&claimID, &statementRefJSON)
	if errors.Is(err, sql.ErrNoRows) {
		if record.ClaimID != "" {
			var existingKey string
			err := tx.QueryRowContext(ctx, `SELECT statement_key FROM memory_claims WHERE id = ?`, record.ClaimID).Scan(&existingKey)
			if err == nil && existingKey != record.ClaimKey {
				return memory.CaptureOutcome{}, fmt.Errorf("claim id is bound to a different statement")
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return memory.CaptureOutcome{}, err
			}
		}
		statementRef, err := json.Marshal(record.StatementRef)
		if err != nil {
			return memory.CaptureOutcome{}, err
		}
		statementRefJSON = string(statementRef)
		if err := insertContentRef(ctx, tx, record.StatementRef, now); err != nil {
			return memory.CaptureOutcome{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_claims(id, statement_key, statement_ref_json, kind, scope, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`, claimID, record.ClaimKey, statementRefJSON,
			record.Kind, record.Scope, formatTime(now), formatTime(now)); err != nil {
			return memory.CaptureOutcome{}, err
		}
		outcome.StatementStored = true
	} else if err != nil {
		return memory.CaptureOutcome{}, err
	}
	memoryID := ""
	var itemStatus, usagePolicy, kind, scope, updated string
	var salience float64
	var pinned int
	err = tx.QueryRowContext(ctx, `
		SELECT id, kind, scope, salience, status, usage_policy, pinned, updated_at FROM memory_items WHERE claim_id = ?`, claimID).
		Scan(&memoryID, &kind, &scope, &salience, &itemStatus, &usagePolicy, &pinned, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		memoryID, err = identifier.New()
		if err != nil {
			return memory.CaptureOutcome{}, err
		}
		itemStatus = "candidate"
		if record.Activate {
			itemStatus = "active"
		}
		if record.Pin {
			pinned = 1
		}
		usagePolicy = "allow"
		kind, scope, salience, updated = record.Kind, record.Scope, 0.5, formatTime(now)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_items(id, claim_id, kind, scope, salience, status, usage_policy, pinned, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, 'allow', ?, ?, ?)`, memoryID, claimID, kind, scope, salience,
			itemStatus, pinned, formatTime(now), formatTime(now)); err != nil {
			return memory.CaptureOutcome{}, err
		}
	} else if err != nil {
		return memory.CaptureOutcome{}, err
	} else if record.Activate {
		itemStatus = "active"
		if record.Pin {
			pinned = 1
		}
		updated = formatTime(now)
		if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET status = 'active', pinned = ?, updated_at = ? WHERE id = ?`, pinned, updated, memoryID); err != nil {
			return memory.CaptureOutcome{}, err
		}
	}
	evidenceRef, err := json.Marshal(record.EvidenceRef)
	if err != nil {
		return memory.CaptureOutcome{}, err
	}
	validUntil := any(nil)
	if !record.ValidUntil.IsZero() {
		validUntil = formatTime(record.ValidUntil)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO memory_evidence(id, claim_id, evidence_key, evidence_ref_json, relation, source_type,
			source_ref, independence_group, reliability, directness, verifiability, observed_at,
			valid_until, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)
		ON CONFLICT(evidence_key) DO NOTHING`, record.EvidenceID, claimID, record.EvidenceKey,
		string(evidenceRef), string(record.Relation), record.SourceType, record.SourceRef,
		record.IndependenceGroup, record.Reliability, record.Directness, record.Verifiability,
		formatTime(record.ObservedAt), validUntil, formatTime(now))
	if err != nil {
		return memory.CaptureOutcome{}, err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return memory.CaptureOutcome{}, err
	}
	if created == 1 {
		if err := insertContentRef(ctx, tx, record.EvidenceRef, now); err != nil {
			return memory.CaptureOutcome{}, err
		}
		outcome.EvidenceStored = true
	}
	actualEvidenceID := record.EvidenceID
	if created == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM memory_evidence WHERE evidence_key = ?`, record.EvidenceKey).Scan(&actualEvidenceID); err != nil {
			return memory.CaptureOutcome{}, err
		}
	}
	updated = formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET updated_at = ?, salience = MIN(1.0, salience + 0.02) WHERE id = ?`, updated, memoryID); err != nil {
		return memory.CaptureOutcome{}, err
	}
	assessment, err := assessClaim(ctx, tx, claimID, now)
	if err != nil {
		return memory.CaptureOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_beliefs(claim_id, status, confidence, support_weight, contradict_weight,
			independent_groups, expired, version, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(claim_id) DO UPDATE SET status = excluded.status, confidence = excluded.confidence,
			support_weight = excluded.support_weight, contradict_weight = excluded.contradict_weight,
			independent_groups = excluded.independent_groups, expired = excluded.expired,
			version = memory_beliefs.version + 1,
			updated_at = excluded.updated_at`, claimID, string(assessment.Status), assessment.Confidence,
		assessment.SupportWeight, assessment.ContradictWeight, assessment.IndependentGroups, assessment.Expired, formatTime(now)); err != nil {
		return memory.CaptureOutcome{}, err
	}
	if itemStatus == "candidate" && assessment.Status == memory.Supported && assessment.IndependentGroups >= 2 && assessment.SupportWeight >= 1.2 {
		itemStatus = "active"
		if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET status = 'active', updated_at = ? WHERE id = ?`, formatTime(now), memoryID); err != nil {
			return memory.CaptureOutcome{}, err
		}
		if err := appendEvent(ctx, tx, "memory", memoryID, "memory.promoted", map[string]any{
			"reason": "independent_evidence_consolidation", "independent_groups": assessment.IndependentGroups,
		}, now); err != nil {
			return memory.CaptureOutcome{}, err
		}
	}
	for _, term := range record.TermKeys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_terms(memory_id, term_key) VALUES(?, ?) ON CONFLICT DO NOTHING`, memoryID, term); err != nil {
			return memory.CaptureOutcome{}, err
		}
	}
	if err := appendEvent(ctx, tx, "memory", memoryID, "memory.evidence_captured", map[string]any{
		"claim_id": claimID, "evidence_id": actualEvidenceID, "relation": record.Relation,
		"source_type": record.SourceType, "independence_group": record.IndependenceGroup,
		"belief_status": assessment.Status, "duplicate": created == 0,
	}, now); err != nil {
		return memory.CaptureOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return memory.CaptureOutcome{}, err
	}
	updatedAt, _ := parseTime(updated)
	outcome.Snapshot = memory.Snapshot{
		MemoryID: memoryID, ClaimID: claimID, Status: assessment.Status, Confidence: assessment.Confidence,
		SupportWeight: assessment.SupportWeight, ContradictWeight: assessment.ContradictWeight,
		IndependentGroups: assessment.IndependentGroups, Kind: kind, Scope: scope,
		UsagePolicy: usagePolicy, LifecycleStatus: itemStatus, Salience: salience, Pinned: pinned == 1,
		Expired: assessment.Expired, UpdatedAt: updatedAt,
	}
	if err := json.Unmarshal([]byte(statementRefJSON), &outcome.Snapshot.StatementRef); err != nil {
		return memory.CaptureOutcome{}, err
	}
	return outcome, nil
}

func assessClaim(ctx context.Context, tx *sql.Tx, claimID string, now time.Time) (memory.Assessment, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT relation, independence_group, reliability, directness, verifiability
		FROM memory_evidence WHERE claim_id = ? AND status = 'active'
			AND (valid_until IS NULL OR valid_until > ?)`, claimID, formatTime(now))
	if err != nil {
		return memory.Assessment{}, err
	}
	defer rows.Close()
	items := make([]memory.WeightedEvidence, 0)
	for rows.Next() {
		var item memory.WeightedEvidence
		if err := rows.Scan(&item.Relation, &item.IndependenceGroup, &item.Reliability, &item.Directness, &item.Verifiability); err != nil {
			return memory.Assessment{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return memory.Assessment{}, err
	}
	assessment := memory.Assess(items)
	if len(items) == 0 {
		var historical int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence WHERE claim_id = ? AND status = 'active'`, claimID).Scan(&historical); err != nil {
			return memory.Assessment{}, err
		}
		assessment.Expired = historical > 0
	}
	return assessment, nil
}

func (s *Store) RetrieveMemory(ctx context.Context, termKeys []string, limit int) ([]memory.Candidate, error) {
	if err := s.refreshExpiredBeliefs(ctx); err != nil {
		return nil, err
	}
	if len(termKeys) == 0 {
		return s.queryMemory(ctx, `
			SELECT m.id FROM memory_items m JOIN memory_beliefs b ON b.claim_id = m.claim_id
			WHERE m.status IN ('active', 'low_salience') AND m.usage_policy = 'allow' AND b.expired = 0
			ORDER BY CASE m.status WHEN 'active' THEN 0 ELSE 1 END, m.salience DESC, m.updated_at DESC LIMIT ?`, []any{limit})
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(termKeys)), ",")
	args := make([]any, 0, len(termKeys)+1)
	for _, key := range termKeys {
		args = append(args, key)
	}
	args = append(args, limit)
	return s.queryMemory(ctx, `
		WITH matched AS (
			SELECT m.id, COUNT(*) AS matches FROM memory_items m JOIN memory_terms t ON t.memory_id = m.id
			JOIN memory_beliefs b ON b.claim_id = m.claim_id
			WHERE m.status IN ('active', 'low_salience') AND m.usage_policy = 'allow' AND b.expired = 0
				AND t.term_key IN (`+placeholders+`)
			GROUP BY m.id
		), expanded AS (
			SELECT id, matches, 0 AS association_hop FROM matched
			UNION ALL
			SELECT CASE WHEN a.left_memory_id = matched.id THEN a.right_memory_id ELSE a.left_memory_id END,
				0, 1
			FROM matched JOIN memory_associations a
				ON a.left_memory_id = matched.id OR a.right_memory_id = matched.id
			WHERE a.weight >= 0.1
		)
		SELECT m.id FROM expanded x JOIN memory_items m ON m.id = x.id
		JOIN memory_beliefs b ON b.claim_id = m.claim_id
		WHERE m.status IN ('active', 'low_salience') AND m.usage_policy = 'allow' AND b.expired = 0
		GROUP BY m.id
		ORDER BY MIN(x.association_hop), MAX(x.matches) DESC,
			CASE m.status WHEN 'active' THEN 0 ELSE 1 END, m.salience DESC, m.updated_at DESC LIMIT ?`, args)
}

func (s *Store) MissingSemanticMemory(ctx context.Context, modelID string, limit int) ([]memory.Candidate, error) {
	if strings.TrimSpace(modelID) == "" || limit <= 0 {
		return nil, fmt.Errorf("semantic model id and positive limit are required")
	}
	if err := s.refreshExpiredBeliefs(ctx); err != nil {
		return nil, err
	}
	return s.queryMemory(ctx, `
		SELECT m.id FROM memory_items m
		JOIN memory_beliefs b ON b.claim_id = m.claim_id
		LEFT JOIN memory_semantic_index semantic ON semantic.memory_id = m.id
		WHERE m.status IN ('active', 'low_salience') AND m.usage_policy = 'allow' AND b.expired = 0
			AND (semantic.memory_id IS NULL OR semantic.model_id != ?)
		ORDER BY CASE m.status WHEN 'active' THEN 0 ELSE 1 END, m.salience DESC, m.updated_at DESC
		LIMIT ?`, []any{modelID, limit})
}

func (s *Store) ReplaceSemanticMemory(ctx context.Context, record memory.SemanticRecord) (content.Ref, bool, error) {
	if record.MemoryID == "" || record.ModelID == "" || record.Dimensions <= 0 || record.ContentHash == "" || record.VectorRef.ObjectID == "" {
		return content.Ref{}, false, fmt.Errorf("semantic memory record is incomplete")
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	encodedRef, err := json.Marshal(record.VectorRef)
	if err != nil {
		return content.Ref{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return content.Ref{}, false, err
	}
	defer tx.Rollback()
	var previousJSON string
	previousFound := true
	if err := tx.QueryRowContext(ctx, `SELECT vector_ref_json FROM memory_semantic_index WHERE memory_id = ?`, record.MemoryID).Scan(&previousJSON); errors.Is(err, sql.ErrNoRows) {
		previousFound = false
	} else if err != nil {
		return content.Ref{}, false, err
	}
	if err := insertContentRef(ctx, tx, record.VectorRef, record.UpdatedAt); err != nil {
		return content.Ref{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_semantic_index(memory_id, model_id, dimensions, content_hash, vector_ref_json, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET model_id = excluded.model_id, dimensions = excluded.dimensions,
			content_hash = excluded.content_hash, vector_ref_json = excluded.vector_ref_json, updated_at = excluded.updated_at`,
		record.MemoryID, record.ModelID, record.Dimensions, record.ContentHash, string(encodedRef), formatTime(record.UpdatedAt)); err != nil {
		return content.Ref{}, false, err
	}
	var previous content.Ref
	if previousFound {
		if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
			return content.Ref{}, false, err
		}
		if previous.ObjectID != record.VectorRef.ObjectID {
			if _, err := tx.ExecContext(ctx, `UPDATE content_objects SET deleted_at = ? WHERE object_id = ? AND version = ?`, formatTime(record.UpdatedAt), previous.ObjectID, previous.Version); err != nil {
				return content.Ref{}, false, err
			}
		}
	}
	if err := appendEvent(ctx, tx, "memory", record.MemoryID, "memory.semantic_indexed", map[string]any{
		"model_id": record.ModelID, "dimensions": record.Dimensions,
	}, record.UpdatedAt); err != nil {
		return content.Ref{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return content.Ref{}, false, err
	}
	return previous, previousFound, nil
}

func (s *Store) SemanticMemory(ctx context.Context, modelID string, limit int) ([]memory.SemanticCandidate, error) {
	if strings.TrimSpace(modelID) == "" || limit <= 0 {
		return nil, fmt.Errorf("semantic model id and positive limit are required")
	}
	if err := s.refreshExpiredBeliefs(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, semantic.dimensions, semantic.content_hash, semantic.vector_ref_json, semantic.updated_at
		FROM memory_semantic_index semantic
		JOIN memory_items m ON m.id = semantic.memory_id
		JOIN memory_beliefs b ON b.claim_id = m.claim_id
		WHERE semantic.model_id = ? AND m.status IN ('active', 'low_salience')
			AND m.usage_policy = 'allow' AND b.expired = 0
		ORDER BY CASE m.status WHEN 'active' THEN 0 ELSE 1 END, m.salience DESC, m.updated_at DESC
		LIMIT ?`, modelID, limit)
	if err != nil {
		return nil, err
	}
	records := make([]memory.SemanticRecord, 0)
	for rows.Next() {
		var memoryID, encodedRef, updated string
		var dimensions int
		var contentHash string
		if err := rows.Scan(&memoryID, &dimensions, &contentHash, &encodedRef, &updated); err != nil {
			return nil, err
		}
		record := memory.SemanticRecord{MemoryID: memoryID, ModelID: modelID, Dimensions: dimensions, ContentHash: contentHash}
		if err := json.Unmarshal([]byte(encodedRef), &record.VectorRef); err != nil {
			rows.Close()
			return nil, err
		}
		record.UpdatedAt, err = parseTime(updated)
		if err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]memory.SemanticCandidate, 0, len(records))
	for _, record := range records {
		candidate, err := s.loadMemoryCandidate(ctx, record.MemoryID)
		if err != nil {
			return nil, err
		}
		result = append(result, memory.SemanticCandidate{Memory: candidate, Index: record})
	}
	return result, nil
}

func (s *Store) InspectMemory(ctx context.Context, limit int) ([]memory.Candidate, error) {
	if err := s.refreshExpiredBeliefs(ctx); err != nil {
		return nil, err
	}
	return s.queryMemory(ctx, `
		SELECT id FROM memory_items WHERE status != 'deleted' ORDER BY updated_at DESC LIMIT ?`, []any{limit})
}

func (s *Store) InspectAllMemory(ctx context.Context) ([]memory.Candidate, error) {
	if err := s.refreshExpiredBeliefs(ctx); err != nil {
		return nil, err
	}
	return s.queryMemory(ctx, `
		SELECT id FROM memory_items WHERE status != 'deleted' ORDER BY updated_at DESC`, nil)
}

func (s *Store) refreshExpiredBeliefs(ctx context.Context) error {
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT claim_id FROM memory_evidence
		WHERE status = 'active' AND valid_until IS NOT NULL AND valid_until <= ?`, formatTime(now))
	if err != nil {
		return err
	}
	claimIDs := make([]string, 0)
	for rows.Next() {
		var claimID string
		if err := rows.Scan(&claimID); err != nil {
			rows.Close()
			return err
		}
		claimIDs = append(claimIDs, claimID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, claimID := range claimIDs {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		assessment, err := assessClaim(ctx, tx, claimID, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE memory_beliefs SET status = ?, confidence = ?, support_weight = ?, contradict_weight = ?,
				independent_groups = ?, expired = ?, version = version + 1, updated_at = ?
			WHERE claim_id = ? AND (status != ? OR ABS(confidence - ?) > 0.000001 OR
				ABS(support_weight - ?) > 0.000001 OR ABS(contradict_weight - ?) > 0.000001 OR
				independent_groups != ? OR expired != ?)`,
			string(assessment.Status), assessment.Confidence, assessment.SupportWeight, assessment.ContradictWeight,
			assessment.IndependentGroups, assessment.Expired, formatTime(now), claimID,
			string(assessment.Status), assessment.Confidence, assessment.SupportWeight, assessment.ContradictWeight,
			assessment.IndependentGroups, assessment.Expired); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) queryMemory(ctx context.Context, query string, args []any) ([]memory.Candidate, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]memory.Candidate, 0, len(ids))
	for _, id := range ids {
		candidate, err := s.loadMemoryCandidate(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, nil
}

func (s *Store) loadMemoryCandidate(ctx context.Context, memoryID string) (memory.Candidate, error) {
	var candidate memory.Candidate
	var statementRef, status, lifecycle, updated string
	var lastAccessed sql.NullString
	var pinned int
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.claim_id, c.statement_ref_json, b.status, b.confidence, b.support_weight,
			b.contradict_weight, b.independent_groups, b.expired, m.kind, m.scope, m.usage_policy,
			m.status, m.salience, m.access_count, m.last_accessed_at, m.pinned, m.updated_at
		FROM memory_items m JOIN memory_claims c ON c.id = m.claim_id
		JOIN memory_beliefs b ON b.claim_id = c.id WHERE m.id = ?`, memoryID).
		Scan(&candidate.MemoryID, &candidate.ClaimID, &statementRef, &status, &candidate.Confidence,
			&candidate.SupportWeight, &candidate.ContradictWeight, &candidate.IndependentGroups, &candidate.Expired,
			&candidate.Kind, &candidate.Scope, &candidate.UsagePolicy, &lifecycle, &candidate.Salience,
			&candidate.AccessCount, &lastAccessed, &pinned, &updated)
	if err != nil {
		return memory.Candidate{}, err
	}
	candidate.Status = memory.BeliefStatus(status)
	candidate.LifecycleStatus = lifecycle
	candidate.Pinned = pinned == 1
	if lastAccessed.Valid {
		candidate.LastAccessedAt, err = parseTime(lastAccessed.String)
		if err != nil {
			return memory.Candidate{}, err
		}
	}
	candidate.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return memory.Candidate{}, err
	}
	if err := json.Unmarshal([]byte(statementRef), &candidate.StatementRef); err != nil {
		return memory.Candidate{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, evidence_ref_json, relation, source_type, source_ref, independence_group, observed_at, reliability
		FROM memory_evidence WHERE claim_id = ? AND status = 'active' ORDER BY observed_at DESC`, candidate.ClaimID)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var source memory.SourceSummary
		var evidenceRef, observed string
		if err := rows.Scan(&source.EvidenceID, &evidenceRef, &source.Relation, &source.SourceType,
			&source.SourceRef, &source.IndependenceGroup, &observed, &source.Reliability); err != nil {
			return memory.Candidate{}, err
		}
		var ref content.Ref
		if err := json.Unmarshal([]byte(evidenceRef), &ref); err != nil {
			return memory.Candidate{}, err
		}
		candidate.EvidenceRefs = append(candidate.EvidenceRefs, ref)
		source.ObservedAt, err = parseTime(observed)
		if err != nil {
			return memory.Candidate{}, err
		}
		candidate.Sources = append(candidate.Sources, source)
	}
	return candidate, rows.Err()
}

func (s *Store) RecordMemoryRetrieval(ctx context.Context, record memory.RetrievalRecord) error {
	if record.ID == "" || record.TaskID == "" || record.InvocationID == "" {
		return fmt.Errorf("memory retrieval id, task id and invocation id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_retrievals(id, task_id, invocation_id, query_key, created_at)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		record.ID, record.TaskID, record.InvocationID, record.QueryKey, formatTime(record.CreatedAt)); err != nil {
		return err
	}
	for _, item := range record.Items {
		reasons, err := json.Marshal(item.Reasons)
		if err != nil {
			return err
		}
		injected := 0
		if item.Injected {
			injected = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_retrieval_items(retrieval_id, memory_id, rank, score, reasons_json, injected)
			VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(retrieval_id, memory_id) DO NOTHING`,
			record.ID, item.MemoryID, item.Rank, item.Score, string(reasons), injected); err != nil {
			return err
		}
	}
	if err := appendEvent(ctx, tx, "memory_retrieval", record.ID, "memory.recall.completed", map[string]any{
		"task_id": record.TaskID, "invocation_id": record.InvocationID, "candidate_count": len(record.Items),
	}, record.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordMemoryUse(ctx context.Context, retrievalID string, memoryIDs []string, now time.Time) error {
	ids := append([]string(nil), memoryIDs...)
	sort.Strings(ids)
	ids = compactStrings(ids)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	newlyUsed := make([]string, 0, len(ids))
	for _, id := range ids {
		var injected, used int
		if err := tx.QueryRowContext(ctx, `
			SELECT injected, used FROM memory_retrieval_items WHERE retrieval_id = ? AND memory_id = ?`, retrievalID, id).Scan(&injected, &used); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("memory %s was not part of retrieval %s", id, retrievalID)
			}
			return err
		}
		if injected != 1 {
			return fmt.Errorf("memory %s was retrieved but not injected", id)
		}
		if used == 1 {
			continue
		}
		var last sql.NullString
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT last_accessed_at, status FROM memory_items WHERE id = ?`, id).Scan(&last, &status); err != nil {
			return err
		}
		bonus := 0.03
		if !last.Valid {
			bonus = 0.10
		} else if previous, err := parseTime(last.String); err == nil {
			days := now.Sub(previous).Hours() / 24
			if days >= 30 {
				bonus = 0.12
			} else if days >= 7 {
				bonus = 0.08
			}
		}
		nextStatus := status
		if status == "low_salience" {
			nextStatus = "active"
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE memory_items SET access_count = access_count + 1,
				last_accessed_at = ?, salience = MIN(1.0, salience + ?), status = ?, updated_at = ?
			WHERE id = ?`, formatTime(now), bonus, nextStatus, formatTime(now), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE memory_retrieval_items SET used = 1, used_at = ?
			WHERE retrieval_id = ? AND memory_id = ? AND used = 0`, formatTime(now), retrievalID, id); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, "memory", id, "memory.used", map[string]any{
			"retrieval_id": retrievalID, "spaced_use_bonus": bonus, "reactivated": status == "low_salience",
		}, now); err != nil {
			return err
		}
		newlyUsed = append(newlyUsed, id)
	}
	allUsed := make([]string, 0)
	if len(newlyUsed) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT memory_id FROM memory_retrieval_items WHERE retrieval_id = ? AND used = 1 ORDER BY memory_id`, retrievalID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			allUsed = append(allUsed, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	newSet := make(map[string]struct{}, len(newlyUsed))
	for _, id := range newlyUsed {
		newSet[id] = struct{}{}
	}
	for left := 0; left < len(allUsed); left++ {
		for right := left + 1; right < len(allUsed); right++ {
			_, leftNew := newSet[allUsed[left]]
			_, rightNew := newSet[allUsed[right]]
			if !leftNew && !rightNew {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO memory_associations(left_memory_id, right_memory_id, weight, last_reinforced_at)
				VALUES(?, ?, 0.20, ?)
				ON CONFLICT(left_memory_id, right_memory_id) DO UPDATE SET
					weight = MIN(1.0, memory_associations.weight + 0.08),
					last_reinforced_at = excluded.last_reinforced_at`, allUsed[left], allUsed[right], formatTime(now)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) PromoteMemory(ctx context.Context, memoryID string, pin bool) error {
	now := time.Now().UTC()
	pinned := 0
	if pin {
		pinned = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE memory_items SET status = 'active', pinned = MAX(pinned, ?), salience = MAX(salience, 0.7), updated_at = ?
		WHERE id = ? AND status IN ('candidate', 'low_salience', 'archived')`, pinned, formatTime(now), memoryID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_items WHERE id = ? AND status = 'active'`, memoryID).Scan(&exists); err != nil || exists != 1 {
			return fmt.Errorf("memory not found")
		}
	}
	if err := appendEvent(ctx, tx, "memory", memoryID, "memory.promoted", map[string]any{"reason": "explicit", "pinned": pin}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConsolidateMemory(ctx context.Context, now time.Time, limit int) (memory.ConsolidationReport, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.status, m.salience, m.pinned, COALESCE(m.last_accessed_at, m.updated_at),
			COALESCE(m.last_consolidated_at, m.updated_at), b.status, b.support_weight, b.independent_groups
		FROM memory_items m JOIN memory_beliefs b ON b.claim_id = m.claim_id
		WHERE m.status IN ('candidate', 'active', 'low_salience')
		ORDER BY COALESCE(m.last_consolidated_at, m.updated_at) LIMIT ?`, limit)
	if err != nil {
		return memory.ConsolidationReport{}, err
	}
	type item struct {
		id, status, accessed, consolidated, belief string
		salience                                   float64
		pinned, groups                             int
		support                                    float64
	}
	items := make([]item, 0)
	for rows.Next() {
		var current item
		if err := rows.Scan(&current.id, &current.status, &current.salience, &current.pinned,
			&current.accessed, &current.consolidated, &current.belief, &current.support, &current.groups); err != nil {
			rows.Close()
			return memory.ConsolidationReport{}, err
		}
		items = append(items, current)
	}
	if err := rows.Close(); err != nil {
		return memory.ConsolidationReport{}, err
	}
	report := memory.ConsolidationReport{}
	for _, current := range items {
		lastConsolidated, err := parseTime(current.consolidated)
		if err != nil {
			return report, err
		}
		if now.Sub(lastConsolidated) < 24*time.Hour {
			continue
		}
		lastAccessed, err := parseTime(current.accessed)
		if err != nil {
			return report, err
		}
		daysSinceConsolidation := maxFloat(0, now.Sub(lastConsolidated).Hours()/24)
		daysSinceAccess := maxFloat(0, now.Sub(lastAccessed).Hours()/24)
		nextSalience := current.salience * math.Exp(-daysSinceConsolidation/180)
		nextStatus := current.status
		if current.pinned == 1 && nextSalience < 0.75 {
			nextSalience = 0.75
		}
		if current.status == "candidate" && current.belief == string(memory.Supported) && current.groups >= 2 && current.support >= 1.2 {
			nextStatus = "active"
			report.Promoted++
		} else if current.pinned == 0 && current.status == "active" && daysSinceAccess >= 90 && nextSalience < 0.25 {
			nextStatus = "low_salience"
			report.Lowered++
		} else if current.pinned == 0 && current.status == "low_salience" && daysSinceAccess >= 365 && nextSalience < 0.12 {
			nextStatus = "archived"
			report.Archived++
		}
		if math.Abs(nextSalience-current.salience) > 0.000001 {
			report.Reweighted++
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE memory_items SET salience = ?, status = ?, last_consolidated_at = ?, updated_at = ? WHERE id = ?`,
			nextSalience, nextStatus, formatTime(now), formatTime(now), current.id); err != nil {
			tx.Rollback()
			return report, err
		}
		if nextStatus != current.status {
			if err := appendEvent(ctx, tx, "memory", current.id, "memory.lifecycle_changed", map[string]any{
				"from": current.status, "to": nextStatus,
			}, now); err != nil {
				tx.Rollback()
				return report, err
			}
		}
		if err := tx.Commit(); err != nil {
			return report, err
		}
	}
	if err := s.consolidateMemoryAssociations(ctx, now, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Store) consolidateMemoryAssociations(ctx context.Context, now time.Time, report *memory.ConsolidationReport) error {
	rows, err := s.db.QueryContext(ctx, `SELECT left_memory_id, right_memory_id, weight, last_reinforced_at FROM memory_associations`)
	if err != nil {
		return err
	}
	type association struct {
		left, right, reinforced string
		weight                  float64
	}
	items := make([]association, 0)
	for rows.Next() {
		var item association
		if err := rows.Scan(&item.left, &item.right, &item.weight, &item.reinforced); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		last, err := parseTime(item.reinforced)
		if err != nil {
			return err
		}
		days := maxFloat(0, now.Sub(last).Hours()/24)
		if days < 30 {
			continue
		}
		next := item.weight * math.Exp(-days/365)
		if next < .05 {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_associations WHERE left_memory_id = ? AND right_memory_id = ?`, item.left, item.right); err != nil {
				return err
			}
			report.AssociationsPruned++
			continue
		}
		if math.Abs(next-item.weight) > .000001 {
			if _, err := s.db.ExecContext(ctx, `UPDATE memory_associations SET weight = ?, last_reinforced_at = ? WHERE left_memory_id = ? AND right_memory_id = ?`, next, formatTime(now), item.left, item.right); err != nil {
				return err
			}
			report.AssociationsDecayed++
		}
	}
	return nil
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func (s *Store) SetMemoryUsagePolicy(ctx context.Context, memoryID, policy string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE memory_items SET usage_policy = ?, updated_at = ? WHERE id = ? AND status != 'deleted'`, policy, formatTime(now), memoryID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("memory not found")
	}
	if err := appendEvent(ctx, tx, "memory", memoryID, "memory.usage_policy_changed", map[string]any{"usage_policy": policy}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PlanDeleteMemory(ctx context.Context, memoryID string) (memory.DeletePlan, error) {
	candidate, err := s.loadMemoryCandidate(ctx, memoryID)
	if err != nil {
		return memory.DeletePlan{}, err
	}
	refs := append([]content.Ref{candidate.StatementRef}, candidate.EvidenceRefs...)
	var semanticRefJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT vector_ref_json FROM memory_semantic_index WHERE memory_id = ?`, memoryID).Scan(&semanticRefJSON); err == nil {
		var semanticRef content.Ref
		if err := json.Unmarshal([]byte(semanticRefJSON), &semanticRef); err != nil {
			return memory.DeletePlan{}, err
		}
		refs = append(refs, semanticRef)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return memory.DeletePlan{}, err
	}
	tasks, err := tasksReferencingMemory(ctx, s.db, memoryID)
	if err != nil {
		return memory.DeletePlan{}, err
	}
	episodes, datasets, snapshots, err := countDerivedMemoryData(ctx, s.db, tasks)
	if err != nil {
		return memory.DeletePlan{}, err
	}
	return memory.DeletePlan{
		MemoryID: memoryID, ClaimID: candidate.ClaimID, ContentRefs: refs,
		Affected: map[string]int{
			"claims": 1, "beliefs": 1, "memory_items": 1, "evidence": len(candidate.EvidenceRefs), "semantic_indexes": len(refs) - len(candidate.EvidenceRefs) - 1,
			"episodes_invalidated": episodes, "dataset_candidates_invalidated": datasets,
			"dataset_snapshots_invalidated": snapshots,
		},
	}, nil
}

func (s *Store) CommitDeleteMemory(ctx context.Context, plan memory.DeletePlan) error {
	now := time.Now().UTC()
	refsJSON, err := json.Marshal(plan.ContentRefs)
	if err != nil {
		return err
	}
	affectedJSON, err := json.Marshal(plan.Affected)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_delete_jobs(memory_id, claim_id, refs_json, affected_json, status, created_at)
		VALUES(?, ?, ?, ?, 'pending', ?)`, plan.MemoryID, plan.ClaimID, string(refsJSON), string(affectedJSON), formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_terms WHERE memory_id = ?`, plan.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_associations WHERE left_memory_id = ? OR right_memory_id = ?`, plan.MemoryID, plan.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_retrieval_items WHERE memory_id = ?`, plan.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_semantic_index WHERE memory_id = ?`, plan.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_evidence WHERE claim_id = ?`, plan.ClaimID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_beliefs WHERE claim_id = ?`, plan.ClaimID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_items WHERE id = ? AND claim_id = ?`, plan.MemoryID, plan.ClaimID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_claims WHERE id = ?`, plan.ClaimID); err != nil {
		return err
	}
	tasks, err := tasksReferencingMemory(ctx, tx, plan.MemoryID)
	if err != nil {
		return err
	}
	for _, taskID := range tasks {
		if _, err := tx.ExecContext(ctx, `
			UPDATE dataset_snapshots SET status = 'invalidated'
			WHERE id IN (
				SELECT dsi.snapshot_id FROM dataset_snapshot_items dsi
				JOIN dataset_candidates dc ON dc.id = dsi.candidate_id
				JOIN episodes e ON e.id = dc.episode_id
				WHERE e.task_id = ?
			) AND status <> 'invalidated'`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE dataset_candidates SET status = 'invalidated'
			WHERE episode_id IN (SELECT id FROM episodes WHERE task_id = ?) AND status <> 'invalidated'`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE episodes SET status = 'invalidated' WHERE task_id = ? AND status <> 'invalidated'`, taskID); err != nil {
			return err
		}
	}
	for _, ref := range plan.ContentRefs {
		if _, err := tx.ExecContext(ctx, `UPDATE content_objects SET deleted_at = ? WHERE object_id = ? AND version = ?`, formatTime(now), ref.ObjectID, ref.Version); err != nil {
			return err
		}
	}
	if err := appendEvent(ctx, tx, "memory", plan.MemoryID, "memory.deleted", map[string]any{"affected": plan.Affected}, now); err != nil {
		return err
	}
	return tx.Commit()
}

type memoryQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func tasksReferencingMemory(ctx context.Context, queryer memoryQueryer, memoryID string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT task_id, context_manifest_json FROM invocations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := map[string]struct{}{}
	for rows.Next() {
		var taskID, encoded string
		if err := rows.Scan(&taskID, &encoded); err != nil {
			return nil, err
		}
		var manifest struct {
			Memory []string `json:"memory"`
		}
		if json.Unmarshal([]byte(encoded), &manifest) != nil {
			continue
		}
		for _, candidate := range manifest.Memory {
			if candidate == memoryID {
				tasks[taskID] = struct{}{}
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(tasks))
	for taskID := range tasks {
		result = append(result, taskID)
	}
	sort.Strings(result)
	return result, nil
}

func countDerivedMemoryData(ctx context.Context, queryer memoryQueryer, tasks []string) (int, int, int, error) {
	episodes, datasets := 0, 0
	snapshotIDs := map[string]struct{}{}
	for _, taskID := range tasks {
		var episodeCount, datasetCount int
		if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM episodes WHERE task_id = ? AND status <> 'invalidated'`, taskID).Scan(&episodeCount); err != nil {
			return 0, 0, 0, err
		}
		if err := queryer.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM dataset_candidates
			WHERE episode_id IN (SELECT id FROM episodes WHERE task_id = ?) AND status <> 'invalidated'`, taskID).Scan(&datasetCount); err != nil {
			return 0, 0, 0, err
		}
		rows, err := queryer.QueryContext(ctx, `
			SELECT DISTINCT dsi.snapshot_id FROM dataset_snapshot_items dsi
			JOIN dataset_candidates dc ON dc.id = dsi.candidate_id
			JOIN episodes e ON e.id = dc.episode_id
			JOIN dataset_snapshots ds ON ds.id = dsi.snapshot_id
			WHERE e.task_id = ? AND ds.status <> 'invalidated'`, taskID)
		if err != nil {
			return 0, 0, 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, 0, err
			}
			snapshotIDs[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return 0, 0, 0, err
		}
		episodes += episodeCount
		datasets += datasetCount
	}
	return episodes, datasets, len(snapshotIDs), nil
}

func (s *Store) PendingMemoryDeletes(ctx context.Context, limit int) ([]memory.DeletePlan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT memory_id, claim_id, refs_json, affected_json FROM memory_delete_jobs
		WHERE status = 'pending' ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := make([]memory.DeletePlan, 0)
	for rows.Next() {
		var plan memory.DeletePlan
		var refsJSON, affectedJSON string
		if err := rows.Scan(&plan.MemoryID, &plan.ClaimID, &refsJSON, &affectedJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refsJSON), &plan.ContentRefs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(affectedJSON), &plan.Affected); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

func (s *Store) CompleteDeleteMemory(ctx context.Context, memoryID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_delete_jobs SET status = 'completed', completed_at = ?
		WHERE memory_id = ? AND status = 'pending'`, formatTime(time.Now().UTC()), memoryID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("memory deletion %s is not pending", memoryID)
	}
	return nil
}
