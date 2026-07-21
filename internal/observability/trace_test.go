package observability

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/memory"
)

type traceRepository struct {
	runs     []RunSummary
	detail   RunDetail
	memories []memory.Candidate
}

func (r *traceRepository) ListRuns(context.Context, int) ([]RunSummary, error) {
	return append([]RunSummary(nil), r.runs...), nil
}

func (r *traceRepository) LoadRun(_ context.Context, id string) (RunDetail, bool, error) {
	if r.detail.Run.ID != id {
		return RunDetail{}, false, nil
	}
	return r.detail, true, nil
}

func (r *traceRepository) InspectMemory(context.Context, int) ([]memory.Candidate, error) {
	return append([]memory.Candidate(nil), r.memories...), nil
}

func (*traceRepository) LoadActiveRunTrace(context.Context, string) (content.Ref, bool, error) {
	return content.Ref{}, false, nil
}

type traceContent map[string][]byte

func (c traceContent) Get(_ context.Context, ref content.Ref) ([]byte, error) {
	return append([]byte(nil), c[ref.ObjectID]...), nil
}

func TestHydrateLoopTraceDoesNotProjectProviderTranscript(t *testing.T) {
	t.Parallel()
	marker := "provider-reasoning-must-not-reach-observatory"
	detail := RunDetail{
		Run:       RunSummary{ID: "run-1"},
		Artifacts: []Artifact{{TraceRef: content.Ref{ObjectID: "trace-1"}}},
	}
	stored := []byte(`{
		"provider_transcript":{"system":"private system","messages":[{"role":"assistant","reasoning_content":"` + marker + `"}]},
		"model_turns":[{"id":"inv-1:turn:1","ordinal":1,"trigger":"initial_request","status":"succeeded","request":{},"assistant":{},"usage":{}}],
		"tool_calls":[],"evaluations":[]
	}`)
	service := NewService(&traceRepository{}, traceContent{"trace-1": stored})
	if err := service.hydrateLoopTrace(context.Background(), &detail); err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(detail.loopTrace)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.loopTrace.ModelTurns) != 1 || strings.Contains(string(projected), marker) || strings.Contains(string(projected), "private system") {
		t.Fatalf("safe observability projection exposed provider transcript: %s", projected)
	}
}

