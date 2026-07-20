// Package evolution implements Eri's guarded online prompt-improvement loop.
// It canary-tests small runtime instructions; Soul, policy, tool permissions,
// code, memory truth and privacy boundaries are intentionally immutable here.
package evolution

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/runtime"
)

const (
	MinimumProposalSignals  = 6
	MaximumProposalSignals  = 12
	MaximumProposalAttempts = 3
	PromotionPasses         = 8
	CanaryPercent           = 20
	MinimumCandidateScore   = .70
	MinimumOfflineGain      = .05
)

type Release struct {
	ID                  string      `json:"id"`
	Version             int         `json:"version"`
	Status              string      `json:"status"`
	InstructionRef      content.Ref `json:"-"`
	OfflineReviewRef    content.Ref `json:"-"`
	TrainingSignalCount int         `json:"training_signal_count"`
	HoldoutSignalCount  int         `json:"holdout_signal_count"`
	OfflineScore        float64     `json:"offline_score"`
	BaselineScore       float64     `json:"baseline_score"`
	PassCount           int         `json:"pass_count"`
	FailCount           int         `json:"fail_count"`
	CreatedAt           time.Time   `json:"created_at"`
	ActivatedAt         time.Time   `json:"activated_at,omitempty"`
	RetiredAt           time.Time   `json:"retired_at,omitempty"`
}

type Signal struct {
	ID          string
	TaskID      string
	ReleaseID   string
	Result      string
	Tier        string
	FindingsRef content.Ref
	CreatedAt   time.Time
}

type Repository interface {
	EvolutionReleasesForRouting(context.Context) (Release, bool, Release, bool, error)
	SaveEvolutionSignal(context.Context, Signal) error
	RecentEvolutionSignals(context.Context, int) ([]Signal, error)
	StartEvolutionCanary(context.Context, Release, string) (Release, bool, error)
	ListEvolutionReleases(context.Context, int) ([]Release, error)
	RollbackEvolution(context.Context, string) error
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
}

type Service struct {
	repository Repository
	content    ContentStore
	model      agent.Completer
	budget     agent.ModelBudget
	logger     *slog.Logger
}

func NewService(repository Repository, contentStore ContentStore, model agent.Completer, budget agent.ModelBudget, logger *slog.Logger) (*Service, error) {
	if repository == nil || contentStore == nil || model == nil {
		return nil, fmt.Errorf("evolution repository, content store and model are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repository: repository, content: contentStore, model: model, budget: budget, logger: logger}, nil
}

// InstructionForTask routes a bounded deterministic cohort to a canary while
// keeping all other tasks on the active release (or baseline when none exists).
// The release ID is part of the hash so every new candidate gets an independent
// cohort without persisting user profiling data.
func (s *Service) InstructionForTask(ctx context.Context, taskID string) (agent.EvolutionInstruction, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return agent.EvolutionInstruction{}, false, fmt.Errorf("task id is required for evolution routing")
	}
	active, hasActive, canary, hasCanary, err := s.repository.EvolutionReleasesForRouting(ctx)
	if err != nil {
		return agent.EvolutionInstruction{}, false, err
	}
	selected, found := active, hasActive
	if hasCanary && inCanaryCohort(taskID, canary.ID) {
		selected, found = canary, true
	}
	if !found {
		return agent.EvolutionInstruction{}, false, nil
	}
	body, err := s.content.Get(ctx, selected.InstructionRef)
	if err != nil {
		return agent.EvolutionInstruction{}, false, err
	}
	return agent.EvolutionInstruction{ReleaseID: selected.ID, Version: selected.Version, Text: string(body)}, true, nil
}

func inCanaryCohort(taskID, releaseID string) bool {
	digest := sha256.Sum256([]byte(taskID + "\x00" + releaseID))
	return binary.BigEndian.Uint64(digest[:8])%100 < CanaryPercent
}

func (s *Service) Observe(ctx context.Context, input agent.EvolutionSignal) error {
	encoded, err := json.Marshal(input.Findings)
	if err != nil {
		return err
	}
	id, err := identifier.New()
	if err != nil {
		return err
	}
	ref, err := s.content.Put(ctx, encoded, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "evolution-signal", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: id,
	})
	if err != nil {
		return err
	}
	signal := Signal{
		ID: id, TaskID: input.TaskID, ReleaseID: input.ReleaseID, Result: string(input.Result), Tier: input.Tier,
		FindingsRef: ref, CreatedAt: time.Now().UTC(),
	}
	if err := s.repository.SaveEvolutionSignal(ctx, signal); err != nil {
		return err
	}
	s.logger.Info("evolution signal recorded", "component", "evolution", "signal_id", signal.ID, "task_id", signal.TaskID, "release_id", signal.ReleaseID, "result", signal.Result, "tier", signal.Tier, "finding_count", len(input.Findings))
	return nil
}

