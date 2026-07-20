package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/userdata"
)

func TestUserDataToolSeparatesPortableExportFromStrongErasure(t *testing.T) {
	service := &fakeUserDataService{}
	store := &fakeUserDataContentStore{}
	candidate, err := NewUserData(service, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"strong approval", "irreversible", "schedules the deletion request now in an awaiting-delivery state", "actual erasure begins only after the local channel accepts"} {
		if !strings.Contains(candidate.Descriptor().Purpose, required) {
			t.Fatalf("user-data tool description is missing %q", required)
		}
	}
	export, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"export"}`))
	if err != nil || export.Action.Effect != policy.ReadOnly {
		t.Fatalf("export=%+v err=%v", export, err)
	}
	exportResult, err := candidate.Execute(context.Background(), export)
	if err != nil || len(exportResult.Attachments) != 1 || exportResult.Attachments[0].Name != "eri-user-data-export.zip" {
		t.Fatalf("result=%+v err=%v", exportResult, err)
	}

	erase, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"delete_all"}`))
	if err != nil || erase.Action.Effect != policy.Destructive || !erase.Action.Irreversible {
		t.Fatalf("erase=%+v err=%v", erase, err)
	}
	erase.TaskID = "task-from-gateway"
	eraseResult, err := candidate.Execute(context.Background(), erase)
	if err != nil || service.scheduledTask != "task-from-gateway" {
		t.Fatalf("result=%+v task=%q err=%v", eraseResult, service.scheduledTask, err)
	}
}

type fakeUserDataService struct{ scheduledTask string }

func (s *fakeUserDataService) Export(context.Context) ([]byte, error) { return []byte("zip"), nil }
func (s *fakeUserDataService) Schedule(_ context.Context, taskID string) (userdata.ErasureJob, error) {
	s.scheduledTask = taskID
	return userdata.ErasureJob{ID: "job", TaskID: taskID, Status: "awaiting_delivery"}, nil
}

type fakeUserDataContentStore struct{}

func (s *fakeUserDataContentStore) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	return content.Ref{ObjectID: "export", Version: 1, MediaType: metadata.MediaType, SizeBytes: int64(len(body))}, nil
}

var _ = time.Time{}