func TestRunSpansPreserveFanOutFanInAndMemoryStages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	statementRef := content.Ref{ObjectID: "statement-1"}
	run := RunSummary{ID: "run-1", TaskID: "task-1", Status: "succeeded", StartedAt: now, EndedAt: now.Add(4 * time.Second), ModelCalls: 2, ToolCalls: 2}
	repository := &traceRepository{
		runs: []RunSummary{run},
		detail: RunDetail{
			Run: run,
			Model: ModelExecution{
				ID: "inv-1", Status: "succeeded", Target: "local-model", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(2 * time.Second),
				ContextManifest: execution.ContextManifest{
					RetrievedMemoryIDs: []string{"memory-1"}, MemoryIDs: []string{"memory-1"}, AppliedMemoryIDs: []string{"memory-1"},
					MemoryChecked: true, ExternalMemoryIDs: []string{"memory-1"},
					SkillIDs: []string{"travel"}, ToolIDs: []string{"calendar", "files"},
				},
				Usage: map[string]any{"model_calls": float64(2), "input_tokens": float64(120), "output_tokens": float64(30)},
			},
			Effects: []Effect{
				{ID: "effect-a", ToolID: "calendar", Target: "plan", Status: "confirmed", CreatedAt: now.Add(2 * time.Second)},
				{ID: "effect-b", ToolID: "files", Target: "brief.md", Status: "confirmed", CreatedAt: now.Add(2 * time.Second)},
			},
			Artifacts: []Artifact{{ID: "artifact-1", Version: 1, EvalID: "eval-1", Eval: "pass", DeliveryID: "delivery-1", Delivery: "sent"}},
		},
		memories: []memory.Candidate{{
			Snapshot: memory.Snapshot{
				MemoryID: "memory-1", ClaimID: "claim-1", StatementRef: statementRef, Status: memory.Supported,
				Confidence: .92, Kind: "preference", Scope: "travel", UsagePolicy: "allow", LifecycleStatus: "active",
			},
			Sources: []memory.SourceSummary{{EvidenceID: "evidence-1", Relation: memory.Supports, SourceType: "user", SourceRef: "interaction:user-1", Reliability: 1}},
		}},
	}
	service := NewService(repository, traceContent{"statement-1": []byte("The user prefers a window seat")})

	spans, err := service.buildRunSpans(context.Background(), repository.detail, memoryExposureDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]RunSpan, len(spans))
	for _, span := range spans {
		byID[span.ID] = span
	}
	if got := byID["loop:inv-1"].DependsOn; !reflect.DeepEqual(got, []string{"context:inv-1", "memory:inv-1"}) {
		t.Fatalf("loop dependencies = %v", got)
	}
	for _, id := range []string{"effect:effect-a", "effect:effect-b"} {
		if got := byID[id].DependsOn; !reflect.DeepEqual(got, []string{"loop:inv-1"}) {
			t.Fatalf("%s dependencies = %v; aggregate effects must fan out from the aggregate loop", id, got)
		}
	}
	if got := byID["eval:eval-1"].DependsOn; !reflect.DeepEqual(got, []string{"effect:effect-a", "effect:effect-b", "loop:inv-1"}) {
		t.Fatalf("eval dependencies = %v; Eval must fan in confirmed work", got)
	}
	memorySpan := byID["memory:inv-1"]
	if memorySpan.Memory == nil || !memorySpan.Memory.Checked || memorySpan.Memory.RetrievedCount != 1 || memorySpan.Memory.InjectedCount != 1 || memorySpan.Memory.AppliedCount != 1 || !memorySpan.Memory.ExternalSent {
		t.Fatalf("memory record = %+v", memorySpan.Memory)
	}
	item := memorySpan.Memory.Items[0]
	if item.Statement != "The user prefers a window seat" || !reflect.DeepEqual(item.Stages, []MemoryStage{MemoryStored, MemoryRetrieved, MemoryInjected, MemoryApplied, MemoryExternal}) {
		t.Fatalf("memory observation = %+v", item)
	}
	if item.Sources[0].SourceRef == "" {
		t.Fatalf("developer observatory lost source provenance: %+v", item.Sources[0])
	}
	safeSpans, err := service.buildRunSpans(context.Background(), repository.detail, memoryExposureConversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range safeSpans {
		if span.Memory != nil && len(span.Memory.Items) > 0 && span.Memory.Items[0].Sources[0].SourceRef != "" {
			t.Fatalf("conversation projection leaked developer source reference: %+v", span.Memory.Items[0].Sources[0])
		}
	}
	encoded, err := json.Marshal(spans)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-leak") || strings.Contains(string(encoded), "private_prompt") {
		t.Fatalf("conversation trace leaked raw context: %s", encoded)
	}
}

