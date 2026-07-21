package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/identity"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestAgentLoopContinuesPastFourNativeToolCallingTurns(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &loopTestModel{toolTurns: 5}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", loopTestToolGateway{}, nil, LoopConfig{
		MaxEvalAttempts: 3,
		MaxOutputTokens: 1024,
		Judge:           loopTestJudge{},
	})
	request := ModelRequest{
		Messages: []Message{{Role: "user", Content: "research until complete"}},
		Tools: []ToolDefinition{{
			Name: "lookup", Description: "lookup evidence", Parameters: map[string]any{"type": "object"},
		}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}
	state := loopState{TaskText: "research until complete", Trace: runTrace{}}

	if err := service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "lookup"}, state); err != nil {
		t.Fatal(err)
	}
	if model.calls != 6 {
		t.Fatalf("model calls = %d, want five tool turns plus one natural final response", model.calls)
	}
	if repository.commit.TerminalStatus != "completed" || repository.commit.FailureCode != "" {
		t.Fatalf("commit = %+v", repository.commit)
	}
	if repository.commit.Usage.ModelCalls != 7 {
		t.Fatalf("accounted model calls = %d, want six agent calls plus one Judge call", repository.commit.Usage.ModelCalls)
	}
	traceBody, err := contentStore.Get(context.Background(), repository.commit.TraceRef)
	if err != nil {
		t.Fatal(err)
	}
	var trace runTrace
	if err := json.Unmarshal(traceBody, &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.ModelTurns) != 6 {
		t.Fatalf("trace turns = %d, want 6", len(trace.ModelTurns))
	}
	for index, turn := range trace.ModelTurns {
		wantID := fmt.Sprintf("invocation:turn:%d", index+1)
		if turn.ID != wantID || turn.Ordinal != index+1 {
			t.Fatalf("turn %d identity = %+v, want %q", index+1, turn, wantID)
		}
		wantTrigger := "tool_observations"
		if index == 0 {
			wantTrigger = "initial_request"
		}
		if turn.Trigger != wantTrigger {
			t.Fatalf("turn %d trigger = %q, want %q", index+1, turn.Trigger, wantTrigger)
		}
	}
	for index, call := range trace.ToolCalls {
		wantTurnID := fmt.Sprintf("invocation:turn:%d", index+1)
		if call.ModelTurnID != wantTurnID {
			t.Fatalf("tool call %d model turn = %q, want %q", index+1, call.ModelTurnID, wantTurnID)
		}
	}
	if len(trace.Evaluations) != 1 || trace.Evaluations[0].ModelTurnID != "invocation:turn:6" || trace.Evaluations[0].Attempt != 1 {
		t.Fatalf("evaluation trace = %+v", trace.Evaluations)
	}
}

func TestAgentLoopAdmitsNewUserInputWithoutCancelingInflightModelCall(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("Use Saturday instead, and avoid early flights."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &interruptibleLoopModel{repository: repository, joinedInput: ContextRecord{
		ID: "input-2", Kind: "text", Sequence: 2, Role: "user", ContentRef: ref,
	}}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", nil, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1}
	state := loopState{TaskText: "Plan the trip for Friday.", InputSequence: 1, Trace: runTrace{}}
	request := ModelRequest{Messages: []Message{{Role: "user", Content: "Plan the trip for Friday."}}, MaxOutputTokens: 1024}

	if err := service.continueLoop(context.Background(), task, request, nil, state); err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want the completed stale call plus one resumed turn", model.calls)
	}
	if len(model.requests) != 2 || len(model.requests[1].Messages) != 2 {
		t.Fatalf("resumed request = %+v", model.requests)
	}
	if got := model.requests[1].Messages[1]; got.Role != "user" || got.Content != "Use Saturday instead, and avoid early flights." {
		t.Fatalf("joined input = %+v", got)
	}
	for _, message := range model.requests[1].Messages {
		if message.Content == "Friday plan ready." {
			t.Fatal("superseded assistant result leaked into the resumed model context")
		}
	}
	if repository.commit.BasisInputSequence != 2 || repository.commit.TerminalStatus != "completed" {
		t.Fatalf("commit = %+v", repository.commit)
	}
	traceBody, err := contentStore.Get(context.Background(), repository.commit.TraceRef)
	if err != nil {
		t.Fatal(err)
	}
	var trace runTrace
	if err := json.Unmarshal(traceBody, &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.ModelTurns) != 2 || trace.ModelTurns[0].Status != "superseded" || trace.ModelTurns[1].Trigger != "user_input" {
		t.Fatalf("model turns = %+v", trace.ModelTurns)
	}
}

