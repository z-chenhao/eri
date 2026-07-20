package episode

import (
	"context"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
)

type episodeRepository struct {
	sources    []DatasetSource
	savedItems []DatasetItem
}

func (*episodeRepository) BuildEpisodeManifest(context.Context, string) (Manifest, error) {
	return Manifest{}, nil
}
func (*episodeRepository) LoadEpisodeForTask(context.Context, string) (Record, bool, error) {
	return Record{}, false, nil
}
func (*episodeRepository) SaveEpisode(_ context.Context, record Record) (Record, error) {
	return record, nil
}
func (*episodeRepository) ListEpisodes(context.Context, int) ([]Record, error) { return nil, nil }
func (*episodeRepository) LoadEpisode(context.Context, string) (Record, bool, error) {
	return Record{}, false, nil
}
func (*episodeRepository) PromoteEpisodeCandidate(context.Context, string) (DatasetCandidate, error) {
	return DatasetCandidate{}, nil
}
func (r *episodeRepository) ResolveDatasetCandidates(context.Context, []string) ([]DatasetSource, error) {
	return append([]DatasetSource(nil), r.sources...), nil
}
func (r *episodeRepository) SaveDatasetSnapshot(_ context.Context, snapshot DatasetSnapshot, items []DatasetItem) (DatasetSnapshot, error) {
	r.savedItems = append([]DatasetItem(nil), items...)
	snapshot.Version = 1
	return snapshot, nil
}
func (*episodeRepository) ListDatasetSnapshots(context.Context, int) ([]DatasetSnapshot, error) {
	return nil, nil
}
func (*episodeRepository) LoadDatasetSnapshot(context.Context, string) (DatasetSnapshot, bool, error) {
	return DatasetSnapshot{}, false, nil
}
func (*episodeRepository) RecordEpisodeExport(context.Context, string, time.Time) error { return nil }

type episodeContent struct{ body []byte }

func (c *episodeContent) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	c.body = append([]byte(nil), body...)
	return content.Ref{ObjectID: "snapshot-manifest", EncryptionDomain: metadata.EncryptionDomain, PrivacyClass: metadata.PrivacyClass, RetentionPolicy: metadata.RetentionPolicy, ProvenanceRef: metadata.ProvenanceRef}, nil
}
func (*episodeContent) Get(context.Context, content.Ref) ([]byte, error) { return nil, nil }

func TestFreezeDatasetDeduplicatesCandidatesAndCreatesStableSplits(t *testing.T) {
	repository := &episodeRepository{sources: []DatasetSource{
		{CandidateID: "candidate-a", EpisodeID: "episode-a", TaskID: "task-a"},
		{CandidateID: "candidate-b", EpisodeID: "episode-b", TaskID: "task-b"},
	}}
	contents := &episodeContent{}
	snapshot, err := NewService(repository, contents).FreezeDataset(context.Background(), " regression ", " stable-seed ", []string{"candidate-b", "candidate-a", "candidate-a"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || snapshot.ItemCount != 2 || snapshot.Purpose != "regression" || snapshot.ManifestRef.ObjectID != "snapshot-manifest" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(repository.savedItems) != 2 || len(contents.body) == 0 {
		t.Fatalf("items=%+v manifest_bytes=%d", repository.savedItems, len(contents.body))
	}
	for _, item := range repository.savedItems {
		if item.Split != datasetSplit("stable-seed", item.TaskID) {
			t.Fatalf("unstable split for %+v", item)
		}
		switch item.Split {
		case "development", "eval", "holdout":
		default:
			t.Fatalf("unknown split %q", item.Split)
		}
	}
}

func TestFreezeDatasetRejectsInvalidOrLeakingCandidateSets(t *testing.T) {
	service := NewService(&episodeRepository{}, &episodeContent{})
	if _, err := service.FreezeDataset(context.Background(), "", "seed", []string{"candidate"}); err == nil {
		t.Fatal("empty purpose was accepted")
	}
	if _, err := service.FreezeDataset(context.Background(), "purpose", "seed", nil); err == nil {
		t.Fatal("empty candidate set was accepted")
	}

	repository := &episodeRepository{sources: []DatasetSource{
		{CandidateID: "candidate-a", EpisodeID: "episode-a", TaskID: "same-task"},
		{CandidateID: "candidate-b", EpisodeID: "episode-b", TaskID: "same-task"},
	}}
	if _, err := NewService(repository, &episodeContent{}).FreezeDataset(context.Background(), "purpose", "seed", []string{"candidate-a", "candidate-b"}); err == nil {
		t.Fatal("near-duplicate source tasks were accepted into one snapshot")
	}
}
