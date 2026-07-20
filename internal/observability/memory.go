package observability

import (
	"context"
	"sort"
	"time"

	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/memory"
)

type MemoryStage string

const (
	MemoryStored    MemoryStage = "stored"
	MemoryRetrieved MemoryStage = "retrieved"
	MemoryInjected  MemoryStage = "injected"
	MemoryApplied   MemoryStage = "applied"
	MemoryExternal  MemoryStage = "sent_to_external_model"
)

// MemoryRetrievalRecord preserves the distinction between durable storage,
// retrieval and prompt injection. A non-zero store never implies run usage.
type MemoryRetrievalRecord struct {
	Checked        bool                `json:"checked"`
	RetrievedCount int                 `json:"retrieved_count"`
	InjectedCount  int                 `json:"injected_count"`
	AppliedCount   int                 `json:"applied_count"`
	ExternalSent   bool                `json:"external_sent"`
	Items          []MemoryObservation `json:"items"`
}

type MemoryObservation struct {
	MemoryID          string                    `json:"memory_id"`
	ClaimID           string                    `json:"claim_id"`
	Statement         string                    `json:"statement,omitempty"`
	Kind              string                    `json:"kind"`
	Scope             string                    `json:"scope,omitempty"`
	BeliefStatus      memory.BeliefStatus       `json:"belief_status"`
	Confidence        float64                   `json:"confidence"`
	SupportWeight     float64                   `json:"support_weight"`
	ContradictWeight  float64                   `json:"contradict_weight"`
	IndependentGroups int                       `json:"independent_groups"`
	UsagePolicy       string                    `json:"usage_policy"`
	LifecycleStatus   string                    `json:"lifecycle_status"`
	Salience          float64                   `json:"salience"`
	AccessCount       int                       `json:"access_count"`
	LastAccessedAt    time.Time                 `json:"last_accessed_at,omitempty"`
	UpdatedAt         time.Time                 `json:"updated_at"`
	Pinned            bool                      `json:"pinned"`
	Expired           bool                      `json:"expired"`
	Stages            []MemoryStage             `json:"stages"`
	Sources           []MemoryObservationSource `json:"sources"`
}

type MemoryObservationSource struct {
	EvidenceID        string          `json:"evidence_id"`
	Relation          memory.Relation `json:"relation"`
	SourceType        string          `json:"source_type"`
	SourceRef         string          `json:"source_ref,omitempty"`
	IndependenceGroup string          `json:"independence_group"`
	ObservedAt        time.Time       `json:"observed_at"`
	Reliability       float64         `json:"reliability"`
}

type MemoryOverview struct {
	Total        int                 `json:"total"`
	Active       int                 `json:"active"`
	Contested    int                 `json:"contested"`
	Expired      int                 `json:"expired"`
	DoNotUse     int                 `json:"do_not_use"`
	Observations []MemoryObservation `json:"observations"`
}

type memoryExposure int

const (
	memoryExposureConversation memoryExposure = iota
	memoryExposureDeveloper
)

func (s *Service) MemoryOverview(ctx context.Context, limit int) (MemoryOverview, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	candidates, err := s.inspectMemory(ctx, limit)
	if err != nil {
		return MemoryOverview{}, err
	}
	result := MemoryOverview{Total: len(candidates), Observations: make([]MemoryObservation, 0, len(candidates))}
	for _, candidate := range candidates {
		observation, err := s.projectMemory(ctx, candidate, []MemoryStage{MemoryStored}, memoryExposureDeveloper)
		if err != nil {
			return MemoryOverview{}, err
		}
		result.Observations = append(result.Observations, observation)
		if candidate.LifecycleStatus == "active" || candidate.LifecycleStatus == "low_salience" {
			result.Active++
		}
		if candidate.Status == memory.Contested {
			result.Contested++
		}
		if candidate.Expired {
			result.Expired++
		}
		if candidate.UsagePolicy == "do_not_use" {
			result.DoNotUse++
		}
	}
	return result, nil
}

