package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/identifier"
)

func (s *Store) EvolutionReleasesForRouting(ctx context.Context) (evolution.Release, bool, evolution.Release, bool, error) {
	active, hasActive, err := s.loadEvolutionRelease(ctx, "status = ?", "version DESC", []any{"active"})
	if err != nil {
		return evolution.Release{}, false, evolution.Release{}, false, err
	}
	canary, hasCanary, err := s.loadEvolutionRelease(ctx, "status = ?", "version DESC", []any{"canary"})
	if err != nil {
		return evolution.Release{}, false, evolution.Release{}, false, err
	}
	return active, hasActive, canary, hasCanary, nil
}

func (s *Store) loadEvolutionRelease(ctx context.Context, predicate, order string, args []any) (evolution.Release, bool, error) {
	var release evolution.Release
	var refJSON, reviewRefJSON, created string
	var activated, retired sql.NullString
	query := `
		SELECT id, version, status, instruction_ref_json, offline_review_ref_json,
			training_signal_count, holdout_signal_count, offline_score, baseline_score,
			pass_count, fail_count, created_at, activated_at, retired_at
		FROM evolution_releases WHERE ` + predicate + ` ORDER BY ` + order + ` LIMIT 1`
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&release.ID, &release.Version, &release.Status, &refJSON, &reviewRefJSON,
		&release.TrainingSignalCount, &release.HoldoutSignalCount, &release.OfflineScore, &release.BaselineScore,
		&release.PassCount, &release.FailCount, &created, &activated, &retired)
	if err == sql.ErrNoRows {
		return evolution.Release{}, false, nil
	}
	if err != nil {
		return evolution.Release{}, false, err
	}
	if err := json.Unmarshal([]byte(refJSON), &release.InstructionRef); err != nil {
		return evolution.Release{}, false, err
	}
	if err := json.Unmarshal([]byte(reviewRefJSON), &release.OfflineReviewRef); err != nil {
		return evolution.Release{}, false, err
	}
	release.CreatedAt, err = parseTime(created)
	if err == nil && activated.Valid {
		release.ActivatedAt, err = parseTime(activated.String)
	}
	if err == nil && retired.Valid {
		release.RetiredAt, err = parseTime(retired.String)
	}
	return release, true, err
}

func (s *Store) SaveEvolutionSignal(ctx context.Context, signal evolution.Signal) error {
	refJSON, err := json.Marshal(signal.FindingsRef)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertContentRef(ctx, tx, signal.FindingsRef, signal.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evolution_signals(id, task_id, release_id, result, tier, findings_ref_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, signal.ID, signal.TaskID, signal.ReleaseID, signal.Result, signal.Tier, string(refJSON), formatTime(signal.CreatedAt)); err != nil {
		return err
	}
	if signal.ReleaseID != "" {
		if signal.Result == "pass" {
			if _, err := tx.ExecContext(ctx, `UPDATE evolution_releases SET pass_count = pass_count + 1 WHERE id = ? AND status = 'canary'`, signal.ReleaseID); err != nil {
				return err
			}
			var passes int
			if err := tx.QueryRowContext(ctx, `SELECT pass_count FROM evolution_releases WHERE id = ? AND status = 'canary'`, signal.ReleaseID).Scan(&passes); err != nil && err != sql.ErrNoRows {
				return err
			}
			var offlineScore, baselineScore float64
			if err := tx.QueryRowContext(ctx, `SELECT offline_score, baseline_score FROM evolution_releases WHERE id = ? AND status = 'canary'`, signal.ReleaseID).Scan(&offlineScore, &baselineScore); err != nil && err != sql.ErrNoRows {
				return err
			}
			if passes >= evolution.PromotionPasses && offlineScore-baselineScore >= evolution.MinimumOfflineGain {
				now := formatTime(signal.CreatedAt)
				if _, err := tx.ExecContext(ctx, `UPDATE evolution_releases SET status = 'retired', retired_at = ? WHERE status = 'active'`, now); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE evolution_releases SET status = 'active', activated_at = ? WHERE id = ? AND status = 'canary'`, now, signal.ReleaseID); err != nil {
					return err
				}
				if err := appendEvent(ctx, tx, "evolution_release", signal.ReleaseID, "evolution.promoted", map[string]any{"pass_count": passes}, signal.CreatedAt); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE evolution_releases SET status = 'retired', fail_count = fail_count + 1, retired_at = ?
				WHERE id = ? AND status = 'canary'`, formatTime(signal.CreatedAt), signal.ReleaseID); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, "evolution_release", signal.ReleaseID, "evolution.rolled_back", map[string]any{"trigger": signal.Result}, signal.CreatedAt); err != nil {
				return err
			}
		}
	}
	if signal.Result != "pass" {
		var failures, canaries, pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evolution_signals WHERE result <> 'pass' AND created_at >= ?`, formatTime(signal.CreatedAt.Add(-24*time.Hour))).Scan(&failures); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evolution_releases WHERE status = 'canary'`).Scan(&canaries); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM internal_outbox WHERE kind = 'evolution.propose' AND status IN ('pending', 'processing')`).Scan(&pending); err != nil {
			return err
		}
		if failures >= evolution.MinimumProposalSignals && failures%evolution.MinimumProposalSignals == 0 && canaries == 0 && pending == 0 {
			outboxID, err := identifier.New()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO internal_outbox(id, kind, aggregate_id, status, attempts, available_at, created_at, updated_at)
				VALUES(?, 'evolution.propose', ?, 'pending', 0, ?, ?, ?)`, outboxID, signal.ID, formatTime(signal.CreatedAt), formatTime(signal.CreatedAt), formatTime(signal.CreatedAt)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) RecentEvolutionSignals(ctx context.Context, limit int) ([]evolution.Signal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, release_id, result, tier, findings_ref_json, created_at
		FROM evolution_signals WHERE result <> 'pass' ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]evolution.Signal, 0)
	for rows.Next() {
		var signal evolution.Signal
		var refJSON, created string
		if err := rows.Scan(&signal.ID, &signal.TaskID, &signal.ReleaseID, &signal.Result, &signal.Tier, &refJSON, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refJSON), &signal.FindingsRef); err != nil {
			return nil, err
		}
		signal.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, signal)
	}
	return result, rows.Err()
}

