// Package memory defines Eri's stable memory semantics. Storage and indexes
// are replaceable, but evidence provenance, conflict and deletion behavior are
// part of the Core contract.
package memory

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/secret"
)

type Relation string

const (
	Supports    Relation = "supports"
	Contradicts Relation = "contradicts"
	Qualifies   Relation = "qualifies"
)

type BeliefStatus string

const (
	Supported BeliefStatus = "supported"
	Tentative BeliefStatus = "tentative"
	Contested BeliefStatus = "contested"
	Unlikely  BeliefStatus = "unlikely"
	Unknown   BeliefStatus = "unknown"
)

type WeightedEvidence struct {
	Relation          Relation
	IndependenceGroup string
	Reliability       float64
	Directness        float64
	Verifiability     float64
}

type Assessment struct {
	Status            BeliefStatus `json:"status"`
	Confidence        float64      `json:"confidence"`
	SupportWeight     float64      `json:"support_weight"`
	ContradictWeight  float64      `json:"contradict_weight"`
	IndependentGroups int          `json:"independent_groups"`
	Expired           bool         `json:"expired"`
}

// Assess gives every independent source group at most one contribution per
// polarity. A repeated syndication therefore cannot outvote one strong primary
// fact. The result is intentionally inspectable rather than model-generated.
func Assess(evidence []WeightedEvidence) Assessment {
	type pair struct{ support, contradict float64 }
	groups := make(map[string]pair)
	for index, item := range evidence {
		group := strings.TrimSpace(item.IndependenceGroup)
		if group == "" {
			group = fmt.Sprintf("ungrouped:%d", index)
		}
		strength := clamp01(item.Reliability) * clamp01(item.Directness) * clamp01(item.Verifiability)
		current := groups[group]
		switch item.Relation {
		case Supports:
			if strength > current.support {
				current.support = strength
			}
		case Contradicts:
			if strength > current.contradict {
				current.contradict = strength
			}
		case Qualifies:
			// Qualifying evidence lowers certainty without pretending the claim
			// is false. It contributes a bounded half-strength contradiction.
			if strength*0.5 > current.contradict {
				current.contradict = strength * 0.5
			}
		}
		groups[group] = current
	}
	assessment := Assessment{IndependentGroups: len(groups)}
	for _, item := range groups {
		assessment.SupportWeight += item.support
		assessment.ContradictWeight += item.contradict
	}
	total := assessment.SupportWeight + assessment.ContradictWeight
	if total > 0 {
		assessment.Confidence = assessment.SupportWeight / total
	}
	support := assessment.SupportWeight
	contradict := assessment.ContradictWeight
	switch {
	case support >= 0.8 && support >= contradict*2:
		assessment.Status = Supported
	case contradict >= 0.8 && contradict >= support*2:
		assessment.Status = Unlikely
	case support >= 0.55 && contradict >= 0.55:
		assessment.Status = Contested
	case support >= 0.8 && support >= contradict*1.35:
		assessment.Status = Supported
	case contradict >= 0.8 && contradict >= support*1.35:
		assessment.Status = Unlikely
	case support > contradict && support >= 0.3:
		assessment.Status = Tentative
	default:
		assessment.Status = Unknown
	}
	return assessment
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

type CaptureRequest struct {
	ClaimID             string    `json:"claim_id,omitempty"`
	Statement           string    `json:"statement"`
	Evidence            string    `json:"evidence,omitempty"`
	Kind                string    `json:"kind"`
	Scope               string    `json:"scope,omitempty"`
	Relation            Relation  `json:"relation"`
	SourceType          string    `json:"source_type"`
	SourceRef           string    `json:"source_ref"`
	IndependenceGroup   string    `json:"independence_group"`
	Reliability         float64   `json:"reliability"`
	Directness          float64   `json:"directness"`
	Verifiability       float64   `json:"verifiability"`
	ObservedAt          time.Time `json:"observed_at,omitempty"`
	ValidUntil          time.Time `json:"valid_until,omitempty"`
	DirectUserStatement bool      `json:"direct_user_statement,omitempty"`
	ExplicitUserMemory  bool      `json:"explicit_user_memory,omitempty"`
}

type CaptureRecord struct {
	ClaimID           string
	ClaimKey          string
	StatementRef      content.Ref
	EvidenceID        string
	EvidenceKey       string
	EvidenceRef       content.Ref
	Kind              string
	Scope             string
	Relation          Relation
	SourceType        string
	SourceRef         string
	IndependenceGroup string
	Reliability       float64
	Directness        float64
	Verifiability     float64
	ObservedAt        time.Time
	ValidUntil        time.Time
	Activate          bool
	Pin               bool
	TermKeys          []string
}

type Snapshot struct {
	MemoryID          string       `json:"memory_id"`
	ClaimID           string       `json:"claim_id"`
	StatementRef      content.Ref  `json:"-"`
	Status            BeliefStatus `json:"status"`
	Confidence        float64      `json:"confidence"`
	SupportWeight     float64      `json:"support_weight"`
	ContradictWeight  float64      `json:"contradict_weight"`
	IndependentGroups int          `json:"independent_groups"`
	Kind              string       `json:"kind"`
	Scope             string       `json:"scope,omitempty"`
	UsagePolicy       string       `json:"usage_policy"`
	LifecycleStatus   string       `json:"lifecycle_status"`
	Salience          float64      `json:"salience"`
	AccessCount       int          `json:"access_count"`
	LastAccessedAt    time.Time    `json:"last_accessed_at,omitempty"`
	Pinned            bool         `json:"pinned"`
	Expired           bool         `json:"expired"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type Candidate struct {
	Snapshot
	EvidenceRefs []content.Ref
	Sources      []SourceSummary
}

type SourceSummary struct {
	EvidenceID        string    `json:"evidence_id"`
	Relation          Relation  `json:"relation"`
	SourceType        string    `json:"source_type"`
	SourceRef         string    `json:"source_ref"`
	IndependenceGroup string    `json:"independence_group"`
	ObservedAt        time.Time `json:"observed_at"`
	Reliability       float64   `json:"reliability"`
}

type Entry struct {
	Snapshot
	Statement     string          `json:"statement"`
	Sources       []SourceSummary `json:"sources"`
	SemanticScore float64         `json:"semantic_score,omitempty"`
	RecallScore   float64         `json:"recall_score,omitempty"`
	RecallReasons []string        `json:"recall_reasons,omitempty"`
}

type Bundle struct {
	RetrievalID  string   `json:"retrieval_id,omitempty"`
	RetrievedIDs []string `json:"retrieved_ids,omitempty"`
	Entries      []Entry  `json:"entries"`
}

type RecallRequest struct {
	Query        string
	Scope        string
	TaskID       string
	InvocationID string
	Limit        int
	At           time.Time
}

type RetrievalRecord struct {
	ID           string
	TaskID       string
	InvocationID string
	QueryKey     string
	CreatedAt    time.Time
	Items        []RetrievalItem
}

type RetrievalItem struct {
	MemoryID string
	Rank     int
	Score    float64
	Reasons  []string
	Injected bool
}

// SemanticEncoder is a replaceable local text-embedding boundary. Core never
// assumes that the configured chat provider also offers embeddings.
type SemanticEncoder interface {
	ID() string
	Embed(context.Context, []string) ([][]float32, error)
}

// SemanticRecord points to an encrypted vector in the governed Content Store.
// The operational database never contains plaintext memory or raw vectors.
type SemanticRecord struct {
	MemoryID    string
	ModelID     string
	Dimensions  int
	ContentHash string
	VectorRef   content.Ref
	UpdatedAt   time.Time
}

type SemanticCandidate struct {
	Memory Candidate
	Index  SemanticRecord
}

// SemanticIndex owns only replaceable candidate-generation metadata. Belief,
// lifecycle, usage policy and deletion remain governed by Repository.
type SemanticIndex interface {
	MissingSemanticMemory(context.Context, string, int) ([]Candidate, error)
	ReplaceSemanticMemory(context.Context, SemanticRecord) (content.Ref, bool, error)
	SemanticMemory(context.Context, string, int) ([]SemanticCandidate, error)
}

type Options struct {
	SemanticEncoder SemanticEncoder
	SemanticIndex   SemanticIndex
	Logger          *slog.Logger
}

type DeletePlan struct {
	MemoryID    string         `json:"memory_id"`
	ClaimID     string         `json:"claim_id"`
	ContentRefs []content.Ref  `json:"-"`
	Affected    map[string]int `json:"affected"`
}

type CaptureOutcome struct {
	Snapshot        Snapshot
	StatementStored bool
	EvidenceStored  bool
}

type ConsolidationReport struct {
	Promoted            int `json:"promoted"`
	Lowered             int `json:"lowered"`
	Archived            int `json:"archived"`
	Reweighted          int `json:"reweighted"`
	AssociationsDecayed int `json:"associations_decayed"`
	AssociationsPruned  int `json:"associations_pruned"`
}

type Repository interface {
	CaptureEvidence(context.Context, CaptureRecord) (CaptureOutcome, error)
	RetrieveMemory(context.Context, []string, int) ([]Candidate, error)
	InspectMemory(context.Context, int) ([]Candidate, error)
	InspectAllMemory(context.Context) ([]Candidate, error)
	RecordMemoryRetrieval(context.Context, RetrievalRecord) error
	RecordMemoryUse(context.Context, string, []string, time.Time) error
	PromoteMemory(context.Context, string, bool) error
	ConsolidateMemory(context.Context, time.Time, int) (ConsolidationReport, error)
	SetMemoryUsagePolicy(context.Context, string, string) error
	PlanDeleteMemory(context.Context, string) (DeletePlan, error)
	CommitDeleteMemory(context.Context, DeletePlan) error
	PendingMemoryDeletes(context.Context, int) ([]DeletePlan, error)
	CompleteDeleteMemory(context.Context, string) error
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

type Service struct {
	repository    Repository
	content       ContentStore
	indexKey      []byte
	semantic      SemanticEncoder
	semanticIndex SemanticIndex
	logger        *slog.Logger
}

func NewService(repository Repository, contentStore ContentStore, masterKey []byte, options ...Options) (*Service, error) {
	if repository == nil || contentStore == nil {
		return nil, fmt.Errorf("memory repository and content store are required")
	}
	if len(masterKey) < 32 {
		return nil, fmt.Errorf("memory index key requires at least 32 bytes")
	}
	digest := hmac.New(sha256.New, masterKey)
	digest.Write([]byte("eri/memory/index/v1"))
	configuration := Options{}
	if len(options) > 1 {
		return nil, fmt.Errorf("memory accepts at most one options value")
	}
	if len(options) == 1 {
		configuration = options[0]
	}
	if (configuration.SemanticEncoder == nil) != (configuration.SemanticIndex == nil) {
		return nil, fmt.Errorf("memory semantic encoder and index must be configured together")
	}
	if configuration.Logger == nil {
		configuration.Logger = slog.Default()
	}
	return &Service{
		repository: repository, content: contentStore, indexKey: digest.Sum(nil),
		semantic: configuration.SemanticEncoder, semanticIndex: configuration.SemanticIndex, logger: configuration.Logger,
	}, nil
}

func (s *Service) Capture(ctx context.Context, request CaptureRequest) (Entry, error) {
	request.Statement = strings.TrimSpace(request.Statement)
	if request.Statement == "" || len([]byte(request.Statement)) > 16*1024 {
		return Entry{}, fmt.Errorf("memory statement must be between 1 byte and 16 KiB")
	}
	if looksSecret(request.Statement) || looksSecret(request.Evidence) || looksSecret(request.SourceRef) {
		return Entry{}, fmt.Errorf("secret-like values cannot be stored in Eri memory")
	}
	if request.Relation != Supports && request.Relation != Contradicts && request.Relation != Qualifies {
		return Entry{}, fmt.Errorf("memory relation is invalid")
	}
	if request.SourceType == "" || request.SourceRef == "" || request.IndependenceGroup == "" {
		return Entry{}, fmt.Errorf("memory source type, source ref and independence group are required")
	}
	if request.Kind == "" {
		request.Kind = "semantic"
	}
	if request.ObservedAt.IsZero() {
		request.ObservedAt = time.Now().UTC()
	}
	if request.Evidence == "" {
		request.Evidence = request.Statement
	}
	if (request.DirectUserStatement || request.ExplicitUserMemory) && request.SourceType == "user" {
		request.Reliability = max(request.Reliability, 1)
		request.Directness = max(request.Directness, 1)
		request.Verifiability = max(request.Verifiability, 0.9)
	}
	claimID := request.ClaimID
	if claimID == "" {
		var err error
		claimID, err = identifier.New()
		if err != nil {
			return Entry{}, err
		}
	}
	evidenceID, err := identifier.New()
	if err != nil {
		return Entry{}, err
	}
	statementRef, err := s.content.Put(ctx, []byte(request.Statement), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "memory", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: claimID,
	})
	if err != nil {
		return Entry{}, err
	}
	evidenceRef, err := s.content.Put(ctx, []byte(request.Evidence), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "memory-evidence", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: evidenceID,
	})
	if err != nil {
		_ = s.content.Delete(context.Background(), statementRef)
		return Entry{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.content.Delete(context.Background(), statementRef)
			_ = s.content.Delete(context.Background(), evidenceRef)
		}
	}()
	record := CaptureRecord{
		ClaimID: claimID, ClaimKey: s.key("claim:" + normalize(request.Statement)), StatementRef: statementRef,
		EvidenceID: evidenceID, EvidenceKey: s.key("evidence:" + normalize(request.Statement) + "\x00" + request.SourceRef + "\x00" + request.IndependenceGroup + "\x00" + string(request.Relation) + "\x00" + normalize(request.Evidence)),
		EvidenceRef: evidenceRef, Kind: request.Kind, Scope: request.Scope, Relation: request.Relation,
		SourceType: request.SourceType, SourceRef: request.SourceRef, IndependenceGroup: request.IndependenceGroup,
		Reliability: clamp01(request.Reliability), Directness: clamp01(request.Directness), Verifiability: clamp01(request.Verifiability),
		ObservedAt: request.ObservedAt.UTC(), ValidUntil: request.ValidUntil.UTC(),
		Activate: request.DirectUserStatement || request.ExplicitUserMemory, Pin: request.ExplicitUserMemory,
		TermKeys: s.termKeys(request.Statement),
	}
	outcome, err := s.repository.CaptureEvidence(ctx, record)
	if err != nil {
		return Entry{}, err
	}
	if !outcome.StatementStored {
		_ = s.content.Delete(context.Background(), statementRef)
	}
	if !outcome.EvidenceStored {
		_ = s.content.Delete(context.Background(), evidenceRef)
	}
	committed = true
	entry := Entry{Snapshot: outcome.Snapshot, Statement: request.Statement}
	if s.semantic != nil && outcome.StatementStored {
		started := time.Now()
		if err := s.indexSemanticEntries(ctx, []Entry{entry}); err != nil {
			s.logger.Warn("memory semantic indexing unavailable", "component", "memory", "operation", "semantic_index_write", "memory_id", entry.MemoryID, "model", s.semantic.ID(), "duration_ms", time.Since(started).Milliseconds(), "error_code", "semantic_index_unavailable")
		} else {
			s.logger.Info("memory semantic index updated", "component", "memory", "operation", "semantic_index_write", "memory_id", entry.MemoryID, "model", s.semantic.ID(), "duration_ms", time.Since(started).Milliseconds())
		}
	}
	return entry, nil
}

func (s *Service) Recall(ctx context.Context, request RecallRequest) (_ Bundle, resultErr error) {
	started := time.Now()
	lexicalCount := 0
	semanticCount := 0
	retrievedCount := 0
	injectedCount := 0
	semanticState := "disabled"
	defer func() {
		attributes := []any{
			"component", "memory", "operation", "recall", "task_id", request.TaskID, "invocation_id", request.InvocationID,
			"lexical_candidates", lexicalCount, "semantic_candidates", semanticCount, "retrieved", retrievedCount,
			"injected", injectedCount, "semantic_status", semanticState, "duration_ms", time.Since(started).Milliseconds(),
		}
		if resultErr != nil {
			attributes = append(attributes, "status", "failed", "error_code", "memory_recall_failed")
			s.logger.Warn("memory recall failed", attributes...)
			return
		}
		attributes = append(attributes, "status", "succeeded")
		s.logger.Info("memory recall completed", attributes...)
	}()
	if request.At.IsZero() {
		request.At = time.Now().UTC()
	}
	request.Limit = clampLimit(request.Limit)
	if _, err := s.repository.ConsolidateMemory(ctx, time.Now().UTC(), 500); err != nil {
		return Bundle{}, err
	}
	candidateLimit := request.Limit * 4
	if candidateLimit > 100 {
		candidateLimit = 100
	}
	lexicalCandidates, err := s.repository.RetrieveMemory(ctx, s.termKeys(request.Query), candidateLimit)
	if err != nil {
		return Bundle{}, err
	}
	lexicalCount = len(lexicalCandidates)
	candidates := append([]Candidate(nil), lexicalCandidates...)
	semanticScores := map[string]float64{}
	if s.semantic != nil && strings.TrimSpace(request.Query) != "" {
		semanticState = "active"
		if err := s.backfillSemanticIndex(ctx, 64); err != nil {
			semanticState = "unavailable"
			s.logger.Warn("memory semantic backfill unavailable", "component", "memory", "operation", "semantic_index_backfill", "model", s.semantic.ID(), "error_code", "semantic_index_unavailable")
		} else if matches, err := s.semanticCandidates(ctx, request.Query, candidateLimit); err != nil {
			semanticState = "unavailable"
			s.logger.Warn("memory semantic retrieval unavailable", "component", "memory", "operation", "semantic_recall", "model", s.semantic.ID(), "error_code", "semantic_recall_unavailable")
		} else {
			seen := make(map[string]struct{}, len(candidates))
			for _, candidate := range candidates {
				seen[candidate.MemoryID] = struct{}{}
			}
			for _, match := range matches {
				semanticScores[match.MemoryID] = match.Score
				if _, exists := seen[match.MemoryID]; !exists {
					candidates = append(candidates, match.Candidate)
					seen[match.MemoryID] = struct{}{}
				}
			}
		}
	}
	semanticCount = len(semanticScores)
	bundle, err := s.resolve(ctx, candidates)
	if err != nil {
		return Bundle{}, err
	}
	for index := range bundle.Entries {
		bundle.Entries[index].SemanticScore = semanticScores[bundle.Entries[index].MemoryID]
	}
	ranked := rerank(request, bundle.Entries)
	retrievedCount = len(ranked)
	bundle.RetrievedIDs = make([]string, 0, len(ranked))
	for _, entry := range ranked {
		bundle.RetrievedIDs = append(bundle.RetrievedIDs, entry.MemoryID)
	}
	injectedCount = min(request.Limit, len(ranked))
	bundle.Entries = append([]Entry(nil), ranked[:injectedCount]...)
	if request.TaskID != "" && len(ranked) > 0 {
		retrievalID, err := identifier.New()
		if err != nil {
			return Bundle{}, err
		}
		bundle.RetrievalID = retrievalID
		items := make([]RetrievalItem, 0, len(ranked))
		for rank, entry := range ranked {
			items = append(items, RetrievalItem{MemoryID: entry.MemoryID, Rank: rank + 1, Score: entry.RecallScore, Reasons: entry.RecallReasons, Injected: rank < injectedCount})
		}
		if err := s.repository.RecordMemoryRetrieval(ctx, RetrievalRecord{
			ID: retrievalID, TaskID: request.TaskID, InvocationID: request.InvocationID,
			QueryKey: s.key("query:" + normalize(request.Query)), CreatedAt: request.At.UTC(), Items: items,
		}); err != nil {
			return Bundle{}, err
		}
	}
	return bundle, nil
}

func (s *Service) Retrieve(ctx context.Context, query string, limit int) (Bundle, error) {
	return s.Recall(ctx, RecallRequest{Query: query, Limit: limit})
}

func (s *Service) MarkUsed(ctx context.Context, retrievalID string, memoryIDs []string) error {
	if strings.TrimSpace(retrievalID) == "" || len(memoryIDs) == 0 {
		return fmt.Errorf("retrieval id and at least one memory id are required")
	}
	started := time.Now()
	if err := s.repository.RecordMemoryUse(ctx, retrievalID, memoryIDs, time.Now().UTC()); err != nil {
		s.logger.Warn("memory application recording failed", "component", "memory", "operation", "mark_used", "retrieval_id", retrievalID, "memory_count", len(memoryIDs), "duration_ms", time.Since(started).Milliseconds(), "error_code", "memory_application_failed")
		return err
	}
	s.logger.Info("memory application recorded", "component", "memory", "operation", "mark_used", "retrieval_id", retrievalID, "memory_count", len(memoryIDs), "duration_ms", time.Since(started).Milliseconds())
	return nil
}

func (s *Service) Inspect(ctx context.Context, limit int) (Bundle, error) {
	if _, err := s.repository.ConsolidateMemory(ctx, time.Now().UTC(), 500); err != nil {
		return Bundle{}, err
	}
	candidates, err := s.repository.InspectMemory(ctx, clampLimit(limit))
	if err != nil {
		return Bundle{}, err
	}
	return s.resolve(ctx, candidates)
}

func (s *Service) Promote(ctx context.Context, memoryID string) error {
	if strings.TrimSpace(memoryID) == "" {
		return fmt.Errorf("memory id is required")
	}
	return s.repository.PromoteMemory(ctx, memoryID, true)
}

func (s *Service) Consolidate(ctx context.Context) (ConsolidationReport, error) {
	return s.repository.ConsolidateMemory(ctx, time.Now().UTC(), 500)
}

func (s *Service) SetUsagePolicy(ctx context.Context, memoryID, policy string) error {
	if policy != "allow" && policy != "do_not_use" {
		return fmt.Errorf("usage policy must be allow or do_not_use")
	}
	return s.repository.SetMemoryUsagePolicy(ctx, memoryID, policy)
}

func (s *Service) Delete(ctx context.Context, memoryID string) (DeletePlan, error) {
	plan, err := s.repository.PlanDeleteMemory(ctx, memoryID)
	if err != nil {
		return DeletePlan{}, err
	}
	if err := s.repository.CommitDeleteMemory(ctx, plan); err != nil {
		return DeletePlan{}, err
	}
	for _, ref := range plan.ContentRefs {
		if err := s.content.Delete(ctx, ref); err != nil {
			return DeletePlan{}, fmt.Errorf("delete memory content: %w", err)
		}
	}
	if err := s.repository.CompleteDeleteMemory(ctx, memoryID); err != nil {
		return DeletePlan{}, err
	}
	return plan, nil
}

func (s *Service) RecoverDeletes(ctx context.Context) error {
	plans, err := s.repository.PendingMemoryDeletes(ctx, 100)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		for _, ref := range plan.ContentRefs {
			if err := s.content.Delete(ctx, ref); err != nil {
				return fmt.Errorf("recover memory deletion %s: %w", plan.MemoryID, err)
			}
		}
		if err := s.repository.CompleteDeleteMemory(ctx, plan.MemoryID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Export(ctx context.Context) ([]byte, error) {
	candidates, err := s.repository.InspectAllMemory(ctx)
	if err != nil {
		return nil, err
	}
	bundle, err := s.resolve(ctx, candidates)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(map[string]any{"version": 1, "exported_at": time.Now().UTC(), "memories": bundle.Entries}, "", "  ")
}

func (s *Service) resolve(ctx context.Context, candidates []Candidate) (Bundle, error) {
	bundle := Bundle{Entries: make([]Entry, 0, len(candidates))}
	for _, candidate := range candidates {
		body, err := s.content.Get(ctx, candidate.StatementRef)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Entries = append(bundle.Entries, Entry{Snapshot: candidate.Snapshot, Statement: string(body), Sources: candidate.Sources})
	}
	return bundle, nil
}

type semanticMatch struct {
	Candidate Candidate
	MemoryID  string
	Score     float64
}

func (s *Service) backfillSemanticIndex(ctx context.Context, limit int) error {
	missing, err := s.semanticIndex.MissingSemanticMemory(ctx, s.semantic.ID(), limit)
	if err != nil || len(missing) == 0 {
		return err
	}
	bundle, err := s.resolve(ctx, missing)
	if err != nil {
		return err
	}
	return s.indexSemanticEntries(ctx, bundle.Entries)
}

func (s *Service) indexSemanticEntries(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	texts := make([]string, 0, len(entries))
	for _, entry := range entries {
		texts = append(texts, entry.Statement)
	}
	vectors, err := s.semantic.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(entries) {
		return fmt.Errorf("semantic encoder returned %d vectors for %d inputs", len(vectors), len(entries))
	}
	for index, vector := range vectors {
		body, err := encodeVector(vector)
		if err != nil {
			return err
		}
		entry := entries[index]
		ref, err := s.content.Put(ctx, body, content.Metadata{
			MediaType: "application/vnd.eri.memory-vector", EncryptionDomain: "memory-index",
			PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: entry.MemoryID,
		})
		if err != nil {
			return err
		}
		record := SemanticRecord{
			MemoryID: entry.MemoryID, ModelID: s.semantic.ID(), Dimensions: len(vector),
			ContentHash: entry.StatementRef.ContentHash, VectorRef: ref, UpdatedAt: time.Now().UTC(),
		}
		if record.ContentHash == "" {
			digest := sha256.Sum256([]byte(entry.Statement))
			record.ContentHash = hex.EncodeToString(digest[:])
		}
		oldRef, replaced, err := s.semanticIndex.ReplaceSemanticMemory(ctx, record)
		if err != nil {
			_ = s.content.Delete(context.Background(), ref)
			return err
		}
		if replaced && oldRef.ObjectID != "" && oldRef.ObjectID != ref.ObjectID {
			_ = s.content.Delete(context.Background(), oldRef)
		}
	}
	return nil
}

func (s *Service) semanticCandidates(ctx context.Context, query string, limit int) ([]semanticMatch, error) {
	vectors, err := s.semantic.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return nil, fmt.Errorf("semantic encoder returned an invalid query vector")
	}
	indexed, err := s.semanticIndex.SemanticMemory(ctx, s.semantic.ID(), 5000)
	if err != nil {
		return nil, err
	}
	matches := make([]semanticMatch, 0, len(indexed))
	for _, candidate := range indexed {
		body, err := s.content.Get(ctx, candidate.Index.VectorRef)
		if err != nil {
			return nil, err
		}
		vector, err := decodeVector(body, candidate.Index.Dimensions)
		if err != nil {
			return nil, err
		}
		score, err := cosine(vectors[0], vector)
		if err != nil {
			return nil, err
		}
		if score <= 0 {
			continue
		}
		matches = append(matches, semanticMatch{Candidate: candidate.Memory, MemoryID: candidate.Memory.MemoryID, Score: score})
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].Score != matches[right].Score {
			return matches[left].Score > matches[right].Score
		}
		return matches[left].MemoryID < matches[right].MemoryID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func encodeVector(vector []float32) ([]byte, error) {
	if len(vector) == 0 || len(vector) > 16384 {
		return nil, fmt.Errorf("semantic vector dimensions must be between 1 and 16384")
	}
	body := make([]byte, len(vector)*4)
	for index, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("semantic vector contains a non-finite value")
		}
		binary.LittleEndian.PutUint32(body[index*4:], math.Float32bits(value))
	}
	return body, nil
}

func decodeVector(body []byte, dimensions int) ([]float32, error) {
	if dimensions <= 0 || dimensions > 16384 || len(body) != dimensions*4 {
		return nil, fmt.Errorf("semantic vector metadata does not match its encrypted body")
	}
	vector := make([]float32, dimensions)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(body[index*4:]))
		if math.IsNaN(float64(vector[index])) || math.IsInf(float64(vector[index]), 0) {
			return nil, fmt.Errorf("semantic vector contains a non-finite value")
		}
	}
	return vector, nil
}

func cosine(left, right []float32) (float64, error) {
	if len(left) == 0 || len(left) != len(right) {
		return 0, fmt.Errorf("semantic vectors have incompatible dimensions")
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l, r := float64(left[index]), float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, fmt.Errorf("semantic vector has zero magnitude")
	}
	return clamp01(dot / math.Sqrt(leftNorm*rightNorm)), nil
}

func rerank(request RecallRequest, entries []Entry) []Entry {
	queryTerms := tokenSet(request.Query)
	scopeTerms := tokenSet(request.Scope)
	for index := range entries {
		entry := &entries[index]
		statementTerms := tokenSet(entry.Statement)
		lexical := overlapRatio(queryTerms, statementTerms)
		scope := overlapRatio(scopeTerms, tokenSet(entry.Scope))
		belief := entry.Confidence
		switch entry.Status {
		case Supported:
			belief = maxFloat64(belief, .75)
		case Contested:
			belief *= .45
		case Unlikely, Unknown:
			belief *= .2
		}
		freshness := .5
		if !entry.UpdatedAt.IsZero() {
			days := request.At.Sub(entry.UpdatedAt).Hours() / 24
			if days < 0 {
				days = 0
			}
			freshness = 1 / (1 + days/180)
		}
		relevance := maxFloat64(lexical, entry.SemanticScore)
		entry.RecallScore = clamp01(.52*relevance + .18*belief + .15*entry.Salience + .10*scope + .05*freshness)
		entry.RecallReasons = entry.RecallReasons[:0]
		if lexical > 0 {
			entry.RecallReasons = append(entry.RecallReasons, "query_match")
		}
		if entry.SemanticScore > 0 {
			entry.RecallReasons = append(entry.RecallReasons, "semantic_match")
		}
		if lexical == 0 && entry.SemanticScore == 0 {
			entry.RecallReasons = append(entry.RecallReasons, "associated_context")
		}
		if scope > 0 {
			entry.RecallReasons = append(entry.RecallReasons, "scope_match")
		}
		if entry.Status == Supported && entry.Confidence >= .75 {
			entry.RecallReasons = append(entry.RecallReasons, "supported_belief")
		}
		if entry.Pinned {
			entry.RecallScore = clamp01(entry.RecallScore + .05)
			entry.RecallReasons = append(entry.RecallReasons, "user_pinned")
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].RecallScore != entries[right].RecallScore {
			return entries[left].RecallScore > entries[right].RecallScore
		}
		return entries[left].UpdatedAt.After(entries[right].UpdatedAt)
	})
	return entries
}

func tokenSet(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, term := range tokenize(value) {
		result[term] = struct{}{}
	}
	return result
}

func overlapRatio(needles, haystack map[string]struct{}) float64 {
	if len(needles) == 0 {
		return 0
	}
	matches := 0
	for term := range needles {
		if _, ok := haystack[term]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(needles))
}

func maxFloat64(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func (s *Service) termKeys(value string) []string {
	terms := tokenize(value)
	keys := make([]string, 0, len(terms))
	for _, term := range terms {
		keys = append(keys, s.key("term:"+term))
	}
	sort.Strings(keys)
	return compact(keys)
}

func (s *Service) key(value string) string {
	digest := hmac.New(sha256.New, s.indexKey)
	digest.Write([]byte(value))
	return hex.EncodeToString(digest.Sum(nil))
}

func normalize(value string) string { return strings.ToLower(strings.Join(strings.Fields(value), " ")) }

func looksSecret(value string) bool {
	if secret.LooksLikeCredential([]byte(value)) {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"\u5bc6\u7801\u662f", "\u5bc6\u7801\uff1a"} {
		if index := strings.Index(lower, marker); index >= 0 {
			candidate := strings.Trim(strings.TrimSpace(value[index+len(marker):]), "\"'`,;:：。()[]{}")
			if len([]byte(candidate)) >= 8 {
				return true
			}
		}
	}
	for _, marker := range []string{"-----begin private key-----", "-----begin rsa private key-----"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func tokenize(value string) []string {
	value = strings.ToLower(value)
	terms := make([]string, 0)
	var run []rune
	flush := func() {
		if len(run) > 0 {
			terms = append(terms, string(run))
			run = run[:0]
		}
	}
	var han []rune
	flushHan := func() {
		for _, r := range han {
			terms = append(terms, string(r))
		}
		for index := 0; index+1 < len(han); index++ {
			terms = append(terms, string(han[index:index+2]))
		}
		han = han[:0]
	}
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			flush()
			han = append(han, r)
			continue
		}
		flushHan()
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			run = append(run, r)
		} else {
			flush()
		}
	}
	flush()
	flushHan()
	return compact(terms)
}

func compact(values []string) []string {
	seen := make(map[string]struct{}, len(values))
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

func clampLimit(limit int) int {
	if limit <= 0 {
		return 12
	}
	if limit > 500 {
		return 500
	}
	return limit
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
