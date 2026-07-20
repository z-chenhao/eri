// Package userdata implements user-owned data export and erasure. It keeps the
// destructive operation behind Eri's normal Tool Gateway and delays erasure
// until the final confirmation delivery has been accepted by the channel.
package userdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/runtime"
)

const ExportFormat = "eri-user-data-export"

// Snapshot is a consistent metadata projection plus immutable content refs.
// It intentionally contains no filesystem paths, encryption keys or runtime
// session material.
type Snapshot struct {
	Tables   map[string][]map[string]any
	Contents []content.Ref
}

type ErasureJob struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"task_id"`
	Status         string    `json:"status"`
	ContentObjects int       `json:"content_objects"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
}

type Repository interface {
	BuildUserDataSnapshot(context.Context) (Snapshot, error)
	ScheduleUserDataErasure(context.Context, ErasureJob) (ErasureJob, error)
	PrepareUserDataErasure(context.Context, string) ([]content.Ref, bool, error)
	CommitUserDataErasure(context.Context, string, int, time.Time) error
}

type ContentStore interface {
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

// Supplement lets separated user-owned configuration domains participate in
// the same export/erasure transaction without coupling userdata to plugins or
// future channel implementations.
type Supplement interface {
	ExportUserData(context.Context) (map[string][]byte, error)
	EraseUserData(context.Context) error
}

type Service struct {
	repository  Repository
	content     ContentStore
	now         func() time.Time
	supplements []Supplement
}

func (s *Service) AddSupplement(supplement Supplement) {
	if supplement != nil {
		s.supplements = append(s.supplements, supplement)
	}
}

func NewService(repository Repository, contentStore ContentStore) *Service {
	return &Service{repository: repository, content: contentStore, now: time.Now}
}

// Export returns a portable ZIP with readable metadata JSON and the exact
// plaintext content bytes referenced by that snapshot.
func (s *Service) Export(ctx context.Context) ([]byte, error) {
	snapshot, err := s.repository.BuildUserDataSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("build user data snapshot: %w", err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)

	tableNames := make([]string, 0, len(snapshot.Tables))
	tableCounts := make(map[string]int, len(snapshot.Tables))
	for name, rows := range snapshot.Tables {
		tableNames = append(tableNames, name)
		tableCounts[name] = len(rows)
	}
	sort.Strings(tableNames)
	manifest := map[string]any{
		"format":          ExportFormat,
		"version":         1,
		"exported_at":     s.now().UTC(),
		"metadata_tables": tableCounts,
		"content_objects": len(snapshot.Contents),
		"excluded": []string{
			"encryption keys", "runtime sockets and sessions", "provider credentials", "rebuildable indexes",
		},
	}
	if err := writeJSONFile(archive, "manifest.json", manifest); err != nil {
		return nil, err
	}
	for _, name := range tableNames {
		if err := writeJSONFile(archive, path.Join("metadata", name+".json"), snapshot.Tables[name]); err != nil {
			return nil, err
		}
	}
	for _, supplement := range s.supplements {
		files, err := supplement.ExportUserData(ctx)
		if err != nil {
			return nil, fmt.Errorf("export supplemental user data: %w", err)
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			clean := path.Clean(strings.TrimPrefix(name, "/"))
			if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("supplement returned an unsafe export path")
			}
			entry, err := archive.Create(path.Join("configuration", clean))
			if err != nil {
				return nil, err
			}
			if _, err := entry.Write(files[name]); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(snapshot.Contents, func(i, j int) bool {
		if snapshot.Contents[i].ObjectID == snapshot.Contents[j].ObjectID {
			return snapshot.Contents[i].Version < snapshot.Contents[j].Version
		}
		return snapshot.Contents[i].ObjectID < snapshot.Contents[j].ObjectID
	})
	contentIndex := make([]map[string]any, 0, len(snapshot.Contents))
	for _, ref := range snapshot.Contents {
		body, err := s.content.Get(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("read content object %s v%d: %w", ref.ObjectID, ref.Version, err)
		}
		filename := path.Join("content", fmt.Sprintf("%s-v%d%s", ref.ObjectID, ref.Version, mediaExtension(ref.MediaType)))
		entry, err := archive.Create(filename)
		if err != nil {
			return nil, fmt.Errorf("create export content entry: %w", err)
		}
		if _, err := entry.Write(body); err != nil {
			return nil, fmt.Errorf("write export content entry: %w", err)
		}
		contentIndex = append(contentIndex, map[string]any{"path": filename, "ref": ref})
	}
	if err := writeJSONFile(archive, "content/index.json", contentIndex); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close user data export: %w", err)
	}
	return buffer.Bytes(), nil
}

// Schedule records an approved erasure request. The repository promotes it to
// the outbox only after this task's final delivery is durably accepted.
func (s *Service) Schedule(ctx context.Context, taskID string) (ErasureJob, error) {
	if strings.TrimSpace(taskID) == "" {
		return ErasureJob{}, fmt.Errorf("task id is required")
	}
	id, err := identifier.New()
	if err != nil {
		return ErasureJob{}, err
	}
	now := s.now().UTC()
	return s.repository.ScheduleUserDataErasure(ctx, ErasureJob{
		ID: id, TaskID: taskID, Status: "awaiting_delivery", CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Service) HandleErase(ctx context.Context, item runtime.OutboxItem) error {
	refs, found, err := s.repository.PrepareUserDataErasure(ctx, item.AggregateID)
	if err != nil || !found {
		return err
	}
	for _, supplement := range s.supplements {
		if err := supplement.EraseUserData(ctx); err != nil {
			return fmt.Errorf("erase supplemental user data: %w", err)
		}
	}
	for _, ref := range refs {
		if err := s.content.Delete(ctx, ref); err != nil {
			return fmt.Errorf("erase content object %s v%d: %w", ref.ObjectID, ref.Version, err)
		}
	}
	if err := s.repository.CommitUserDataErasure(ctx, item.AggregateID, len(refs), s.now().UTC()); err != nil {
		return fmt.Errorf("commit user data erasure: %w", err)
	}
	return nil
}

func writeJSONFile(archive *zip.Writer, name string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	entry, err := archive.Create(name)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := entry.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func mediaExtension(mediaType string) string {
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	switch mediaType {
	case "application/json":
		return ".json"
	case "text/plain", "text/markdown":
		return ".txt"
	case "text/html":
		return ".html"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	default:
		return ".bin"
	}
}
