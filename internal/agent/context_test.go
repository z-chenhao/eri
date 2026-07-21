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
	"github.com/z-chenhao/eri/internal/tool"
)

type contextTestModel struct{ calls int }

func TestRuntimeContextSuppliesCurrentTimeTimezoneAndChannel(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	observed := time.Date(2026, time.July, 20, 9, 30, 0, 0, location)
	context := runtimeContext("lark", observed)
	for _, expected := range []string{"Current local date: 2026-07-20", "Asia/Shanghai", "Source channel: lark", "do not infer a wall-clock time"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("runtime context is missing %q: %s", expected, context)
		}
	}
	if strings.Contains(context, "09:30:00") || strings.Contains(context, "01:30:00") {
		t.Fatalf("runtime context contains cache-busting wall-clock precision: %s", context)
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

func TestBuildMessagesScopesOtherTasksWithoutChangingCurrentTaskText(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	currentRef, err := contentStore.Put(context.Background(), []byte("Finish the approved draft."), content.Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	otherRef, err := contentStore.Put(context.Background(), []byte("Start a separate research task."), content.Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{content: contentStore}
	task := TaskContext{TaskID: "current-task", Messages: []ContextRecord{
		{ID: "current-message", TaskID: "current-task", Role: "user", ContentRef: currentRef},
		{ID: "other-message", TaskID: "other-task", Role: "user", ContentRef: otherRef},
	}}
	messages, err := service.buildMessages(context.Background(), task, ModelCapabilities{ContextTokens: 32_768})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || !strings.Contains(messages[0].Content, "relationship evidence") || !strings.Contains(messages[2].Content, "<other_task_context>") {
		t.Fatalf("scoped messages=%+v", messages)
	}
	if got := latestTaskContentForTask(messages, task.Messages, task.TaskID); got != "Finish the approved draft." {
		t.Fatalf("current task text=%q", got)
	}
}

func TestCurrentTaskMessagesPreserveSchedulerProvenanceAndObjective(t *testing.T) {
	scheduledFor := time.Date(2026, time.July, 21, 1, 0, 0, 0, time.UTC)
	messages := currentTaskMessages(execution.TaskCapsule{
		TaskID: "task-1", SourceInteractionID: "trigger-1", SourceKind: "internal_trigger",
		SourceRole: "system", TriggerChannel: "scheduler", TriggerEvent: "commitment.due",
		TriggerState: "occurred", ExecutionPhase: "fulfillment", CommitmentID: "commitment-1",
		ScheduledFor: scheduledFor,
	}, "Check the monitored sources for material changes.", 42)
	if len(messages) != 3 || messages[0].Role != "system" || messages[1].Role != "system" || messages[2].Role != "system" {
		t.Fatalf("current task messages = %+v", messages)
	}
	for _, expected := range []string{
		"<current_task>", `"source_role":"system"`, `"trigger_channel":"scheduler"`,
		`"trigger_event":"commitment.due"`, `"trigger_state":"occurred"`,
		`"execution_phase":"fulfillment"`,
		`"commitment_id":"commitment-1"`, "2026-07-21T01:00:00Z",
		"trigger registration is already complete", "Do not recreate, update, or extend",
	} {
		if !strings.Contains(messages[0].Content, expected) {
			t.Fatalf("current task is missing %q: %s", expected, messages[0].Content)
		}
	}
	if !strings.Contains(messages[1].Content, "<task_objective>") || !strings.Contains(messages[1].Content, "Check the monitored sources") {
		t.Fatalf("task objective = %s", messages[1].Content)
	}
	if !strings.Contains(messages[2].Content, "<current_step>") || !strings.Contains(messages[2].Content, "input_sequence=42") {
		t.Fatalf("current step = %s", messages[2].Content)
	}
}

func TestCurrentTaskMessagesDoNotElevateUserObjectiveRole(t *testing.T) {
	messages := currentTaskMessages(execution.TaskCapsule{
		TaskID: "task-1", SourceInteractionID: "user-1", SourceKind: "text", SourceRole: "user", TriggerChannel: "web",
	}, "Review this report.", 3)
	if len(messages) != 3 || messages[1].Role != "user" || !strings.Contains(messages[1].Content, "Review this report") {
		t.Fatalf("user task objective role was not preserved: %+v", messages)
	}
	if strings.Contains(messages[0].Content, "Review this report") {
		t.Fatalf("user objective was copied into Runtime system metadata: %s", messages[0].Content)
	}
	if strings.Contains(messages[0].Content, "trigger registration is already complete") {
		t.Fatalf("direct user task was mislabeled as an occurred event: %s", messages[0].Content)
	}
}

func TestFulfillmentTaskCannotSeeSourceCommitmentCapability(t *testing.T) {
	descriptors := []tool.Descriptor{
		{ID: "builtin.commitments", Version: "1"},
		{ID: "builtin.notification", Version: "1"},
		{ID: "builtin.web", Version: "1"},
	}
	filtered := descriptorsForTask(descriptors, execution.TaskCapsule{
		TriggerEvent: execution.TriggerEventCommitmentDue, ExecutionPhase: execution.TaskPhaseFulfillment,
	})
	if len(filtered) != 2 || filtered[0].ID != "builtin.notification" || filtered[1].ID != "builtin.web" {
		t.Fatalf("fulfillment tools = %+v", filtered)
	}
	ordinary := descriptorsForTask(descriptors, execution.TaskCapsule{})
	if len(ordinary) != len(descriptors) {
		t.Fatalf("ordinary task tools = %+v", ordinary)
	}
}

type contextTestRepository struct {
	checkpoint ContextCheckpoint
	inputs     []ContextRecord
	updates    []ContextRecord
	manifest   string
}

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

func TestLoopCompactionRepinsDynamicAndCurrentTaskContext(t *testing.T) {
	repository := &contextTestRepository{}
	model := &contextTestModel{}
	service := &Service{content: nil, model: model, logger: slog.Default()}
	service.repository = contextRepositoryAdapter{contextTestRepository: repository}

	messages := make([]Message, 0, 16)
	for index := 0; index < 12; index++ {
		role := "assistant"
		if index%2 == 0 {
			role = "user"
		}
		messages = append(messages, Message{Role: role, Content: strings.Repeat("historical execution evidence ", 180)})
	}
	messages = append(messages,
		Message{Role: "system", Content: "<relevant_memory_context>\nselected durable preference\n</relevant_memory_context>"},
		Message{Role: "system", Content: "<current_runtime_context>\nCurrent local date: 2026-07-21\n</current_runtime_context>"},
	)
	capsule := execution.TaskCapsule{TaskID: "task-1", SourceInteractionID: "source-1", SourceKind: "internal_trigger", SourceRole: "system", TriggerChannel: "scheduler"}
	messages = append(messages, currentTaskMessages(capsule, "Check the monitored sources.", 7)...)
	request := ModelRequest{System: "stable system", Messages: messages, MaxOutputTokens: 512}
	state := loopState{ContextManifest: execution.ContextManifest{CurrentTask: &capsule}}
	compacted, usage, err := service.compactLoopContext(context.Background(), TaskContext{
		TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1", CurrentTask: capsule,
	}, request, ModelCapabilities{Text: true, ContextTokens: 8_000, MaxOutputTokens: 2_048}, &state)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls == 0 || usage.ModelCalls == 0 {
		t.Fatalf("compactor calls=%d usage=%+v", model.calls, usage)
	}
	joined := ""
	for _, message := range compacted.Messages {
		joined += message.Content
	}
	for _, required := range []string{"selected durable preference", "Current local date: 2026-07-21", "<current_task>", "Check the monitored sources.", "<current_step>"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("compacted context lost %q: %s", required, joined)
		}
	}
	if last := compacted.Messages[len(compacted.Messages)-1]; !strings.HasPrefix(last.Content, "<current_step>") {
		t.Fatalf("current step is not final after compaction: %+v", last)
	}
}

func TestJoinedInputRefreshesCurrentStepWithoutReplacingTaskCapsule(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	joinedRef, err := contentStore.Put(context.Background(), []byte("Use the corrected scope."), content.Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	capsule := execution.TaskCapsule{TaskID: "task-1", SourceInteractionID: "source-1", SourceKind: "text", SourceRole: "user", TriggerChannel: "web"}
	repository := &contextTestRepository{inputs: []ContextRecord{{ID: "joined-1", Kind: "text", Sequence: 2, Role: "user", ContentRef: joinedRef}}}
	service := &Service{content: contentStore, logger: slog.Default()}
	service.repository = contextRepositoryAdapter{contextTestRepository: repository}
	request := ModelRequest{Messages: currentTaskMessages(capsule, "Review the report.", 1)}
	state := loopState{InputSequence: 1, Capabilities: ModelCapabilities{ContextTokens: 8_000}, ContextManifest: execution.ContextManifest{CurrentTask: &capsule}}
	changed, err := service.refreshTaskInputs(context.Background(), TaskContext{
		TaskID: "task-1", RunID: "run-1", InvocationID: "invocation-1", CurrentTask: capsule,
	}, &request, &state)
	if err != nil || !changed {
		t.Fatalf("refresh changed=%t err=%v", changed, err)
	}
	if state.InputSequence != 2 || state.TaskText != "Use the corrected scope." {
		t.Fatalf("refreshed state sequence=%d task=%q", state.InputSequence, state.TaskText)
	}
	if len(request.Messages) != 5 || request.Messages[3].Role != "user" || request.Messages[3].Content != "Use the corrected scope." {
		t.Fatalf("refreshed messages = %+v", request.Messages)
	}
	if last := request.Messages[len(request.Messages)-1]; last.Role != "system" || !strings.Contains(last.Content, "input_sequence=2") {
		t.Fatalf("refreshed current step = %+v", last)
	}
	if err := validateModelTranscript(request.Messages); err != nil {
		t.Fatalf("refreshed task transcript is invalid: %v", err)
	}
}

func TestConversationUpdateIsEvidenceNotCurrentTaskAmendment(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("A later task already confirmed the external name."), content.Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	capsule := execution.TaskCapsule{TaskID: "older-task", SourceInteractionID: "older-input", SourceKind: "text", SourceRole: "user", TriggerChannel: "lark"}
	repository := &contextTestRepository{updates: []ContextRecord{{
		ID: "later-delivery", TaskID: "later-task", DeliveryID: "delivery-exact", Kind: "text",
		Sequence: 9, Role: "assistant", ContentRef: ref,
	}}}
	service := &Service{content: contentStore, logger: slog.Default()}
	service.repository = contextRepositoryAdapter{contextTestRepository: repository}
	request := ModelRequest{Messages: currentTaskMessages(capsule, "Continue the older task.", 3)}
	state := loopState{
		TaskText: "Continue the older task.", InputSequence: 3, ConversationSequence: 4,
		Capabilities:    ModelCapabilities{ContextTokens: 8_000},
		ContextManifest: execution.ContextManifest{CurrentTask: &capsule, ConversationSequence: 4},
	}
	changed, err := service.refreshConversationUpdates(context.Background(), TaskContext{
		TaskID: "older-task", RunID: "run", InvocationID: "invocation", CurrentTask: capsule,
	}, &request, &state)
	if err != nil || !changed {
		t.Fatalf("refresh changed=%t err=%v", changed, err)
	}
	joined := ""
	for _, message := range request.Messages {
		joined += message.Content
	}
	for _, expected := range []string{"<conversation_update>", "not amendments", `delivery_id="delivery-exact"`, "Resume only the current Task"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Conversation update is missing %q: %s", expected, joined)
		}
	}
	if state.TaskText != "Continue the older task." || state.InputSequence != 3 || state.ConversationSequence != 9 {
		t.Fatalf("state after Conversation update=%+v", state)
	}
	if last := request.Messages[len(request.Messages)-1]; !strings.HasPrefix(last.Content, "<current_step>") {
		t.Fatalf("current step is not final: %+v", last)
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

func (r contextRepositoryAdapter) UpdateInvocationContext(_ context.Context, _ string, manifest string) error {
	r.manifest = manifest
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
func (r contextRepositoryAdapter) LoadTaskInputsAfter(context.Context, string, int64) ([]ContextRecord, error) {
	return append([]ContextRecord(nil), r.inputs...), nil
}
func (r contextRepositoryAdapter) LoadConversationUpdatesAfter(context.Context, string, int64) ([]ContextRecord, error) {
	return append([]ContextRecord(nil), r.updates...), nil
}