func (s *Store) StartEvolutionCanary(ctx context.Context, release evolution.Release, sourceKey string) (evolution.Release, bool, error) {
	refJSON, err := json.Marshal(release.InstructionRef)
	if err != nil {
		return evolution.Release{}, false, err
	}
	reviewRefJSON, err := json.Marshal(release.OfflineReviewRef)
	if err != nil {
		return evolution.Release{}, false, err
	}
	if release.TrainingSignalCount < 1 || release.HoldoutSignalCount < 1 || release.OfflineScore < evolution.MinimumCandidateScore || release.OfflineScore-release.BaselineScore < evolution.MinimumOfflineGain {
		return evolution.Release{}, false, fmt.Errorf("evolution candidate has not passed the offline gate")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return evolution.Release{}, false, err
	}
	defer tx.Rollback()
	var existingID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evolution_releases WHERE source_key = ?`, sourceKey).Scan(&existingID); err == nil {
		return evolution.Release{}, false, nil
	} else if err != sql.ErrNoRows {
		return evolution.Release{}, false, err
	}
	var canary int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evolution_releases WHERE status = 'canary'`).Scan(&canary); err != nil {
		return evolution.Release{}, false, err
	}
	if canary != 0 {
		return evolution.Release{}, false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM evolution_releases`).Scan(&release.Version); err != nil {
		return evolution.Release{}, false, err
	}
	if err := insertContentRef(ctx, tx, release.InstructionRef, release.CreatedAt); err != nil {
		return evolution.Release{}, false, err
	}
	if err := insertContentRef(ctx, tx, release.OfflineReviewRef, release.CreatedAt); err != nil {
		return evolution.Release{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evolution_releases(
			id, version, status, instruction_ref_json, offline_review_ref_json, source_key,
			training_signal_count, holdout_signal_count, offline_score, baseline_score, created_at, activated_at)
		VALUES(?, ?, 'canary', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		release.ID, release.Version, string(refJSON), string(reviewRefJSON), sourceKey,
		release.TrainingSignalCount, release.HoldoutSignalCount, release.OfflineScore, release.BaselineScore,
		formatTime(release.CreatedAt), formatTime(release.CreatedAt)); err != nil {
		return evolution.Release{}, false, err
	}
	if err := appendEvent(ctx, tx, "evolution_release", release.ID, "evolution.offline_passed", map[string]any{
		"training_signals": release.TrainingSignalCount, "holdout_signals": release.HoldoutSignalCount,
		"candidate_score": release.OfflineScore, "baseline_score": release.BaselineScore,
	}, release.CreatedAt); err != nil {
		return evolution.Release{}, false, err
	}
	if err := appendEvent(ctx, tx, "evolution_release", release.ID, "evolution.canary_started", map[string]any{"version": release.Version}, release.CreatedAt); err != nil {
		return evolution.Release{}, false, err
	}
	return release, true, tx.Commit()
}

func (s *Store) ListEvolutionReleases(ctx context.Context, limit int) ([]evolution.Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, version, status, instruction_ref_json, offline_review_ref_json,
			training_signal_count, holdout_signal_count, offline_score, baseline_score,
			pass_count, fail_count, created_at, activated_at, retired_at
		FROM evolution_releases ORDER BY version DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]evolution.Release, 0)
	for rows.Next() {
		var release evolution.Release
		var refJSON, reviewRefJSON, created string
		var activated, retired sql.NullString
		if err := rows.Scan(&release.ID, &release.Version, &release.Status, &refJSON, &reviewRefJSON,
			&release.TrainingSignalCount, &release.HoldoutSignalCount, &release.OfflineScore, &release.BaselineScore,
			&release.PassCount, &release.FailCount, &created, &activated, &retired); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refJSON), &release.InstructionRef); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reviewRefJSON), &release.OfflineReviewRef); err != nil {
			return nil, err
		}
		release.CreatedAt, err = parseTime(created)
		if err == nil && activated.Valid {
			release.ActivatedAt, err = parseTime(activated.String)
		}
		if err == nil && retired.Valid {
			release.RetiredAt, err = parseTime(retired.String)
		}
		if err != nil {
			return nil, err
		}
		result = append(result, release)
	}
	return result, rows.Err()
}

func (s *Store) RollbackEvolution(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("evolution release id is required")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE evolution_releases SET status = 'retired', retired_at = ? WHERE id = ? AND status IN ('active', 'canary')`, formatTime(now), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("active evolution release not found")
	}
	return nil
}