func TestAgentLoopSupersedesCandidateWhenAnotherTaskAdvancesConversation(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("A later task corrected the shared factual premise."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &interruptibleLoopModel{repository: repository, conversationUpdate: ContextRecord{
		ID: "other-task-input", TaskID: "other-task", Kind: "text", Sequence: 2, Role: "user", ContentRef: ref,
	}}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", nil, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	capsule := execution.TaskCapsule{TaskID: "task", SourceInteractionID: "input-1", SourceKind: "text", SourceRole: "user"}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1, CurrentTask: capsule}
	state := loopState{
		TaskText: "Finish the earlier task.", InputSequence: 1, ConversationSequence: 1, Trace: runTrace{},
		ContextManifest: execution.ContextManifest{CurrentTask: &capsule, ConversationSequence: 1},
	}
	request := ModelRequest{Messages: []Message{{Role: "user", Content: "Finish the earlier task."}}, MaxOutputTokens: 1024}

	if err := service.continueLoop(context.Background(), task, request, nil, state); err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls=%d, want stale candidate plus reconciled turn", model.calls)
	}
	joined := ""
	for _, message := range model.requests[1].Messages {
		joined += message.Content
	}
	if !strings.Contains(joined, "<conversation_update>") || !strings.Contains(joined, "not amendments") || strings.Contains(joined, "Friday plan ready.") {
		t.Fatalf("reconciled request=%+v", model.requests[1].Messages)
	}
	if repository.commit.BasisConversationSequence != 2 {
		t.Fatalf("commit Conversation basis=%d", repository.commit.BasisConversationSequence)
	}
}

func TestAgentLoopDropsUnstartedToolFrameWhenNewInputArrivesAfterModelReturn(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("Also tell me what to wear."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &multiToolInterruptModel{onFirstResponse: func() {
		repository.inputs = []ContextRecord{{ID: "input-2", Kind: "text", Sequence: 2, Role: "user", ContentRef: ref}}
	}}
	gateway := &joiningToolGateway{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", gateway, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "Check the weather."}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1}
	state := loopState{TaskText: "Check the weather.", InputSequence: 1, Trace: runTrace{}}

	if err := service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "builtin.lookup"}, state); err != nil {
		t.Fatal(err)
	}
	if len(gateway.calls) != 0 {
		t.Fatalf("stale tools executed: %v", gateway.calls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want stale tool decision plus resumed turn", len(model.requests))
	}
	resumed := model.requests[1].Messages
	if len(resumed) != 2 || resumed[0].Role != "user" || resumed[1].Role != "user" || resumed[1].Content != "Also tell me what to wear." {
		t.Fatalf("resumed transcript = %+v", resumed)
	}
	if err := validateModelTranscript(resumed); err != nil {
		t.Fatalf("resumed transcript is invalid: %v", err)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if trace.ModelTurns[0].Status != "superseded" || trace.ModelTurns[1].Trigger != "user_input" {
		t.Fatalf("turns = %+v", trace.ModelTurns)
	}
}

func TestAgentLoopClosesPartialToolFrameBeforeAdmittingNewInput(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("Also tell me what to wear."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &multiToolInterruptModel{}
	gateway := &joiningToolGateway{
		repository: repository, joinAfter: 1,
		joinedInput: ContextRecord{ID: "input-2", Kind: "text", Sequence: 2, Role: "user", ContentRef: ref},
	}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", gateway, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "Check two weather sources."}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1}
	state := loopState{TaskText: "Check two weather sources.", InputSequence: 1, Trace: runTrace{}}

	if err := service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "builtin.lookup"}, state); err != nil {
		t.Fatal(err)
	}
	if got := gateway.calls; len(got) != 1 || got[0] != "call-1" {
		t.Fatalf("executed calls = %v, want only call-1", got)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want interrupted tool turn plus resumed turn", len(model.requests))
	}
	resumed := model.requests[1].Messages
	if err := validateModelTranscript(resumed); err != nil {
		t.Fatalf("resumed transcript is invalid: %v\n%+v", err, resumed)
	}
	if len(resumed) != 5 || resumed[1].Role != "assistant" || len(resumed[1].ToolCalls) != 2 ||
		resumed[2].Role != "tool" || resumed[2].ToolCallID != "call-1" ||
		resumed[3].Role != "tool" || resumed[3].ToolCallID != "call-2" ||
		resumed[4].Role != "user" || resumed[4].Content != "Also tell me what to wear." {
		t.Fatalf("resumed transcript order = %+v", resumed)
	}
	var skipped map[string]any
	if err := json.Unmarshal([]byte(resumed[3].Content), &skipped); err != nil || skipped["status"] != "superseded_before_execution" {
		t.Fatalf("skipped observation = %q err=%v", resumed[3].Content, err)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if trace.ModelTurns[0].Status != "succeeded" || trace.ModelTurns[1].Trigger != "user_input" {
		t.Fatalf("turns = %+v", trace.ModelTurns)
	}
	if len(trace.ToolCalls) != 2 || trace.ToolCalls[0].Status != string(tool.IntentConfirmed) || trace.ToolCalls[1].Status != "superseded_before_execution" {
		t.Fatalf("tool trace = %+v", trace.ToolCalls)
	}
}

func TestAgentLoopCarriesSoulGuidedProfileIntoJudge(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &loopTestModel{}
	judge := &interpersonalCapturingJudge{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", loopTestToolGateway{}, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: judge,
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "It is finally fixed"}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup evidence", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	state := loopState{
		TaskText: "It is finally fixed", Trace: runTrace{},
	}

	if err := service.continueLoop(context.Background(), TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}, request, map[string]string{"lookup": "lookup"}, state); err != nil {
		t.Fatal(err)
	}
	if !judge.called {
		t.Fatal("Judge was not called")
	}
	if !judge.request.SoulGuidedResponse {
		t.Fatalf("Judge did not receive the Run's interpersonal response profile: %+v", judge.request)
	}
}