func TestRunSpansExposeExplicitAgentIterationsWithoutInventingLoopBackEdges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	detail := RunDetail{
		Run: RunSummary{ID: "run-1", TaskID: "task-1", Status: "succeeded", StartedAt: now, EndedAt: now.Add(9 * time.Second)},
		Model: ModelExecution{
			ID: "inv-1", Status: "succeeded", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(8 * time.Second),
			ContextManifest: execution.ContextManifest{MemoryChecked: true}, Usage: map[string]any{"model_calls": float64(3)},
		},
		Effects: []Effect{
			{ID: "intent-a", ToolID: "calendar", Status: "confirmed", CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(4 * time.Second)},
			{ID: "intent-b", ToolID: "builtin.delegate", Status: "confirmed", CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(4 * time.Second)},
			{ID: "child-intent", ParentIntentID: "intent-b", ToolID: "builtin.web", Status: "confirmed", CreatedAt: now.Add(3500 * time.Millisecond), UpdatedAt: now.Add(3900 * time.Millisecond)},
		},
		Artifacts: []Artifact{
			{ID: "progress-artifact", Kind: "progress", Version: 1, EvalID: "progress-eval", Eval: "pass", DeliveryID: "progress-delivery", Delivery: "sent"},
			{ID: "artifact-1", Kind: "text", Version: 2, EvalID: "final-eval", Eval: "pass", DeliveryID: "delivery-1", Delivery: "sent"},
		},
		loopTrace: persistedRunTrace{
			ModelTurns: []persistedModelTurn{
				{ID: "inv-1:turn:1", Ordinal: 1, Trigger: "initial_request", Status: "succeeded", StartedAt: now.Add(time.Second), EndedAt: now.Add(2 * time.Second), Checkpoints: []string{"ready_for_model", "model_received"}, FinishReason: "tool_calls"},
				{ID: "inv-1:turn:2", Ordinal: 2, Trigger: "tool_observations", Status: "succeeded", StartedAt: now.Add(4 * time.Second), EndedAt: now.Add(5 * time.Second), Checkpoints: []string{"ready_for_model", "candidate_received"}, FinishReason: "stop"},
				{ID: "inv-1:turn:3", Ordinal: 3, Trigger: "eval_repair", Status: "succeeded", StartedAt: now.Add(6 * time.Second), EndedAt: now.Add(7 * time.Second), Checkpoints: []string{"ready_for_model", "candidate_received"}, FinishReason: "stop"},
			},
			ToolCalls: []persistedToolCall{
				{ModelTurnID: "inv-1:turn:1", ToolCallID: "call-a", ToolID: "calendar", IntentID: "intent-a", Status: "confirmed"},
				{ModelTurnID: "inv-1:turn:1", ToolCallID: "call-b", ToolID: "builtin.delegate", IntentID: "intent-b", Status: "confirmed"},
			},
			Evaluations: []persistedEvaluation{
				{ID: "eval-1", ModelTurnID: "inv-1:turn:2", Attempt: 1, Result: "repair", StartedAt: now.Add(5 * time.Second), EndedAt: now.Add(6 * time.Second)},
				{ID: "eval-2", ModelTurnID: "inv-1:turn:3", Attempt: 2, Result: "pass", StartedAt: now.Add(7 * time.Second), EndedAt: now.Add(8 * time.Second)},
			},
			Progress: []persistedProgress{{ID: "progress-artifact", ModelTurnID: "inv-1:turn:1", DeliveryID: "progress-delivery", Status: "queued", CreatedAt: now.Add(2 * time.Second)}},
		},
	}
	spans, err := NewService(&traceRepository{}, nil).buildRunSpans(context.Background(), detail, memoryExposureDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]RunSpan, len(spans))
	for _, span := range spans {
		byID[span.ID] = span
	}
	loop := byID["loop:inv-1"]
	if loop.Kind != "agent_loop" || loop.Metadata["focusable"] != true || loop.Metadata["turn_count"] != 3 {
		t.Fatalf("loop compound span = %+v", loop)
	}
	if got := byID["observation:inv-1:turn:1"].DependsOn; !reflect.DeepEqual(got, []string{"loop-tool:inv-1:turn:1:call-a:1", "loop-tool:inv-1:turn:1:call-b:2"}) {
		t.Fatalf("parallel Tool fan-in = %v", got)
	}
	child := byID["effect:child-intent"]
	if got := child.DependsOn; !reflect.DeepEqual(got, []string{"loop-tool:inv-1:turn:1:call-b:2"}) {
		t.Fatalf("Child Tool parent dependency = %v", got)
	}
	if child.ParentID != "loop:inv-1" || child.Metadata["loop_id"] != "loop:inv-1" || child.Metadata["parent_intent_id"] != "intent-b" {
		t.Fatalf("Child Tool loop projection = %+v", child)
	}
	if got := byID["checkpoint:inv-1:turn:2:ready"].DependsOn; !reflect.DeepEqual(got, []string{"observation:inv-1:turn:1"}) {
		t.Fatalf("Turn 2 entry = %v", got)
	}
	if got := byID["checkpoint:inv-1:turn:3:ready"].DependsOn; !reflect.DeepEqual(got, []string{"repair:eval-1"}) {
		t.Fatalf("repair-to-next-Turn dependency = %v", got)
	}
	if got := byID["eval:final-eval"].DependsOn; !reflect.DeepEqual(got, []string{"loop:inv-1"}) {
		t.Fatalf("main Run Eval dependencies = %v", got)
	}
	if got := byID["eval:progress-eval"].DependsOn; !reflect.DeepEqual(got, []string{"model:inv-1:turn:1"}) {
		t.Fatalf("progress Eval dependency = %v", got)
	}
	for _, span := range spans {
		for _, dependency := range span.DependsOn {
			if span.ID == dependency {
				t.Fatalf("self loop found in span %+v", span)
			}
		}
	}
}

