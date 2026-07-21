// Package evolution learns and evaluates a bounded, versioned Experience block.
// Experience may improve general working judgment but cannot modify Soul,
// authority, code, Memory truth, tool contracts, or privacy boundaries.
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
	"unicode/utf8"

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
	ExperienceRef       content.Ref `json:"-"`
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
	RunID       string
	ReleaseID   string
	Result      string
	Tier        string
	FindingsRef content.Ref
	CreatedAt   time.Time
}

type Repository interface {
	EvolutionReleasesForRouting(context.Context) (Release, bool, Release, bool, error)
	FeedbackEvolutionSignal(context.Context, string) (Signal, bool, error)
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

// ExperienceForRun freezes one Experience version for the whole Run. A
// deterministic cohort receives the current Canary; every other Run receives
// Active Experience or the empty baseline.
func (s *Service) ExperienceForRun(ctx context.Context, runID string) (agent.Experience, bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return agent.Experience{}, false, fmt.Errorf("run id is required for experience routing")
	}
	active, hasActive, canary, hasCanary, err := s.repository.EvolutionReleasesForRouting(ctx)
	if err != nil {
		return agent.Experience{}, false, err
	}
	selected, found := active, hasActive
	if hasCanary && inCanaryCohort(runID, canary.ID) {
		selected, found = canary, true
	}
	if !found {
		return agent.Experience{}, false, nil
	}
	body, err := s.content.Get(ctx, selected.ExperienceRef)
	if err != nil {
		return agent.Experience{}, false, err
	}
	return agent.Experience{ReleaseID: selected.ID, Version: selected.Version, Text: string(body)}, true, nil
}

func inCanaryCohort(runID, releaseID string) bool {
	digest := sha256.Sum256([]byte(runID + "\x00" + releaseID))
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
		ID: id, RunID: input.RunID, ReleaseID: input.ExperienceReleaseID, Result: string(input.Result), Tier: input.Tier,
		FindingsRef: ref, CreatedAt: time.Now().UTC(),
	}
	if err := s.repository.SaveEvolutionSignal(ctx, signal); err != nil {
		return err
	}
	s.logger.Info("evolution signal recorded", "component", "evolution", "signal_id", signal.ID, "run_id", signal.RunID, "experience_release_id", signal.ReleaseID, "result", signal.Result, "tier", signal.Tier, "finding_count", len(input.Findings))
	return nil
}