func TestAgentLoopRecoversFromModelCheckpointWithoutRepeatingConfirmedEffect(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{failCheckpointAt: 3}
	model := &loopTestModel{toolTurns: 1}
	gateway := &replayAwareToolGateway{seen: make(map[string]bool)}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", gateway, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "perform one effect and finish"}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup evidence", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}
	state := loopState{TaskText: "perform one effect and finish", Trace: runTrace{}}

	err = service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "lookup"}, state)
	if err == nil {
		t.Fatal("expected simulated crash after the first confirmed tool effect")
	}
	if repository.checkpointPhase != "model_received" || repository.checkpointRef.ObjectID == "" {
		t.Fatalf("durable checkpoint = %q %+v", repository.checkpointPhase, repository.checkpointRef)
	}
	if gateway.actualEffects != 1 {
		t.Fatalf("actual effects before recovery = %d, want 1", gateway.actualEffects)
	}

	repository.failCheckpointAt = 0
	recovered := task
	recovered.CheckpointPhase = repository.checkpointPhase
	recovered.CheckpointRef = repository.checkpointRef
	if err := service.resumeAgentCheckpoint(context.Background(), recovered, map[string]string{"lookup": "lookup"}); err != nil {
		t.Fatal(err)
	}
	if gateway.actualEffects != 1 || gateway.replays != 1 {
		t.Fatalf("actual effects=%d replays=%d, want one real effect and one idempotent replay", gateway.actualEffects, gateway.replays)
	}
	if len(model.requests) != 2 || len(model.requests[1].Messages) < 2 || model.requests[1].Messages[1].ReasoningContent != "reasoning-1" {
		t.Fatalf("recovered model request lost reasoning_content: %+v", model.requests)
	}
	if repository.commit.TerminalStatus != "completed" {
		t.Fatalf("commit = %+v", repository.commit)
	}
	if repository.commit.Usage.ModelCalls != 3 {
		t.Fatalf("accounted model calls after recovery = %d, want two agent calls plus one Judge call", repository.commit.Usage.ModelCalls)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if len(trace.ModelTurns) == 0 || trace.ModelTurns[0].Message.ReasoningContent != "" {
		t.Fatalf("safe final trace retained reasoning_content: %+v", trace.ModelTurns)
	}
	if trace.ProviderTranscript == nil || len(trace.ProviderTranscript.Messages) < 2 || trace.ProviderTranscript.Messages[1].ReasoningContent != "reasoning-1" {
		t.Fatalf("encrypted provider transcript lost reasoning_content: %+v", trace.ProviderTranscript)
	}
}