func TestSupersededTurnProjectsAttentionBoundaryWithoutCandidateCausality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	detail := RunDetail{
		Run: RunSummary{ID: "run-1", TaskID: "task-1", Status: "succeeded", StartedAt: now},
		Model: ModelExecution{
			ID: "inv-1", Status: "succeeded", CreatedAt: now, UpdatedAt: now.Add(3 * time.Second),
			ContextManifest: execution.ContextManifest{MemoryChecked: true},
		},
		loopTrace: persistedRunTrace{ModelTurns: []persistedModelTurn{
			{ID: "inv-1:turn:1", Ordinal: 1, Trigger: "initial_request", Status: "superseded", InputSequence: 10, StartedAt: now, EndedAt: now.Add(time.Second), Checkpoints: []string{"ready_for_model", "newer_user_input"}, FinishReason: "stop"},
			{ID: "inv-1:turn:2", Ordinal: 2, Trigger: "user_input", Status: "succeeded", InputSequence: 11, StartedAt: now.Add(2 * time.Second), EndedAt: now.Add(3 * time.Second), Checkpoints: []string{"ready_for_model", "candidate_received"}, FinishReason: "stop"},
		}},
	}
	spans, err := NewService(&traceRepository{}, nil).buildRunSpans(context.Background(), detail, memoryExposureConversation)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]RunSpan, len(spans))
	for _, span := range spans {
		byID[span.ID] = span
	}
	if _, exists := byID["candidate:inv-1:turn:1"]; exists {
		t.Fatal("superseded model output was projected as a candidate")
	}
	attention := byID["attention:inv-1:turn:1"]
	if !reflect.DeepEqual(attention.DependsOn, []string{"model:inv-1:turn:1"}) {
		t.Fatalf("attention boundary = %+v", attention)
	}
	if got := byID["checkpoint:inv-1:turn:2:ready"].DependsOn; !reflect.DeepEqual(got, []string{"attention:inv-1:turn:1"}) {
		t.Fatalf("resumed Turn dependency = %v", got)
	}
	if got := byID["iteration:inv-1:turn:2"].Metadata["input_sequence"]; got != int64(11) {
		t.Fatalf("resumed input sequence = %#v", got)
	}
}

func TestInterruptedToolFrameProjectsSkippedSiblingBeforeAttentionBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	detail := RunDetail{
		Run: RunSummary{ID: "run-1", TaskID: "task-1", Status: "succeeded", StartedAt: now},
		Model: ModelExecution{
			ID: "inv-1", Status: "succeeded", CreatedAt: now, UpdatedAt: now.Add(4 * time.Second),
			ContextManifest: execution.ContextManifest{MemoryChecked: true},
		},
		Effects: []Effect{{ID: "intent-1", ToolCallID: "call-1", ToolID: "builtin.web", Status: "confirmed", CreatedAt: now, UpdatedAt: now.Add(time.Second)}},
		loopTrace: persistedRunTrace{
			ModelTurns: []persistedModelTurn{
				{ID: "inv-1:turn:1", Ordinal: 1, Trigger: "initial_request", Status: "succeeded", InputSequence: 10, StartedAt: now, EndedAt: now.Add(2 * time.Second), Checkpoints: []string{"ready_for_model", "model_received", "newer_user_input"}, FinishReason: "tool_calls"},
				{ID: "inv-1:turn:2", Ordinal: 2, Trigger: "user_input", Status: "succeeded", InputSequence: 11, StartedAt: now.Add(3 * time.Second), EndedAt: now.Add(4 * time.Second), Checkpoints: []string{"ready_for_model", "candidate_received"}, FinishReason: "stop"},
			},
			ToolCalls: []persistedToolCall{
				{ModelTurnID: "inv-1:turn:1", ToolCallID: "call-1", ToolID: "builtin.web", IntentID: "intent-1", Status: "confirmed"},
				{ModelTurnID: "inv-1:turn:1", ToolCallID: "call-2", ToolID: "builtin.web", Status: "superseded_before_execution"},
			},
		},
	}
	spans, err := NewService(&traceRepository{}, nil).buildRunSpans(context.Background(), detail, memoryExposureConversation)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]RunSpan, len(spans))
	for _, span := range spans {
		byID[span.ID] = span
	}
	skipped := byID["loop-tool:inv-1:turn:1:call-2:2"]
	if skipped.Status != "superseded_before_execution" {
		t.Fatalf("skipped sibling = %+v", skipped)
	}
	attention := byID["attention:inv-1:turn:1"]
	if !reflect.DeepEqual(attention.DependsOn, []string{"observation:inv-1:turn:1"}) {
		t.Fatalf("attention dependency = %+v", attention)
	}
	if got := byID["checkpoint:inv-1:turn:2:ready"].DependsOn; !reflect.DeepEqual(got, []string{"attention:inv-1:turn:1"}) {
		t.Fatalf("resumed Turn dependency = %v", got)
	}
}

