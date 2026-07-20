package agent

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/execution"
)

type contextTestModel struct{ calls int }

func TestRuntimeContextSuppliesCurrentTimeTimezoneAndChannel(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	observed := time.Date(2026, time.July, 20, 9, 30, 0, 0, location)
	context := runtimeContext("lark", observed)
	for _, expected := range []string{"2026-07-20T09:30:00+08:00", "2026-07-20T01:30:00Z", "Asia/Shanghai", "Source channel: lark"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("runtime context is missing %q: %s", expected, context)
		}
	}
}

func (*contextTestModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (m *contextTestModel) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	m.calls++
	return ModelResponse{
		Message: Message{Role: "assistant", Content: "## Goal\nContinue the user's work.\n\n## Confirmed progress and evidence\nEarlier turns were compacted with source indices."},
		Usage:   Usage{Provider: "test", Model: "compact", InputTokens: 1200, OutputTokens: 80, ModelCalls: 1},
	}, nil
}

func TestBuildMessagesSendsImagesOnlyToVisionCapableProvider(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	messageRef, err := contentStore.Put(context.Background(), []byte("what is shown?"), content.Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	imageBody := []byte("fake-png-bytes")
	imageRef, err := contentStore.Put(context.Background(), imageBody, content.Metadata{MediaType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{content: contentStore}
	task := TaskContext{Messages: []ContextRecord{{
		ID: "message", Role: "user", ContentRef: messageRef,
		Attachments: []ContextAttachment{{ID: "image", Name: "photo.png", MediaType: "image/png", SizeBytes: int64(len(imageBody)), ContentRef: imageRef}},
	}}}
	vision, err := service.buildMessages(context.Background(), task, ModelCapabilities{Image: true, ContextTokens: 32_768})
	if err != nil {
		t.Fatal(err)
	}
	if len(vision) != 1 || len(vision[0].Images) != 1 || vision[0].Images[0].Data != base64.StdEncoding.EncodeToString(imageBody) {
		t.Fatalf("vision messages=%+v", vision)
	}
	textOnly, err := service.buildMessages(context.Background(), task, ModelCapabilities{ContextTokens: 32_768})
	if err != nil {
		t.Fatal(err)
	}
	if len(textOnly[0].Images) != 0 || !strings.Contains(textOnly[0].Content, "do not claim to have inspected") {
		t.Fatalf("text-only messages=%+v", textOnly)
	}
}

type contextTestRepository struct{ checkpoint ContextCheckpoint }

func (r *contextTestRepository) SaveContextCheckpoint(_ context.Context, _, _ string, checkpoint ContextCheckpoint) error {
	r.checkpoint = checkpoint
	return nil
}

func TestPersistentContextCompactionKeepsRecentTurnAndPersistsCheckpoint(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &contextTestRepository{}
	model := &contextTestModel{}
	service := &Service{repository: nil, content: contentStore, model: model, logger: slog.Default()}
	service.repository = contextRepositoryAdapter{contextTestRepository: repository}

	records := make([]ContextRecord, 0, 12)
	messages := make([]Message, 0, 12)
	for index := 0; index < 12; index++ {
		role := "assistant"
		if index%2 == 0 {
			role = "user"
		}
		records = append(records, ContextRecord{ID: "message-" + string(rune('a'+index)), Sequence: int64(index + 1), Role: role})
		messages = append(messages, Message{Role: role, Content: strings.Repeat("context evidence ", 170)})
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", Messages: records}
	request := ModelRequest{System: strings.Repeat("stable system ", 100), Messages: messages, MaxOutputTokens: 512}
	manifest := execution.ContextManifest{ExternalData: &execution.ExternalData{}}

	compacted, usage, err := service.compactPersistentContext(context.Background(), task, request, ModelCapabilities{
		Text: true, ContextTokens: 8_000, MaxOutputTokens: 2_048,
	}, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls == 0 || usage.ModelCalls == 0 {
		t.Fatalf("compactor calls=%d usage=%+v", model.calls, usage)
	}
	if repository.checkpoint.ID == "" || repository.checkpoint.SummarizedCount == 0 {
		t.Fatalf("checkpoint=%+v", repository.checkpoint)
	}
	if compacted.Messages[0].Role != "system" || !strings.Contains(compacted.Messages[0].Content, "durable context checkpoint") {
		t.Fatalf("first compacted message=%+v", compacted.Messages[0])
	}
	if got := compacted.Messages[len(compacted.Messages)-1].Content; got != messages[len(messages)-1].Content {
		t.Fatal("latest conversation message was not preserved verbatim")
	}
	if !manifest.Compression.Applied || manifest.Compression.CheckpointID != repository.checkpoint.ID {
		t.Fatalf("manifest compression=%+v", manifest.Compression)
	}
}

// contextRepositoryAdapter supplies the main Repository methods without
// weakening the production interface merely for this focused test.
type contextRepositoryAdapter struct{ *contextTestRepository }

func (contextRepositoryAdapter) ClaimTask(context.Context, string, string, time.Duration, string, string, string) (TaskContext, bool, error) {
	return TaskContext{}, false, nil
}
func (contextRepositoryAdapter) MarkInvocationDispatched(context.Context, string) error { return nil }
func (contextRepositoryAdapter) CommitArtifact(context.Context, Commit) error           { return nil }
func (contextRepositoryAdapter) CommitProgress(context.Context, ProgressCommit) (bool, error) {
	return true, nil
}
func (contextRepositoryAdapter) PauseForApproval(context.Context, ApprovalCommit) error { return nil }
func (contextRepositoryAdapter) ClaimApprovalResume(context.Context, string, string, time.Duration) (ApprovalResume, bool, error) {
	return ApprovalResume{}, false, nil
}
func (contextRepositoryAdapter) PauseForSubagent(context.Context, SubagentWaitCommit) error {
	return nil
}
func (contextRepositoryAdapter) ClaimSubagentResume(context.Context, string, string, time.Duration) (SubagentResume, bool, error) {
	return SubagentResume{}, false, nil
}
func (contextRepositoryAdapter) UpdateInvocationContext(context.Context, string, string) error {
	return nil
}
func (contextRepositoryAdapter) TaskCancelRequested(context.Context, string) (bool, error) {
	return false, nil
}
func (contextRepositoryAdapter) CommitTaskCancellation(context.Context, string, string, string, content.Ref, Usage) error {
	return nil
}
func (contextRepositoryAdapter) SaveAgentCheckpoint(context.Context, TaskContext, string, content.Ref) error {
	return nil
}
func (contextRepositoryAdapter) LoadTaskInputsAfter(context.Context, string, int64) ([]ContextRecord, error) {
	return nil, nil
}