func TestTaskCancellationRetainsEncryptedProviderTranscript(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{cancelRequested: true}
	service := &Service{repository: repository, content: contentStore}
	request := ModelRequest{Messages: []Message{
		{Role: "user", Content: "check one fact"},
		{Role: "assistant", ReasoningContent: "reasoning-before-cancel", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
	}}
	canceled, err := service.cancelIfRequested(context.Background(), TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}, request, &loopState{Trace: runTrace{}})
	if err != nil || !canceled {
		t.Fatalf("cancel result=%v err=%v", canceled, err)
	}
	body, err := contentStore.Get(context.Background(), repository.cancellationTraceRef)
	if err != nil {
		t.Fatal(err)
	}
	var trace runTrace
	if err := json.Unmarshal(body, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.ProviderTranscript == nil || len(trace.ProviderTranscript.Messages) != 2 || trace.ProviderTranscript.Messages[1].ReasoningContent != "reasoning-before-cancel" {
		t.Fatalf("canceled Run lost provider transcript: %+v", trace.ProviderTranscript)
	}
}

func TestTerminalFailureRetainsEncryptedProviderTranscript(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	service := NewService(repository, contentStore, failingLoopModel{}, identity.Snapshot{}, "test-owner", loopTestToolGateway{}, nil, LoopConfig{MaxOutputTokens: 1024})
	request := ModelRequest{
		Messages: []Message{
			{Role: "user", Content: "continue from the observation"},
			{Role: "assistant", ReasoningContent: "reasoning-before-failure", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "call-1", Content: `{"found":true}`},
		},
		Tools:           []ToolDefinition{{Name: "lookup", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	err = service.continueLoop(context.Background(), TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}, request, map[string]string{"lookup": "lookup"}, loopState{})
	if err != nil {
		t.Fatal(err)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if repository.commit.TerminalStatus != "failed" || trace.ProviderTranscript == nil || len(trace.ProviderTranscript.Messages) != 3 || trace.ProviderTranscript.Messages[1].ReasoningContent != "reasoning-before-failure" {
		t.Fatalf("failed Run did not retain provider transcript: commit=%+v transcript=%+v", repository.commit, trace.ProviderTranscript)
	}
}

func TestAgentLoopRecoveryReplaysCompletedToolThenClosesRemainingFrameForNewInput(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := contentStore.Put(context.Background(), []byte("Use the new constraint instead."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{failCheckpointAt: 3}
	model := &multiToolInterruptModel{}
	gateway := &staleAwareReplayGateway{repository: repository, seen: make(map[string]tool.Outcome)}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", gateway, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "Run two checks."}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1}
	state := loopState{TaskText: "Run two checks.", InputSequence: 1, Trace: runTrace{}}

	err = service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "builtin.lookup"}, state)
	if err == nil {
		t.Fatal("expected simulated crash after the first confirmed tool effect")
	}
	if repository.checkpointPhase != "model_received" || gateway.actualEffects != 1 {
		t.Fatalf("checkpoint=%q actual effects=%d", repository.checkpointPhase, gateway.actualEffects)
	}
	repository.inputs = []ContextRecord{{ID: "input-2", Kind: "text", Sequence: 2, Role: "user", ContentRef: ref}}
	repository.failCheckpointAt = 0
	recovered := task
	recovered.CheckpointPhase = repository.checkpointPhase
	recovered.CheckpointRef = repository.checkpointRef
	if err := service.resumeAgentCheckpoint(context.Background(), recovered, map[string]string{"lookup": "builtin.lookup"}); err != nil {
		t.Fatal(err)
	}
	if gateway.actualEffects != 1 || gateway.replays != 1 || len(gateway.invoked) != 2 || gateway.invoked[0] != "call-1" || gateway.invoked[1] != "call-1" {
		t.Fatalf("actual=%d replays=%d calls=%v", gateway.actualEffects, gateway.replays, gateway.invoked)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d", len(model.requests))
	}
	resumed := model.requests[1].Messages
	if err := validateModelTranscript(resumed); err != nil {
		t.Fatalf("recovered transcript is invalid: %v\n%+v", err, resumed)
	}
	if len(resumed) != 5 || resumed[2].ToolCallID != "call-1" || resumed[3].ToolCallID != "call-2" || resumed[4].Role != "user" {
		t.Fatalf("recovered transcript = %+v", resumed)
	}
	var skipped map[string]any
	if err := json.Unmarshal([]byte(resumed[3].Content), &skipped); err != nil || skipped["status"] != "superseded_before_execution" {
		t.Fatalf("skipped recovery observation = %q err=%v", resumed[3].Content, err)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if len(trace.ToolCalls) != 2 || trace.ToolCalls[0].Status != string(tool.IntentConfirmed) || trace.ToolCalls[1].Status != "superseded_before_execution" {
		t.Fatalf("recovered tool trace = %+v", trace.ToolCalls)
	}
}

func TestApprovalResumeClosesDeniedAndSkippedToolCallsBeforeNewInput(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	joinedRef, err := contentStore.Put(context.Background(), []byte("Do not do the second action either."), content.Metadata{
		MediaType: "text/plain", EncryptionDomain: "conversation", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := []ToolCall{
		{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"source":1}`)},
		{ID: "call-2", Name: "lookup", Arguments: json.RawMessage(`{"source":2}`)},
	}
	continuation := pendingContinuation{
		Request: ModelRequest{
			Messages:        []Message{{Role: "user", Content: "Run two actions."}, {Role: "assistant", ToolCalls: calls}},
			Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: map[string]any{"type": "object"}}},
			MaxOutputTokens: 1024,
		},
		ModelToolIDs: map[string]string{"lookup": "builtin.lookup"}, PendingCalls: calls,
		State: loopState{
			Trace: runTrace{ModelTurns: []modelTurnTrace{{
				ID: "invocation:turn:1", Ordinal: 1, Trigger: "initial_request", Status: "succeeded", InputSequence: 1,
				Message: Message{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls",
			}}},
			TaskText: "Run two actions.", InputSequence: 1, TurnsUsed: 1,
		},
	}
	body, err := json.Marshal(continuation)
	if err != nil {
		t.Fatal(err)
	}
	continuationRef, err := contentStore.Put(context.Background(), body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "runtime", PrivacyClass: "private", RetentionPolicy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation", InputSequence: 1}
	repository := &loopTestRepository{
		inputs:          []ContextRecord{{ID: "input-2", Kind: "text", Sequence: 2, Role: "user", ContentRef: joinedRef}},
		approvalClaimed: true,
		approvalResume: ApprovalResume{
			Task: task, ApprovalID: "approval-1", Decision: "denied", ContinuationRef: continuationRef,
		},
	}
	model := &multiToolInterruptModel{calls: 1}
	gateway := &joiningToolGateway{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", gateway, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	if err := service.HandleApprovalResume(context.Background(), runtime.OutboxItem{AggregateID: "approval-1"}); err != nil {
		t.Fatal(err)
	}
	if len(gateway.calls) != 0 {
		t.Fatalf("tool calls executed after denial and newer input: %v", gateway.calls)
	}
	if len(model.requests) != 1 {
		t.Fatalf("resumed model requests = %d", len(model.requests))
	}
	resumed := model.requests[0].Messages
	if err := validateModelTranscript(resumed); err != nil {
		t.Fatalf("approval-resumed transcript is invalid: %v\n%+v", err, resumed)
	}
	if len(resumed) != 5 || resumed[2].ToolCallID != "call-1" || resumed[3].ToolCallID != "call-2" || resumed[4].Role != "user" {
		t.Fatalf("approval-resumed transcript = %+v", resumed)
	}
	trace := readCommittedLoopTrace(t, contentStore, repository)
	if len(trace.ToolCalls) != 2 || trace.ToolCalls[0].Status != "user_denied" || trace.ToolCalls[1].Status != "superseded_before_execution" {
		t.Fatalf("approval tool trace = %+v", trace.ToolCalls)
	}
}

func TestAgentLoopForcesOneEvidenceOnlySynthesisAfterVerifiedNoProgress(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &stagnantLoopModel{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", loopTestToolGateway{}, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "find new evidence"}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup evidence", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}
	if err := service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "lookup"}, loopState{TaskText: "find new evidence", Trace: runTrace{}}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 5 {
		t.Fatalf("model calls = %d, want four repeated tool turns plus one synthesis turn", model.calls)
	}
	if repository.commit.FailureCode != "" || repository.commit.TerminalStatus != "completed" {
		t.Fatalf("commit = %+v", repository.commit)
	}
	if repository.commit.ArtifactKind != "text" || repository.commit.EvalEvaluator != "llm_judge" {
		t.Fatalf("no-progress synthesis was not evaluated as an ordinary reply: %+v", repository.commit)
	}
	if repository.commit.Usage.ModelCalls != 6 {
		t.Fatalf("accounted model calls = %d, want five agent calls plus one Judge call", repository.commit.Usage.ModelCalls)
	}
}

func TestAgentLoopDeliversEvaluatedProgressWithoutEndingTask(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &progressLoopModel{}
	judge := &progressCapturingJudge{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", loopTestToolGateway{}, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: judge,
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "compare several options"}},
		Tools:           []ToolDefinition{{Name: "lookup", Description: "lookup evidence", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}
	if err := service.continueLoop(context.Background(), task, request, map[string]string{"lookup": "lookup"}, loopState{TaskText: "compare several options", Trace: runTrace{}}); err != nil {
		t.Fatal(err)
	}
	if len(repository.progress) != 1 {
		t.Fatalf("progress commits = %d, want 1", len(repository.progress))
	}
	progress := repository.progress[0]
	if progress.Commit.ArtifactKind != "progress" || progress.ModelTurnID != "invocation:turn:1" || progress.Commit.EvalEvaluator != "llm_judge_progress" {
		t.Fatalf("progress commit = %+v", progress)
	}
	body, err := contentStore.Get(context.Background(), progress.Commit.ArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "I found the first reliable source. I’m checking the remaining options before I compare them." {
		t.Fatalf("progress body = %q", body)
	}
	if repository.commit.TerminalStatus != "completed" || repository.commit.ArtifactKind != "text" {
		t.Fatalf("final commit = %+v", repository.commit)
	}
	if len(judge.requests) != 2 || judge.requests[0].Purpose != "progress" || judge.requests[1].Purpose != "" {
		t.Fatalf("Judge requests = %+v", judge.requests)
	}
	progressMessages := judge.requests[0].Messages
	if err := validateModelTranscript(progressMessages); err != nil {
		t.Fatalf("progress Judge received an invalid transcript: %v\n%+v", err, progressMessages)
	}
	if len(progressMessages) < 4 || progressMessages[len(progressMessages)-2].Role != "tool" || progressMessages[len(progressMessages)-2].ToolCallID == "" {
		t.Fatalf("progress Judge lost the final Tool observation: %+v", progressMessages)
	}
	if candidate := progressMessages[len(progressMessages)-1]; candidate.Role != "assistant" || candidate.Content != string(body) {
		t.Fatalf("progress Judge candidate = %+v, want delivered body %q", candidate, body)
	}
}

func TestAgentLoopKeepsProviderToolProtocolValidWhileSynthesizingDeferredProgress(t *testing.T) {
	contentStore, err := content.New(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository := &loopTestRepository{}
	model := &deferredProgressModel{}
	service := NewService(repository, contentStore, model, identity.Snapshot{}, "test-owner", deferredToolGateway{}, nil, LoopConfig{
		MaxEvalAttempts: 3, MaxOutputTokens: 1024, Judge: loopTestJudge{},
	})
	request := ModelRequest{
		Messages:        []Message{{Role: "user", Content: "Ask the engineering team to investigate."}},
		Tools:           []ToolDefinition{{Name: "delegate", Description: "delegate bounded work", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1024,
	}
	task := TaskContext{TaskID: "task", RunID: "run", InvocationID: "invocation"}
	if err := service.continueLoop(context.Background(), task, request, map[string]string{"delegate": "builtin.delegate"}, loopState{TaskText: "delegate investigation", Trace: runTrace{}}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want delegation turn plus progress synthesis", model.calls)
	}
	if len(repository.progress) != 0 {
		t.Fatalf("pre-effect progress deliveries = %d, want none", len(repository.progress))
	}
	if repository.subagentWait.DelegationID != "delegation-1" || repository.subagentWait.EvalResult != eval.Pass {
		t.Fatalf("subagent wait = %+v", repository.subagentWait)
	}
}

type loopTestModel struct {
	toolTurns int
	calls     int
	requests  []ModelRequest
}

type failingLoopModel struct{}

func (failingLoopModel) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, fmt.Errorf("provider unavailable")
}

func (failingLoopModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return ModelCapabilities{Text: true, ToolCalling: true, ContextTokens: 32_768, MaxOutputTokens: 4_096}, nil
}

type stagnantLoopModel struct{ calls int }

type progressLoopModel struct{ calls int }

type deferredProgressModel struct{ calls int }

type interruptibleLoopModel struct {
	repository         *loopTestRepository
	joinedInput        ContextRecord
	conversationUpdate ContextRecord
	calls              int
	requests           []ModelRequest
}

type multiToolInterruptModel struct {
	calls           int
	requests        []ModelRequest
	onFirstResponse func()
}

type joiningToolGateway struct {
	repository  *loopTestRepository
	joinedInput ContextRecord
	joinAfter   int
	calls       []string
}

type staleAwareReplayGateway struct {
	repository    *loopTestRepository
	seen          map[string]tool.Outcome
	invoked       []string
	actualEffects int
	replays       int
}

func (*loopTestModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (*stagnantLoopModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (*progressLoopModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (*deferredProgressModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (m *deferredProgressModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.calls++
	if len(request.Tools) != 1 {
		return ModelResponse{}, fmt.Errorf("tool definitions disappeared from deferred transcript")
	}
	if err := validateModelTranscript(request.Messages); err != nil {
		return ModelResponse{}, err
	}
	usage := Usage{Provider: "test", Model: "test", ModelCalls: 1}
	if m.calls == 1 {
		return ModelResponse{Message: Message{
			Content:   "I’m handing this to the engineering team now.",
			ToolCalls: []ToolCall{{ID: "delegate-call", Name: "delegate", Arguments: json.RawMessage(`{"role":"engineering_team"}`)}},
		}, FinishReason: "tool_calls", Usage: usage}, nil
	}
	return ModelResponse{Message: Message{Content: "The engineering team has taken this on. I will return with the conclusion after I review its result."}, FinishReason: "stop", Usage: usage}, nil
}

func (*interruptibleLoopModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (*multiToolInterruptModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func (m *interruptibleLoopModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.calls++
	m.requests = append(m.requests, request)
	if m.calls == 1 {
		if m.joinedInput.ID != "" {
			m.repository.inputs = []ContextRecord{m.joinedInput}
		}
		if m.conversationUpdate.ID != "" {
			m.repository.conversationUpdates = []ContextRecord{m.conversationUpdate}
		}
		return ModelResponse{Message: Message{Content: "Friday plan ready."}, FinishReason: "stop", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1}}, nil
	}
	return ModelResponse{Message: Message{Content: "Saturday plan ready."}, FinishReason: "stop", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1}}, nil
}

func (m *multiToolInterruptModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	if err := validateModelTranscript(request.Messages); err != nil {
		return ModelResponse{}, err
	}
	m.calls++
	m.requests = append(m.requests, cloneModelRequest(request))
	if m.calls == 1 {
		if m.onFirstResponse != nil {
			m.onFirstResponse()
		}
		return ModelResponse{
			Message: Message{ToolCalls: []ToolCall{
				{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"source":1}`)},
				{ID: "call-2", Name: "lookup", Arguments: json.RawMessage(`{"source":2}`)},
			}},
			FinishReason: "tool_calls", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1},
		}, nil
	}
	return ModelResponse{Message: Message{Content: "The weather and clothing advice are ready."}, FinishReason: "stop", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1}}, nil
}