func TestCallExchangesExposeGovernedDetailsWithoutPromptsOrCredentials(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	detail := RunDetail{
		Run:   RunSummary{ID: "run-1", TaskID: "task-1", Status: "succeeded", StartedAt: now},
		Model: ModelExecution{ID: "inv-1", Status: "succeeded", CreatedAt: now, UpdatedAt: now.Add(time.Second), ContextManifest: execution.ContextManifest{MemoryChecked: true}},
		Effects: []Effect{{
			ID: "intent-1", ToolCallID: "call-1", ToolID: "builtin.web", Status: "confirmed",
			PayloadRef: content.Ref{ObjectID: "payload"}, ResultRef: content.Ref{ObjectID: "result"}, CreatedAt: now, UpdatedAt: now.Add(time.Second),
		}},
		loopTrace: persistedRunTrace{
			ModelTurns: []persistedModelTurn{{
				ID: "inv-1:turn:1", Ordinal: 1, Trigger: "initial_request", Status: "succeeded", StartedAt: now, EndedAt: now.Add(time.Second), Checkpoints: []string{"ready_for_model", "model_received"}, FinishReason: "tool_calls",
				Request:   persistedModelRequest{MessageCount: 3, MessageRoles: map[string]int{"user": 1, "system": 2}, ToolNames: []string{"builtin_web"}, MaxOutputTokens: 1024, EstimatedInputTokens: 400},
				Assistant: persistedAssistant{Content: "private candidate text", ToolCalls: []persistedToolName{{ID: "call-1", Name: "builtin_web"}}},
				Usage:     persistedUsage{Provider: "deepseek", Model: "deepseek-chat", InputTokens: 310, OutputTokens: 24},
			}},
			ToolCalls: []persistedToolCall{{ModelTurnID: "inv-1:turn:1", ToolCallID: "call-1", ToolID: "builtin.web", IntentID: "intent-1", Status: "confirmed"}},
		},
	}
	service := NewService(&traceRepository{}, traceContent{
		"payload": []byte(`{"query":"World Cup tickets","api_key":"must-not-leak"}`),
		"result":  []byte(`{"results":[{"title":"Official ticket page"}]}`),
	})
	if err := service.hydrateEffectExchanges(context.Background(), &detail); err != nil {
		t.Fatal(err)
	}
	spans, err := service.buildRunSpans(context.Background(), detail, memoryExposureConversation)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]RunSpan{}
	for _, span := range spans {
		byID[span.ID] = span
	}
	model := byID["model:inv-1:turn:1"]
	toolSpan := byID["loop-tool:inv-1:turn:1:call-1:1"]
	encoded, err := json.Marshal([]*CallExchange{model.Exchange, toolSpan.Exchange})
	if err != nil {
		t.Fatal(err)
	}
	projection := string(encoded)
	for _, forbidden := range []string{"private candidate text", "must-not-leak", "private_system_prompt_must_not_leak"} {
		if strings.Contains(projection, forbidden) {
			t.Fatalf("exchange projection leaked %q: %s", forbidden, projection)
		}
	}
	for _, required := range []string{"message_count", "builtin_web", "World Cup tickets", "[REDACTED]", "Official ticket page"} {
		if !strings.Contains(projection, required) {
			t.Fatalf("exchange projection missing %q: %s", required, projection)
		}
	}
}

func TestMemoryOverviewShowsStoredStateWithoutClaimingRunUse(t *testing.T) {
	t.Parallel()
	repository := &traceRepository{memories: []memory.Candidate{{Snapshot: memory.Snapshot{
		MemoryID: "memory-1", ClaimID: "claim-1", Status: memory.Contested, LifecycleStatus: "active", UsagePolicy: "do_not_use", Expired: true,
	}}}}
	overview, err := NewService(repository, nil).MemoryOverview(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Total != 1 || overview.Active != 1 || overview.Contested != 1 || overview.Expired != 1 || overview.DoNotUse != 1 {
		t.Fatalf("overview = %+v", overview)
	}
	if got := overview.Observations[0].Stages; !reflect.DeepEqual(got, []MemoryStage{MemoryStored}) {
		t.Fatalf("stored inventory stages = %v; it must not imply retrieval", got)
	}
}
