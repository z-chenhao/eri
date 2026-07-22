package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/store/sqlite"
)

func TestMemoryToolBindsExplicitUserMemoryToGatewayTask(t *testing.T) {
	service := &fakeMemoryService{}
	candidate, err := NewMemory(service, fakeMemoryContentStore{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := candidate.Descriptor()
	for _, required := range []string{"does not need to say remember", "exact useful excerpt", "never paraphrase or infer", "explicit_user_memory=true only", "Use search or list before correcting", "replaces_memory_id", "old item remains archived", "Never record transient task details"} {
		if !strings.Contains(descriptor.Purpose, required) {
			t.Fatalf("memory tool description is missing %q", required)
		}
	}
	properties := descriptor.InputSchema["properties"].(map[string]any)
	if _, exists := properties["claim_id"]; exists {
		t.Fatal("model-visible Memory schema still exposes claim_id")
	}
	if _, exists := properties["relation"]; exists {
		t.Fatal("model-visible Memory schema still exposes evidence relation")
	}
	if _, exists := properties["replaces_memory_id"]; !exists {
		t.Fatal("model-visible Memory schema is missing replaces_memory_id")
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
		t.Fatal("expected missing source interaction to reject untraceable user memory")
	}
	prepared.SourceInteractionID = "runtime-introduction"
	prepared.SourceInteractionText = "I prefer quiet hotel rooms."
	prepared.SourceInteractionRole = "system"
	prepared.SourceInteractionKind = "internal_trigger"
	prepared.SourceContextValidated = true
	if _, err := candidate.Execute(context.Background(), prepared); err == nil {
		t.Fatal("Runtime text was accepted as a direct user Memory")
	}

	prepared.SourceInteractionID = "user-message"
	prepared.SourceInteractionText = "Please remember this: I prefer quiet hotel rooms."
	prepared.SourceInteractionRole = "user"
	prepared.SourceInteractionKind = "text"
	prepared.SourceContextValidated = true
	if _, err := candidate.Execute(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if service.captured.SourceRef != "interaction:user-message" || service.captured.SourceType != "user" || service.captured.IndependenceGroup != "user:self" {
		t.Fatalf("captured provenance = %+v", service.captured)
	}

	correction, err := candidate.Prepare(context.Background(), json.RawMessage(`{
		"operation":"record",
		"replaces_memory_id":"memory-original",
		"statement":"I now prefer rooms near the lift."
	}`))
	if err != nil {
		t.Fatal(err)
	}
	correction.SourceInteractionID = "correction-message"
	correction.SourceInteractionText = "I now prefer rooms near the lift."
	correction.SourceInteractionRole = "user"
	correction.SourceInteractionKind = "text"
	correction.SourceContextValidated = true
	if _, err := candidate.Execute(context.Background(), correction); err != nil {
		t.Fatal(err)
	}
	if service.captured.ReplacesMemoryID != "memory-original" || service.captured.Relation != memory.Supports || service.captured.SourceRef != "interaction:correction-message" {
		t.Fatalf("captured correction = %+v", service.captured)
	}
	if !service.captured.DirectUserStatement || service.captured.ExplicitUserMemory {
		t.Fatalf("direct stable statement should be recorded without requiring an explicit remember request: %+v", service.captured)
	}

	unsupported, err := candidate.Prepare(context.Background(), json.RawMessage(`{
		"operation":"record",
		"statement":"The user dislikes all noisy places.",
		"kind":"preference"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	unsupported.SourceInteractionID = "user-message"
	unsupported.SourceInteractionText = "I prefer quiet hotel rooms."
	unsupported.SourceInteractionRole = "user"
	unsupported.SourceInteractionKind = "text"
	unsupported.SourceContextValidated = false
	if _, err := candidate.Execute(context.Background(), unsupported); err == nil {
		t.Fatal("unvalidated source context was accepted as a direct user statement")
	}
}

func TestMemoryToolBindsSearchAndListToRunProvenance(t *testing.T) {
	service := &fakeMemoryService{}
	candidate, err := NewMemory(service, fakeMemoryContentStore{})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"search", "list"} {
		prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"`+operation+`","query":"travel","limit":3}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := candidate.Execute(context.Background(), prepared); err == nil {
			t.Fatalf("%s accepted an untraceable Memory read", operation)
		}
		prepared.RunID = "run-1"
		prepared.SourceInteractionID = "source-1"
		prepared.InvocationID = "intent-" + operation
		prepared.SourceContextValidated = true
		if _, err := candidate.Execute(context.Background(), prepared); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
	if service.recalled.RunID != "run-1" || service.recalled.SourceInteractionID != "source-1" || service.recalled.RetrievalID != "intent-search" || service.recalled.Query != "travel" {
		t.Fatalf("search provenance=%+v", service.recalled)
	}
	if service.inspected.RunID != "run-1" || service.inspected.SourceInteractionID != "source-1" || service.inspected.RetrievalID != "intent-list" || service.inspected.Limit != 3 {
		t.Fatalf("list provenance=%+v", service.inspected)
	}
}

func TestMemoryToolReplacesExistingMemoryWithRealMemoryService(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x75}, 32)
	contentStore, err := content.New(filepath.Join(root, "content"), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(filepath.Join(root, "metadata", "eri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := memory.NewService(store, contentStore, key)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := service.Capture(ctx, memory.CaptureRequest{
		Statement: "I prefer hotel rooms with a window.", Kind: "preference", Scope: "travel",
		Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:preference-original",
		IndependenceGroup: "user:self", DirectUserStatement: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	candidate, err := NewMemory(service, contentStore)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"operation": "record", "replaces_memory_id": initial.MemoryID,
		"statement": "I now prefer rooms near the lift.", "kind": "preference", "scope": "travel",
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := candidate.Prepare(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	prepared.SourceInteractionID = "preference-correction"
	prepared.SourceInteractionText = "I now prefer rooms near the lift."
	prepared.SourceInteractionRole = "user"
	prepared.SourceInteractionKind = "text"
	prepared.SourceContextValidated = true
	if _, err := candidate.Execute(ctx, prepared); err != nil {
		t.Fatal(err)
	}

	recalled, err := service.Retrieve(ctx, "lift", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Entries) != 1 || recalled.Entries[0].Statement != "I now prefer rooms near the lift." || recalled.Entries[0].ClaimID == initial.ClaimID || recalled.Entries[0].ReplacesMemoryID != initial.MemoryID || recalled.Entries[0].Status != memory.Supported {
		t.Fatalf("real replacement recall=%+v", recalled)
	}
	if stale, err := service.Retrieve(ctx, "window", 10); err != nil || len(stale.Entries) != 0 {
		t.Fatalf("replaced fact remained recallable: %+v err=%v", stale, err)
	}
	inspected, err := service.Inspect(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Entries) != 2 {
		t.Fatalf("replacement history=%+v", inspected)
	}
	var oldLifecycle string
	for _, entry := range inspected.Entries {
		if entry.MemoryID == initial.MemoryID {
			oldLifecycle = entry.LifecycleStatus
		}
	}
	if oldLifecycle != "archived" {
		t.Fatalf("old memory lifecycle=%q, want archived", oldLifecycle)
	}
	if _, err := service.Delete(ctx, recalled.Entries[0].MemoryID); err != nil {
		t.Fatal(err)
	}
	if stale, err := service.Retrieve(ctx, "window", 10); err != nil || len(stale.Entries) != 0 {
		t.Fatalf("deleting replacement revived archived memory: %+v err=%v", stale, err)
	}
}

type fakeMemoryService struct {
	captured  memory.CaptureRequest
	recalled  memory.RecallRequest
	inspected memory.RecallRequest
}

func (s *fakeMemoryService) Capture(_ context.Context, request memory.CaptureRequest) (memory.Entry, error) {
	s.captured = request
	return memory.Entry{}, nil
}
func (s *fakeMemoryService) Recall(_ context.Context, request memory.RecallRequest) (memory.Bundle, error) {
	s.recalled = request
	return memory.Bundle{RetrievalID: "search-retrieval"}, nil
}
func (*fakeMemoryService) MarkUsed(context.Context, string, []string) error { return nil }
func (*fakeMemoryService) Inspect(context.Context, int) (memory.Bundle, error) {
	return memory.Bundle{}, nil
}
func (s *fakeMemoryService) InspectForRun(_ context.Context, request memory.RecallRequest) (memory.Bundle, error) {
	s.inspected = request
	return memory.Bundle{RetrievalID: "list-retrieval"}, nil
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