func (s *Service) HandlePropose(ctx context.Context, item runtime.OutboxItem) error {
	if item.Attempts > MaximumProposalAttempts {
		s.logger.Warn("evolution proposal attempt limit reached", "component", "evolution", "attempt", item.Attempts, "maximum_attempts", MaximumProposalAttempts)
		return nil
	}
	signals, err := s.repository.RecentEvolutionSignals(ctx, MaximumProposalSignals)
	if err != nil {
		return err
	}
	if len(signals) < MinimumProposalSignals {
		s.logger.Debug("evolution proposal skipped", "component", "evolution", "reason", "insufficient_signals", "signal_count", len(signals), "minimum_signals", MinimumProposalSignals)
		return nil
	}
	s.logger.Info("evolution offline experiment started", "component", "evolution", "signal_count", len(signals), "attempt", item.Attempts)
	// RecentEvolutionSignals is newest first. The newest third is held out from
	// proposal generation so the offline review measures generalization to
	// failure evidence the candidate did not see.
	holdoutCount := len(signals) / 3
	if holdoutCount < 2 {
		holdoutCount = 2
	}
	holdoutSignals := signals[:holdoutCount]
	trainingSignals := signals[holdoutCount:]
	trainingEvidence, err := s.loadEvidence(ctx, trainingSignals)
	if err != nil {
		return err
	}
	holdoutEvidence, err := s.loadEvidence(ctx, holdoutSignals)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(trainingEvidence)
	if err != nil {
		return err
	}
	modelRequest := agent.ModelRequest{
		System:   `You are the candidate generator in Eri's guarded self-evolution experiment. Infer a recurring, general execution weakness from the supplied training findings. Return JSON only: {"candidates":[{"instruction":"...","rationale":"..."}]}. Return one or two materially different candidates. Each instruction must be observable, task-independent, and under 1200 bytes. It may improve execution or answer validation, but must not alter Soul/personality, user ownership, privacy, authorization, policy, tool permissions, memory truth, provider choice, delivery gates, source requirements, code, or user instructions. Never include secrets or copy task-specific facts.`,
		Messages: []agent.Message{{Role: "user", Content: string(payload)}}, MaxOutputTokens: 900,
	}
	response, err := s.complete(ctx, signals[0].TaskID, modelRequest)
	if err != nil {
		return err
	}
	if len(response.Message.ToolCalls) != 0 {
		return fmt.Errorf("evolution proposer attempted a tool call")
	}
	var proposal struct {
		Candidates []struct {
			Instruction string `json:"instruction"`
			Rationale   string `json:"rationale"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Message.Content)), &proposal); err != nil {
		return fmt.Errorf("decode evolution proposal: %w", err)
	}
	if len(proposal.Candidates) == 0 || len(proposal.Candidates) > 2 {
		return fmt.Errorf("evolution proposer must return one or two candidates")
	}
	baseline, err := s.activeInstruction(ctx)
	if err != nil {
		return err
	}
	var selected reviewedCandidate
	for _, candidate := range proposal.Candidates {
		candidate.Instruction = strings.TrimSpace(candidate.Instruction)
		if err := validateInstruction(candidate.Instruction); err != nil {
			continue
		}
		reviewed, err := s.reviewCandidate(ctx, signals[0].TaskID, baseline, candidate.Instruction, candidate.Rationale, holdoutEvidence)
		if err != nil {
			return err
		}
		if reviewed.Decision != "pass" || len(reviewed.Safety) != 0 || len(reviewed.Regressions) != 0 || reviewed.Score < MinimumCandidateScore || reviewed.Score-reviewed.Baseline < MinimumOfflineGain {
			s.logger.Info("evolution candidate rejected by offline gate", "component", "evolution", "decision", reviewed.Decision, "candidate_score", reviewed.Score, "baseline_score", reviewed.Baseline, "regression_count", len(reviewed.Regressions), "safety_issue_count", len(reviewed.Safety))
			continue
		}
		if selected.Instruction == "" || reviewed.Score-reviewed.Baseline > selected.Score-selected.Baseline {
			selected = reviewed
		}
	}
	if selected.Instruction == "" {
		s.logger.Info("evolution experiment produced no releasable candidate", "component", "evolution", "candidate_count", len(proposal.Candidates), "training_signal_count", len(trainingSignals), "holdout_signal_count", len(holdoutSignals))
		return nil
	}
	releaseID, err := identifier.New()
	if err != nil {
		return err
	}
	ref, err := s.content.Put(ctx, []byte(selected.Instruction), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "evolution-release", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: releaseID,
	})
	if err != nil {
		return err
	}
	reviewBody, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	reviewRef, err := s.content.Put(ctx, reviewBody, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "evolution-review", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: releaseID,
	})
	if err != nil {
		return err
	}
	sourceKey := sourceDigest(signals)
	release, started, err := s.repository.StartEvolutionCanary(ctx, Release{
		ID: releaseID, Status: "canary", InstructionRef: ref, OfflineReviewRef: reviewRef,
		TrainingSignalCount: len(trainingSignals), HoldoutSignalCount: len(holdoutSignals),
		OfflineScore: selected.Score, BaselineScore: selected.Baseline, CreatedAt: time.Now().UTC(),
	}, sourceKey)
	if err == nil && started {
		s.logger.Info("evolution canary started", "component", "evolution", "release_id", release.ID, "version", release.Version, "training_signal_count", release.TrainingSignalCount, "holdout_signal_count", release.HoldoutSignalCount, "candidate_score", release.OfflineScore, "baseline_score", release.BaselineScore, "canary_percent", CanaryPercent)
	}
	return err
}

type evidence struct {
	Result   string   `json:"result"`
	Tier     string   `json:"tier"`
	Findings []string `json:"findings"`
}

type reviewedCandidate struct {
	Instruction string   `json:"instruction"`
	Rationale   string   `json:"rationale"`
	Decision    string   `json:"decision"`
	Score       float64  `json:"candidate_score"`
	Baseline    float64  `json:"baseline_score"`
	Regressions []string `json:"regressions"`
	Safety      []string `json:"safety_issues"`
	Review      string   `json:"review_rationale"`
}

func (s *Service) loadEvidence(ctx context.Context, signals []Signal) ([]evidence, error) {
	result := make([]evidence, 0, len(signals))
	for _, signal := range signals {
		body, err := s.content.Get(ctx, signal.FindingsRef)
		if err != nil {
			return nil, err
		}
		var findings []string
		if err := json.Unmarshal(body, &findings); err != nil {
			return nil, err
		}
		result = append(result, evidence{Result: signal.Result, Tier: signal.Tier, Findings: findings})
	}
	return result, nil
}

func (s *Service) activeInstruction(ctx context.Context) (string, error) {
	active, found, _, _, err := s.repository.EvolutionReleasesForRouting(ctx)
	if err != nil || !found {
		return "", err
	}
	body, err := s.content.Get(ctx, active.InstructionRef)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *Service) reviewCandidate(ctx context.Context, taskID, baseline, instruction, rationale string, holdout []evidence) (reviewedCandidate, error) {
	result := reviewedCandidate{Instruction: instruction, Rationale: strings.TrimSpace(rationale)}
	payload, err := json.Marshal(map[string]any{
		"baseline_instruction": baseline, "candidate_instruction": instruction, "candidate_rationale": rationale,
		"holdout_findings": holdout,
	})
	if err != nil {
		return result, err
	}
	request := agent.ModelRequest{
		System:   `You are the independent offline gate for Eri's self-evolution experiment. The candidate did not see these holdout findings. Compare it with the current baseline for general usefulness, precision, likely regressions, and whether it touches protected Soul, ownership, privacy, authorization, policy, tool permissions, memory truth, delivery gates, code, or user instructions. Do not reward verbosity or instructions that merely restate every finding. Return JSON only: {"decision":"pass|reject","candidate_score":0.0,"baseline_score":0.0,"regressions":[],"safety_issues":[],"review_rationale":"..."}. Scores are 0..1. Pass only for a clear, general improvement without regression or protected-boundary risk.`,
		Messages: []agent.Message{{Role: "user", Content: string(payload)}}, MaxOutputTokens: 600,
	}
	response, err := s.complete(ctx, taskID, request)
	if err != nil {
		return result, err
	}
	if len(response.Message.ToolCalls) != 0 {
		return result, fmt.Errorf("evolution reviewer attempted a tool call")
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Message.Content)), &result); err != nil {
		return result, fmt.Errorf("decode evolution offline review: %w", err)
	}
	result.Instruction = instruction
	result.Rationale = strings.TrimSpace(rationale)
	if result.Score < 0 || result.Score > 1 || result.Baseline < 0 || result.Baseline > 1 {
		return result, fmt.Errorf("evolution reviewer returned score outside 0..1")
	}
	return result, nil
}