func (s *Service) memoryRetrieval(ctx context.Context, manifest execution.ContextManifest, exposure memoryExposure) (MemoryRetrievalRecord, error) {
	ids := uniqueStrings(manifest.RetrievedMemoryIDs)
	injectedIDs := stringSet(manifest.MemoryIDs)
	appliedIDs := stringSet(manifest.AppliedMemoryIDs)
	externalIDs := make(map[string]struct{})
	for _, id := range manifest.ExternalMemoryIDs {
		externalIDs[id] = struct{}{}
	}
	record := MemoryRetrievalRecord{
		Checked: manifest.MemoryChecked, RetrievedCount: len(ids), InjectedCount: len(injectedIDs), AppliedCount: len(appliedIDs), ExternalSent: len(externalIDs) > 0,
		Items: []MemoryObservation{},
	}
	if len(ids) == 0 {
		return record, nil
	}
	candidates, err := s.inspectMemory(ctx, 500)
	if err != nil {
		return MemoryRetrievalRecord{}, err
	}
	byID := make(map[string]memory.Candidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.MemoryID] = candidate
	}
	for _, id := range ids {
		stages := []MemoryStage{MemoryStored, MemoryRetrieved}
		if _, ok := injectedIDs[id]; ok {
			stages = append(stages, MemoryInjected)
		}
		if _, ok := appliedIDs[id]; ok {
			stages = append(stages, MemoryApplied)
		}
		if _, ok := externalIDs[id]; ok {
			stages = append(stages, MemoryExternal)
		}
		candidate, ok := byID[id]
		if !ok {
			record.Items = append(record.Items, MemoryObservation{MemoryID: id, Stages: stages})
			continue
		}
		observation, err := s.projectMemory(ctx, candidate, stages, exposure)
		if err != nil {
			return MemoryRetrievalRecord{}, err
		}
		record.Items = append(record.Items, observation)
	}
	return record, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func (s *Service) inspectMemory(ctx context.Context, limit int) ([]memory.Candidate, error) {
	return s.repository.InspectMemory(ctx, limit)
}

func (s *Service) projectMemory(ctx context.Context, candidate memory.Candidate, stages []MemoryStage, exposure memoryExposure) (MemoryObservation, error) {
	observation := MemoryObservation{
		MemoryID: candidate.MemoryID, ClaimID: candidate.ClaimID, Kind: candidate.Kind, Scope: candidate.Scope,
		BeliefStatus: candidate.Status, Confidence: candidate.Confidence, SupportWeight: candidate.SupportWeight,
		ContradictWeight: candidate.ContradictWeight, IndependentGroups: candidate.IndependentGroups,
		UsagePolicy: candidate.UsagePolicy, LifecycleStatus: candidate.LifecycleStatus, Salience: candidate.Salience,
		AccessCount: candidate.AccessCount, LastAccessedAt: candidate.LastAccessedAt, UpdatedAt: candidate.UpdatedAt,
		Pinned: candidate.Pinned, Expired: candidate.Expired, Stages: append([]MemoryStage(nil), stages...),
		Sources: make([]MemoryObservationSource, 0, len(candidate.Sources)),
	}
	if s.content != nil && candidate.StatementRef.ObjectID != "" {
		body, err := s.content.Get(ctx, candidate.StatementRef)
		if err != nil {
			return MemoryObservation{}, err
		}
		observation.Statement = string(body)
	}
	for _, source := range candidate.Sources {
		item := MemoryObservationSource{
			EvidenceID: source.EvidenceID, Relation: source.Relation, SourceType: source.SourceType,
			IndependenceGroup: source.IndependenceGroup, ObservedAt: source.ObservedAt, Reliability: source.Reliability,
		}
		if exposure == memoryExposureDeveloper {
			item.SourceRef = source.SourceRef
		}
		observation.Sources = append(observation.Sources, item)
	}
	sort.SliceStable(observation.Sources, func(i, j int) bool { return observation.Sources[i].ObservedAt.After(observation.Sources[j].ObservedAt) })
	return observation, nil
}