func (*joiningToolGateway) Descriptors() []tool.Descriptor { return nil }

func (g *joiningToolGateway) Invoke(_ context.Context, request tool.Request) (tool.Outcome, error) {
	g.calls = append(g.calls, request.ToolCallID)
	if g.repository != nil && g.joinAfter > 0 && len(g.calls) == g.joinAfter {
		g.repository.inputs = []ContextRecord{g.joinedInput}
	}
	return tool.Outcome{
		Intent: tool.Intent{ID: "intent-" + request.ToolCallID, ToolID: request.ToolID, Status: tool.IntentConfirmed},
		Result: tool.Result{Output: json.RawMessage(`{"found":true}`), Source: "test", Receipt: "receipt-" + request.ToolCallID},
	}, nil
}

func (*staleAwareReplayGateway) Descriptors() []tool.Descriptor { return nil }

func (g *staleAwareReplayGateway) Invoke(_ context.Context, request tool.Request) (tool.Outcome, error) {
	g.invoked = append(g.invoked, request.ToolCallID)
	key := request.RunID + ":" + request.ToolID + ":" + string(request.Input)
	if outcome, ok := g.seen[key]; ok {
		g.replays++
		outcome.Replayed = true
		return outcome, nil
	}
	for _, input := range g.repository.inputs {
		if input.Sequence > request.BasisInputSequence {
			return tool.Outcome{}, tool.ErrStaleTaskInput
		}
	}
	g.actualEffects++
	outcome := tool.Outcome{
		Intent: tool.Intent{ID: "intent-" + request.ToolCallID, ToolID: request.ToolID, Status: tool.IntentConfirmed},
		Result: tool.Result{Output: json.RawMessage(`{"found":true}`), Source: "test", Receipt: "receipt-" + request.ToolCallID},
	}
	g.seen[key] = outcome
	return outcome, nil
}

