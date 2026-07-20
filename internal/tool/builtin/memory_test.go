package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/memory"
)

func TestMemoryToolBindsExplicitUserMemoryToGatewayTask(t *testing.T) {
	service := &fakeMemoryService{}
	candidate, err := NewMemory(service, fakeMemoryContentStore{})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"explicitly asks to remember", "direct_user_statement=true", "explicit_user_memory=true only", "first recall or inspect it", "exact returned claim_id", "relation=contradicts or relation=qualifies", "Never record transient task detail"} {
		if !strings.Contains(candidate.Descriptor().Purpose, required) {
			t.Fatalf("memory tool description is missing %q", required)
		}
	}
	prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{
		"operation":"record",
		"statement":"I prefer quiet hotel rooms.",
		"kind":"preference",
		"explicit_user_memory":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.Execute(context.Background(), prepared); err == nil {
		t.Fatal("expected missing gateway task to reject untraceable user memory")
	}

	prepared.TaskID = "task-user-message"
	if _, err := candidate.Execute(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if service.captured.SourceRef != "task:task-user-message" || service.captured.SourceType != "user" || service.captured.IndependenceGroup != "user:self" {
		t.Fatalf("captured provenance = %+v", service.captured)
	}

	correction, err := candidate.Prepare(context.Background(), json.RawMessage(`{
		"operation":"record",
		"claim_id":"claim-original",
		"statement":"I now prefer rooms near the lift.",
		"relation":"contradicts",
		"direct_user_statement":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	correction.TaskID = "task-correction"
	if _, err := candidate.Execute(context.Background(), correction); err != nil {
		t.Fatal(err)
	}
	if service.captured.ClaimID != "claim-original" || service.captured.Relation != memory.Contradicts || service.captured.SourceRef != "task:task-correction" {
		t.Fatalf("captured correction = %+v", service.captured)
	}
}

type fakeMemoryService struct {
	captured memory.CaptureRequest
}

func (s *fakeMemoryService) Capture(_ context.Context, request memory.CaptureRequest) (memory.Entry, error) {
	s.captured = request
	return memory.Entry{}, nil
}
func (*fakeMemoryService) Retrieve(context.Context, string, int) (memory.Bundle, error) {
	return memory.Bundle{}, nil
}
func (*fakeMemoryService) MarkUsed(context.Context, string, []string) error { return nil }
func (*fakeMemoryService) Inspect(context.Context, int) (memory.Bundle, error) {
	return memory.Bundle{}, nil
}
func (*fakeMemoryService) Promote(context.Context, string) error { return nil }
func (*fakeMemoryService) Consolidate(context.Context) (memory.ConsolidationReport, error) {
	return memory.ConsolidationReport{}, nil
}
func (*fakeMemoryService) SetUsagePolicy(context.Context, string, string) error { return nil }
func (*fakeMemoryService) Delete(context.Context, string) (memory.DeletePlan, error) {
	return memory.DeletePlan{}, nil
}
func (*fakeMemoryService) Export(context.Context) ([]byte, error) { return []byte(`[]`), nil }

type fakeMemoryContentStore struct{}

func (fakeMemoryContentStore) Put(_ context.Context, body []byte, metadata content.Metadata) (content.Ref, error) {
	return content.Ref{ObjectID: "memory-export", Version: 1, MediaType: metadata.MediaType, SizeBytes: int64(len(body))}, nil
}
