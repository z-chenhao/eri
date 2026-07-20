package userdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/runtime"
)

func TestExportProducesPortableMetadataAndContentArchive(t *testing.T) {
	ref := content.Ref{ObjectID: "object-a", Version: 1, ContentHash: "hash", MediaType: "text/plain; charset=utf-8", SizeBytes: 12}
	repository := &fakeRepository{snapshot: Snapshot{
		Tables:   map[string][]map[string]any{"interactions": {{"id": "message-a", "role": "user"}}},
		Contents: []content.Ref{ref},
	}}
	contents := &fakeContentStore{bodies: map[string][]byte{ref.ObjectID: []byte("private note")}}
	service := NewService(repository, contents)
	supplement := &fakeSupplement{files: map[string][]byte{"plugins/calendar/active.json": []byte(`{"version":"1.0.0"}`)}}
	service.AddSupplement(supplement)
	service.now = func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) }

	body, err := service.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name], err = io.ReadAll(entry)
		entry.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !json.Valid(files["manifest.json"]) || !bytes.Contains(files["manifest.json"], []byte(ExportFormat)) {
		t.Fatalf("manifest = %s", files["manifest.json"])
	}
	if !bytes.Contains(files["metadata/interactions.json"], []byte("message-a")) {
		t.Fatalf("metadata = %s", files["metadata/interactions.json"])
	}
	if got := files["content/object-a-v1.txt"]; !bytes.Equal(got, []byte("private note")) {
		t.Fatalf("content = %q", got)
	}
	if !json.Valid(files["content/index.json"]) {
		t.Fatalf("content index = %s", files["content/index.json"])
	}
	if !bytes.Contains(files["configuration/plugins/calendar/active.json"], []byte("1.0.0")) {
		t.Fatalf("supplemental configuration = %s", files["configuration/plugins/calendar/active.json"])
	}
}

func TestErasureDeletesContentBeforeCommittingMetadata(t *testing.T) {
	refs := []content.Ref{{ObjectID: "a", Version: 1}, {ObjectID: "b", Version: 1}}
	repository := &fakeRepository{erasureRefs: refs, erasureFound: true}
	contents := &fakeContentStore{bodies: map[string][]byte{"a": {}, "b": {}}}
	service := NewService(repository, contents)
	supplement := &fakeSupplement{}
	service.AddSupplement(supplement)
	service.now = func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) }
	if err := service.HandleErase(context.Background(), runtime.OutboxItem{AggregateID: "job"}); err != nil {
		t.Fatal(err)
	}
	if len(contents.deleted) != 2 || repository.committedCount != 2 || !supplement.erased {
		t.Fatalf("deleted=%v committed=%d supplement_erased=%v", contents.deleted, repository.committedCount, supplement.erased)
	}
}

type fakeSupplement struct {
	files  map[string][]byte
	erased bool
}

func (s *fakeSupplement) ExportUserData(context.Context) (map[string][]byte, error) {
	return s.files, nil
}
func (s *fakeSupplement) EraseUserData(context.Context) error {
	s.erased = true
	return nil
}

type fakeRepository struct {
	snapshot       Snapshot
	erasureRefs    []content.Ref
	erasureFound   bool
	committedCount int
}

func (r *fakeRepository) BuildUserDataSnapshot(context.Context) (Snapshot, error) {
	return r.snapshot, nil
}
func (r *fakeRepository) ScheduleUserDataErasure(_ context.Context, job ErasureJob) (ErasureJob, error) {
	return job, nil
}
func (r *fakeRepository) PrepareUserDataErasure(context.Context, string) ([]content.Ref, bool, error) {
	return r.erasureRefs, r.erasureFound, nil
}
func (r *fakeRepository) CommitUserDataErasure(_ context.Context, _ string, count int, _ time.Time) error {
	r.committedCount = count
	return nil
}

type fakeContentStore struct {
	bodies  map[string][]byte
	deleted []string
}

func (s *fakeContentStore) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	return append([]byte(nil), s.bodies[ref.ObjectID]...), nil
}
func (s *fakeContentStore) Delete(_ context.Context, ref content.Ref) error {
	s.deleted = append(s.deleted, ref.ObjectID)
	delete(s.bodies, ref.ObjectID)
	return nil
}