func cloneModelRequest(request ModelRequest) ModelRequest {
	body, _ := json.Marshal(request)
	var clone ModelRequest
	_ = json.Unmarshal(body, &clone)
	return clone
}

func readCommittedLoopTrace(t *testing.T, contentStore *content.Store, repository *loopTestRepository) runTrace {
	t.Helper()
	body, err := contentStore.Get(context.Background(), repository.commit.TraceRef)
	if err != nil {
		t.Fatal(err)
	}
	var trace runTrace
	if err := json.Unmarshal(body, &trace); err != nil {
		t.Fatal(err)
	}
	return trace
}

func (m *progressLoopModel) Complete(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		return ModelResponse{
			Message: Message{
				Content:   "I found the first reliable source. I’m checking the remaining options before I compare them.",
				ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"query":"remaining options"}`)}},
			},
			FinishReason: "tool_calls", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1},
		}, nil
	}
	return ModelResponse{Message: Message{Content: "The comparison is complete."}, FinishReason: "stop", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1}}, nil
}

func (m *stagnantLoopModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.calls++
	if len(request.Tools) == 0 {
		return ModelResponse{
			Message:      Message{Content: "I could not find anything newer after repeated checks. Here is the confirmed evidence I do have."},
			FinishReason: "stop", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1},
		}, nil
	}
	return ModelResponse{
		Message: Message{ToolCalls: []ToolCall{{
			ID: fmt.Sprintf("repeated-%d", m.calls), Name: "lookup", Arguments: json.RawMessage(`{"query":"same"}`),
		}}},
		FinishReason: "tool_calls", Usage: Usage{Provider: "test", Model: "test", ModelCalls: 1},
	}, nil
}

func (m *loopTestModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.calls++
	m.requests = append(m.requests, cloneModelRequest(request))
	if len(request.Tools) != 1 {
		return ModelResponse{}, fmt.Errorf("tools disappeared on model call %d", m.calls)
	}
	usage := Usage{Provider: "test", Model: "test", ModelCalls: 1}
	if m.calls <= m.toolTurns {
		arguments, _ := json.Marshal(map[string]int{"turn": m.calls})
		return ModelResponse{
			Message:      Message{ReasoningContent: fmt.Sprintf("reasoning-%d", m.calls), ToolCalls: []ToolCall{{ID: fmt.Sprintf("call-%d", m.calls), Name: "lookup", Arguments: arguments}}},
			FinishReason: "tool_calls", Usage: usage,
		}, nil
	}
	return ModelResponse{Message: Message{Content: "completed after six model calls"}, FinishReason: "stop", Usage: usage}, nil
}

type loopTestToolGateway struct{}

type deferredToolGateway struct{}

func (loopTestToolGateway) Descriptors() []tool.Descriptor { return nil }

func (loopTestToolGateway) Invoke(_ context.Context, request tool.Request) (tool.Outcome, error) {
	return tool.Outcome{
		Intent: tool.Intent{ID: "intent-" + request.ToolID, ToolID: request.ToolID, Status: tool.IntentConfirmed},
		Result: tool.Result{Output: json.RawMessage(`{"found":true}`), Source: "test", Receipt: "receipt"},
	}, nil
}

func (deferredToolGateway) Descriptors() []tool.Descriptor { return nil }

func (deferredToolGateway) Invoke(_ context.Context, request tool.Request) (tool.Outcome, error) {
	return tool.Outcome{
		Intent: tool.Intent{ID: "intent-delegate", ToolID: request.ToolID, Status: tool.IntentConfirmed},
		Result: tool.Result{
			Output: json.RawMessage(`{"status":"queued"}`), Receipt: "queued-receipt",
			Deferred: &tool.Deferred{ID: "delegation-1", Kind: "subagent", Type: "engineering_team", ProviderID: "codex"},
		},
	}, nil
}

type replayAwareToolGateway struct {
	seen          map[string]bool
	actualEffects int
	replays       int
}

func (*replayAwareToolGateway) Descriptors() []tool.Descriptor { return nil }

func (g *replayAwareToolGateway) Invoke(_ context.Context, request tool.Request) (tool.Outcome, error) {
	key := request.RunID + ":" + request.ToolID + ":" + string(request.Input)
	replayed := g.seen[key]
	if replayed {
		g.replays++
	} else {
		g.seen[key] = true
		g.actualEffects++
	}
	return tool.Outcome{
		Intent:   tool.Intent{ID: "intent-" + request.ToolID, ToolID: request.ToolID, Status: tool.IntentConfirmed},
		Result:   tool.Result{Output: json.RawMessage(`{"found":true}`), Source: "test", Receipt: "receipt"},
		Replayed: replayed,
	}, nil
}

type loopTestJudge struct{}

func (loopTestJudge) Evaluate(context.Context, JudgeRequest) (eval.Decision, Usage, error) {
	return eval.Decision{Result: eval.Pass, Tier: "routine"}, Usage{}, nil
}

type progressCapturingJudge struct{ requests []JudgeRequest }

func (j *progressCapturingJudge) Evaluate(_ context.Context, request JudgeRequest) (eval.Decision, Usage, error) {
	j.requests = append(j.requests, request)
	return eval.Decision{Result: eval.Pass, Tier: "routine"}, Usage{}, nil
}

type interpersonalCapturingJudge struct {
	called  bool
	request JudgeRequest
}

func (j *interpersonalCapturingJudge) Evaluate(_ context.Context, request JudgeRequest) (eval.Decision, Usage, error) {
	j.called = true
	j.request = request
	return eval.Decision{Result: eval.Pass, Tier: "routine"}, Usage{}, nil
}

type loopTestRepository struct {
	commit               Commit
	progress             []ProgressCommit
	checkpointRef        content.Ref
	checkpointPhase      string
	checkpointSaves      int
	failCheckpointAt     int
	inputs               []ContextRecord
	conversationUpdates  []ContextRecord
	approvalResume       ApprovalResume
	approvalClaimed      bool
	subagentWait         SubagentWaitCommit
	cancelRequested      bool
	cancellationTraceRef content.Ref
}

func (r *loopTestRepository) ClaimTask(context.Context, string, string, time.Duration, string, string, string) (TaskContext, bool, error) {
	return TaskContext{}, false, nil
}
func (r *loopTestRepository) MarkInvocationDispatched(context.Context, string) error { return nil }
func (r *loopTestRepository) CommitArtifact(_ context.Context, commit Commit) error {
	r.commit = commit
	return nil
}
func (r *loopTestRepository) CommitProgress(_ context.Context, commit ProgressCommit) (bool, error) {
	r.progress = append(r.progress, commit)
	return true, nil
}
func (r *loopTestRepository) PauseForApproval(context.Context, ApprovalCommit) error { return nil }
func (r *loopTestRepository) ClaimApprovalResume(context.Context, string, string, time.Duration) (ApprovalResume, bool, error) {
	return r.approvalResume, r.approvalClaimed, nil
}

func (r *loopTestRepository) PauseForSubagent(_ context.Context, commit SubagentWaitCommit) error {
	r.subagentWait = commit
	return nil
}
func (r *loopTestRepository) ClaimSubagentResume(context.Context, string, string, time.Duration) (SubagentResume, bool, error) {
	return SubagentResume{}, false, nil
}
func (r *loopTestRepository) UpdateInvocationContext(context.Context, string, string) error {
	return nil
}
func (r *loopTestRepository) TaskCancelRequested(context.Context, string) (bool, error) {
	return r.cancelRequested, nil
}
func (r *loopTestRepository) CommitTaskCancellation(_ context.Context, _, _, _ string, traceRef content.Ref, _ Usage) error {
	r.cancellationTraceRef = traceRef
	return nil
}

func (r *loopTestRepository) SaveAgentCheckpoint(_ context.Context, _ TaskContext, phase string, ref content.Ref) error {
	r.checkpointSaves++
	if r.failCheckpointAt > 0 && r.checkpointSaves == r.failCheckpointAt {
		return fmt.Errorf("simulated process interruption")
	}
	r.checkpointPhase = phase
	r.checkpointRef = ref
	return nil
}

func (r *loopTestRepository) SaveContextCheckpoint(context.Context, string, string, ContextCheckpoint) error {
	return nil
}
func (r *loopTestRepository) LoadTaskInputsAfter(_ context.Context, _ string, after int64) ([]ContextRecord, error) {
	result := make([]ContextRecord, 0, len(r.inputs))
	for _, record := range r.inputs {
		if record.Sequence > after {
			result = append(result, record)
		}
	}
	return result, nil
}

func (r *loopTestRepository) LoadConversationUpdatesAfter(_ context.Context, _ string, after int64) ([]ContextRecord, error) {
	result := make([]ContextRecord, 0, len(r.conversationUpdates))
	for _, input := range r.conversationUpdates {
		if input.Sequence > after {
			result = append(result, input)
		}
	}
	return result, nil
}

func testModelCapabilities() ModelCapabilities {
	return ModelCapabilities{
		Text: true, ToolCalling: true, Usage: true, Cancellation: true,
		ContextTokens: 32_768, MaxOutputTokens: 4_096, DataResidency: "test",
	}
}