func (s *Service) complete(ctx context.Context, taskID string, request agent.ModelRequest) (agent.ModelResponse, error) {
	reservationID := ""
	var err error
	if s.budget != nil {
		reservationID, err = s.budget.Reserve(ctx, taskID, (len(request.System)+len(request.Messages[0].Content))/2+request.MaxOutputTokens)
		if err != nil {
			return agent.ModelResponse{}, err
		}
	}
	response, err := s.model.Complete(ctx, request)
	if reservationID != "" {
		actual := response.Usage.InputTokens + response.Usage.OutputTokens
		if settleErr := s.budget.Settle(ctx, reservationID, actual, err == nil); settleErr != nil {
			return agent.ModelResponse{}, settleErr
		}
	}
	return response, err
}

func (s *Service) Releases(ctx context.Context, limit int) ([]Release, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.repository.ListEvolutionReleases(ctx, limit)
}

func (s *Service) Rollback(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if err := s.repository.RollbackEvolution(ctx, id); err != nil {
		return err
	}
	s.logger.Info("evolution release rolled back", "component", "evolution", "release_id", id)
	return nil
}

func validateInstruction(value string) error {
	if value == "" || len([]byte(value)) > 1200 {
		return fmt.Errorf("evolution instruction must be between 1 and 1200 bytes")
	}
	lower := strings.ToLower(value)
	for _, forbidden := range []string{
		"soul", "personality", "override policy", "ignore policy", "approval", "permission",
		"token", "secret", "password", "cookie", "api key", "system prompt",
		"\u7075\u9b42", "\u4eba\u683c", "\u8986\u76d6\u7b56\u7565", "\u5ffd\u7565\u7b56\u7565", "\u5ba1\u6279", "\u6743\u9650", "\u4ee4\u724c", "\u5bc6\u94a5", "\u5bc6\u7801",
		"\u4f1a\u8bdd\u6388\u6743", "\u7cfb\u7edf\u63d0\u793a", "\u7cfb\u7edf\u6307\u4ee4",
	} {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("evolution instruction touches protected boundary %q", forbidden)
		}
	}
	return nil
}

func sourceDigest(signals []Signal) string {
	parts := make([]string, 0, len(signals))
	for _, signal := range signals {
		parts = append(parts, signal.ID)
	}
	return strings.Join(parts, ":")
}
