// Package episode builds governed, replay-safe task manifests from operational
// facts. It never re-invokes side effects.
package episode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/runtime"
)

type Manifest struct {
	TaskID       string           `json:"task_id"`
	Status       string           `json:"status"`
	Runs         []map[string]any `json:"runs"`
	Invocations  []map[string]any `json:"invocations"`
	Artifacts    []map[string]any `json:"artifacts"`
	Effects      []map[string]any `json:"effects"`
	Events       []map[string]any `json:"events"`
	Privacy      map[string]any   `json:"privacy_manifest"`
	ReplayPolicy map[string]any   `json:"replay_policy"`
	BuiltAt      time.Time        `json:"built_at"`
}

type Record struct {
	ID          string      `json:"id"`
	TaskID      string      `json:"task_id"`
	ManifestRef content.Ref `json:"-"`
	Status      string      `json:"status"`
	CreatedAt   time.Time   `json:"created_at"`
}

type DatasetCandidate struct {
	ID        string    `json:"id"`
	EpisodeID string    `json:"episode_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type DatasetSource struct {
	CandidateID string      `json:"candidate_id"`
	EpisodeID   string      `json:"episode_id"`
	TaskID      string      `json:"task_id"`
	ManifestRef content.Ref `json:"episode_manifest_ref"`
}

type DatasetItem struct {
	DatasetSource
	Split string `json:"split"`
}

type DatasetSnapshot struct {
	ID          string      `json:"id"`
	Version     int         `json:"version"`
	Purpose     string      `json:"purpose"`
	Status      string      `json:"status"`
	ManifestRef content.Ref `json:"-"`
	ItemCount   int         `json:"item_count"`
	CreatedAt   time.Time   `json:"created_at"`
}

type DatasetSnapshotManifest struct {
	SchemaVersion int            `json:"schema_version"`
	Purpose       string         `json:"purpose"`
	SplitSeed     string         `json:"split_seed"`
	Items         []DatasetItem  `json:"items"`
	Privacy       map[string]any `json:"privacy_manifest"`
	FrozenAt      time.Time      `json:"frozen_at"`
}

type Repository interface {
	BuildEpisodeManifest(context.Context, string) (Manifest, error)
	LoadEpisodeForTask(context.Context, string) (Record, bool, error)
	SaveEpisode(context.Context, Record) (Record, error)
	ListEpisodes(context.Context, int) ([]Record, error)
	LoadEpisode(context.Context, string) (Record, bool, error)
	PromoteEpisodeCandidate(context.Context, string) (DatasetCandidate, error)
	ResolveDatasetCandidates(context.Context, []string) ([]DatasetSource, error)
	SaveDatasetSnapshot(context.Context, DatasetSnapshot, []DatasetItem) (DatasetSnapshot, error)
	ListDatasetSnapshots(context.Context, int) ([]DatasetSnapshot, error)
	LoadDatasetSnapshot(context.Context, string) (DatasetSnapshot, bool, error)
	RecordEpisodeExport(context.Context, string, time.Time) error
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
}

type Service struct {
	repository Repository
	content    ContentStore
}

func NewService(repository Repository, contentStore ContentStore) *Service {
	return &Service{repository: repository, content: contentStore}
}

func (s *Service) HandleBuild(ctx context.Context, item runtime.OutboxItem) error {
	_, err := s.Build(ctx, item.AggregateID)
	return err
}

func (s *Service) Build(ctx context.Context, taskID string) (Record, error) {
	if existing, found, err := s.repository.LoadEpisodeForTask(ctx, taskID); err != nil || found {
		return existing, err
	}
	manifest, err := s.repository.BuildEpisodeManifest(ctx, taskID)
	if err != nil {
		return Record{}, err
	}
	manifest.BuiltAt = time.Now().UTC()
	body, err := json.Marshal(manifest)
	if err != nil {
		return Record{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return Record{}, err
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "episode", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: taskID,
	})
	if err != nil {
		return Record{}, err
	}
	record, err := s.repository.SaveEpisode(ctx, Record{ID: id, TaskID: taskID, ManifestRef: ref, Status: "ready", CreatedAt: manifest.BuiltAt})
	if err != nil {
		return Record{}, fmt.Errorf("save episode: %w", err)
	}
	return record, nil
}

func (s *Service) List(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repository.ListEpisodes(ctx, limit)
}

func (s *Service) Inspect(ctx context.Context, id string) (Manifest, bool, error) {
	return s.loadManifest(ctx, id)
}

func (s *Service) Export(ctx context.Context, id string) (Manifest, bool, error) {
	manifest, found, err := s.loadManifest(ctx, id)
	if err != nil || !found {
		return manifest, found, err
	}
	if err := s.repository.RecordEpisodeExport(ctx, id, time.Now().UTC()); err != nil {
		return Manifest{}, false, err
	}
	return manifest, true, nil
}

func (s *Service) loadManifest(ctx context.Context, id string) (Manifest, bool, error) {
	record, found, err := s.repository.LoadEpisode(ctx, id)
	if err != nil || !found {
		return Manifest{}, found, err
	}
	body, err := s.content.Get(ctx, record.ManifestRef)
	if err != nil {
		return Manifest{}, false, err
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, false, err
	}
	return manifest, true, nil
}

func (s *Service) Promote(ctx context.Context, episodeID string) (DatasetCandidate, error) {
	return s.repository.PromoteEpisodeCandidate(ctx, episodeID)
}

func (s *Service) FreezeDataset(ctx context.Context, purpose, splitSeed string, candidateIDs []string) (DatasetSnapshot, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" || len([]byte(purpose)) > 128 {
		return DatasetSnapshot{}, fmt.Errorf("dataset purpose is required and must be at most 128 bytes")
	}
	if len(candidateIDs) == 0 || len(candidateIDs) > 10_000 {
		return DatasetSnapshot{}, fmt.Errorf("dataset requires between 1 and 10000 candidates")
	}
	if splitSeed = strings.TrimSpace(splitSeed); splitSeed == "" {
		splitSeed = "eri-v1"
	}
	unique := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return DatasetSnapshot{}, fmt.Errorf("candidate id is required")
		}
		unique[id] = struct{}{}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sources, err := s.repository.ResolveDatasetCandidates(ctx, ids)
	if err != nil {
		return DatasetSnapshot{}, err
	}
	if len(sources) != len(ids) {
		return DatasetSnapshot{}, fmt.Errorf("one or more dataset candidates are unavailable or invalidated")
	}
	items := make([]DatasetItem, 0, len(sources))
	seenTasks := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if _, duplicate := seenTasks[source.TaskID]; duplicate {
			return DatasetSnapshot{}, fmt.Errorf("near-duplicate source task %s appears more than once", source.TaskID)
		}
		seenTasks[source.TaskID] = struct{}{}
		items = append(items, DatasetItem{DatasetSource: source, Split: datasetSplit(splitSeed, source.TaskID)})
	}
	now := time.Now().UTC()
	manifest := DatasetSnapshotManifest{
		SchemaVersion: 1, Purpose: purpose, SplitSeed: splitSeed, Items: items, FrozenAt: now,
		Privacy: map[string]any{
			"contains_message_bodies": false, "contains_credentials": false,
			"immutable": true, "source_deletion_invalidates_snapshot": true,
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return DatasetSnapshot{}, err
	}
	id, err := identifier.New()
	if err != nil {
		return DatasetSnapshot{}, err
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "dataset_snapshot", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: id,
	})
	if err != nil {
		return DatasetSnapshot{}, err
	}
	return s.repository.SaveDatasetSnapshot(ctx, DatasetSnapshot{
		ID: id, Purpose: purpose, Status: "frozen", ManifestRef: ref, ItemCount: len(items), CreatedAt: now,
	}, items)
}

func (s *Service) DatasetSnapshots(ctx context.Context, limit int) ([]DatasetSnapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.repository.ListDatasetSnapshots(ctx, limit)
}

func (s *Service) InspectDatasetSnapshot(ctx context.Context, id string) (DatasetSnapshotManifest, bool, error) {
	snapshot, found, err := s.repository.LoadDatasetSnapshot(ctx, id)
	if err != nil || !found {
		return DatasetSnapshotManifest{}, found, err
	}
	body, err := s.content.Get(ctx, snapshot.ManifestRef)
	if err != nil {
		return DatasetSnapshotManifest{}, false, err
	}
	var manifest DatasetSnapshotManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return DatasetSnapshotManifest{}, false, err
	}
	return manifest, true, nil
}

func datasetSplit(seed, taskID string) string {
	digest := sha256.Sum256([]byte(seed + "\x00" + taskID))
	switch digest[0] % 10 {
	case 0:
		return "holdout"
	case 1, 2:
		return "eval"
	default:
		return "development"
	}
}