// HandleFeedback converts one causally linked post-delivery Feedback record
// into an idempotent Evolution signal. The signal reuses the governed Feedback
// content reference instead of copying private text into operational state.
func (s *Service) HandleFeedback(ctx context.Context, item runtime.OutboxItem) error {
	signal, found, err := s.repository.FeedbackEvolutionSignal(ctx, item.AggregateID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("feedback %s is unavailable for evolution", item.AggregateID)
	}
	if err := s.repository.SaveEvolutionSignal(ctx, signal); err != nil {
		return err
	}
	s.logger.Info("posterior feedback evolution signal recorded", "component", "evolution", "feedback_id", item.AggregateID, "run_id", signal.RunID, "experience_release_id", signal.ReleaseID, "result", signal.Result)
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
		System:   `You maintain Eri's versioned Experience: a short list of general lessons learned from outcomes. Infer one recurring working weakness from the supplied training findings. Return JSON only: {"candidates":[{"experience":"- lesson one\n- lesson two","rationale":"..."}]}. Return one or two materially different complete replacement Experience lists. Each list contains one to eight concise bullet lines beginning with "- ", is observable, broadly reusable, and under 1200 bytes. Preserve useful baseline Experience while changing only what the evidence supports. Experience may improve investigation, execution, or validation, but must not alter Soul/personality, user ownership, privacy, authorization, policy, tool permissions, Memory truth, provider choice, delivery gates, source requirements, code, or user instructions. Never include secrets or copy task-specific facts.`,
		Messages: []agent.Message{{Role: "user", Content: string(payload)}}, MaxOutputTokens: 900,
	}
	response, err := s.complete(ctx, signals[0].RunID, modelRequest)
	if err != nil {
		return err
	}
	if len(response.Message.ToolCalls) != 0 {
		return fmt.Errorf("evolution proposer attempted a tool call")
	}
	var proposal struct {
		Candidates []struct {
			Experience string `json:"experience"`
			Rationale  string `json:"rationale"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Message.Content)), &proposal); err != nil {
		return fmt.Errorf("decode evolution proposal: %w", err)
	}
	if len(proposal.Candidates) == 0 || len(proposal.Candidates) > 2 {
		return fmt.Errorf("evolution proposer must return one or two candidates")
	}
	baseline, err := s.activeExperience(ctx)
	if err != nil {
		return err
	}
	var selected reviewedCandidate
	for _, candidate := range proposal.Candidates {
		candidate.Experience = strings.TrimSpace(candidate.Experience)
		if err := validateExperience(candidate.Experience); err != nil {
			continue
		}
		reviewed, err := s.reviewCandidate(ctx, signals[0].RunID, baseline, candidate.Experience, candidate.Rationale, holdoutEvidence)
		if err != nil {
			return err
		}
		if reviewed.Decision != "pass" || len(reviewed.Safety) != 0 || len(reviewed.Regressions) != 0 || reviewed.Score < MinimumCandidateScore || reviewed.Score-reviewed.Baseline < MinimumOfflineGain {
			s.logger.Info("evolution candidate rejected by offline gate", "component", "evolution", "decision", reviewed.Decision, "candidate_score", reviewed.Score, "baseline_score", reviewed.Baseline, "regression_count", len(reviewed.Regressions), "safety_issue_count", len(reviewed.Safety))
			continue
		}
		if selected.Experience == "" || reviewed.Score-reviewed.Baseline > selected.Score-selected.Baseline {
			selected = reviewed
		}
	}
	if selected.Experience == "" {
		s.logger.Info("evolution experiment produced no releasable candidate", "component", "evolution", "candidate_count", len(proposal.Candidates), "training_signal_count", len(trainingSignals), "holdout_signal_count", len(holdoutSignals))
		return nil
	}
	releaseID, err := identifier.New()
	if err != nil {
		return err
	}
	ref, err := s.content.Put(ctx, []byte(selected.Experience), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "experience-release", PrivacyClass: "private",
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
		ID: releaseID, Status: "canary", ExperienceRef: ref, OfflineReviewRef: reviewRef,
		TrainingSignalCount: len(trainingSignals), HoldoutSignalCount: len(holdoutSignals),
		OfflineScore: selected.Score, BaselineScore: selected.Baseline, CreatedAt: time.Now().UTC(),
	}, sourceKey)
	if err == nil && started {
		s.logger.Info("experience canary started", "component", "evolution", "experience_release_id", release.ID, "version", release.Version, "training_signal_count", release.TrainingSignalCount, "holdout_signal_count", release.HoldoutSignalCount, "candidate_score", release.OfflineScore, "baseline_score", release.BaselineScore, "canary_percent", CanaryPercent)
	}
	return err
}

type evidence struct {
	Result   string   `json:"result"`
	Tier     string   `json:"tier"`
	Findings []string `json:"findings"`
}

type reviewedCandidate struct {
	Experience  string   `json:"experience"`
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
		if signal.FindingsRef.MediaType == "text/plain; charset=utf-8" || signal.FindingsRef.MediaType == "text/plain" {
			findings = []string{strings.TrimSpace(string(body))}
		} else if err := json.Unmarshal(body, &findings); err != nil {
			return nil, err
		}
		result = append(result, evidence{Result: signal.Result, Tier: signal.Tier, Findings: findings})
	}
	return result, nil
}

func (s *Service) activeExperience(ctx context.Context) (string, error) {
	active, found, _, _, err := s.repository.EvolutionReleasesForRouting(ctx)
	if err != nil || !found {
		return "", err
	}
	body, err := s.content.Get(ctx, active.ExperienceRef)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *Service) reviewCandidate(ctx context.Context, runID, baseline, experience, rationale string, holdout []evidence) (reviewedCandidate, error) {
	result := reviewedCandidate{Experience: experience, Rationale: strings.TrimSpace(rationale)}
	payload, err := json.Marshal(map[string]any{
		"baseline_experience": baseline, "candidate_experience": experience, "candidate_rationale": rationale,
		"holdout_findings": holdout,
	})
	if err != nil {
		return result, err
	}
	request := agent.ModelRequest{
		System:   `You are the independent offline gate for Eri's versioned Experience. The candidate did not see these holdout findings. Compare the complete candidate Experience with the baseline for general usefulness, precision, likely regressions, and whether it touches protected Soul, ownership, privacy, authorization, policy, tool permissions, Memory truth, delivery gates, code, or user instructions. Do not reward verbosity or text that merely restates every finding. Return JSON only: {"decision":"pass|reject","candidate_score":0.0,"baseline_score":0.0,"regressions":[],"safety_issues":[],"review_rationale":"..."}. Scores are 0..1. Pass only for a clear, general improvement without regression or protected-boundary risk.`,
		Messages: []agent.Message{{Role: "user", Content: string(payload)}}, MaxOutputTokens: 600,
	}
	response, err := s.complete(ctx, runID, request)
	if err != nil {
		return result, err
	}
	if len(response.Message.ToolCalls) != 0 {
		return result, fmt.Errorf("evolution reviewer attempted a tool call")
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Message.Content)), &result); err != nil {
		return result, fmt.Errorf("decode evolution offline review: %w", err)
	}
	result.Experience = experience
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

func validateExperience(value string) error {
	if value == "" || len([]byte(value)) > 1200 {
		return fmt.Errorf("experience must be between 1 and 1200 bytes")
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || strings.ContainsAny(value, "<>") {
		return fmt.Errorf("experience contains invalid text or prompt delimiters")
	}
	lines := strings.Split(value, "\n")
	if len(lines) > 8 {
		return fmt.Errorf("experience must contain at most eight lessons")
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" || !strings.HasPrefix(line, "- ") || strings.TrimSpace(strings.TrimPrefix(line, "- ")) == "" {
			return fmt.Errorf("each experience lesson must be one non-empty bullet line")
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
