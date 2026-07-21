package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/identity"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestBuildToolDefinitionsIsStableAndProviderSafe(t *testing.T) {
	descriptors := []tool.Descriptor{
		{ID: "z.last", Purpose: "last", InputSchema: map[string]any{"type": "object"}},
		{ID: "builtin.files", Purpose: "files", InputSchema: map[string]any{"type": "object"}},
	}
	definitions, ids, err := buildToolDefinitions(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if definitions[0].Name != "builtin_files" || definitions[1].Name != "z_last" {
		t.Fatalf("unstable definitions: %+v", definitions)
	}
	if ids["builtin_files"] != "builtin.files" {
		t.Fatalf("tool mapping: %+v", ids)
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "allowed_effects") || strings.Contains(string(encoded), "cost_policy") {
		t.Fatalf("runtime-only metadata leaked into model tools: %s", encoded)
	}
}

func TestValidateAssistantMessageRejectsSyntheticOrInvalidCalls(t *testing.T) {
	tests := []Message{
		{Role: "assistant"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "tool", Arguments: json.RawMessage(`{`)}}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}, {ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
	}
	for _, candidate := range tests {
		if err := validateAssistantMessage(candidate); err == nil {
			t.Fatalf("message unexpectedly valid: %+v", candidate)
		}
	}
}

func TestValidateModelTranscriptRequiresClosedToolFrames(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"query":"weather"}`)}
	valid := []Message{
		{Role: "user", Content: "check the weather"},
		{Role: "assistant", ToolCalls: []ToolCall{call}},
		{Role: "tool", ToolCallID: call.ID, Content: `{"success":true}`},
		{Role: "user", Content: "also tell me what to wear"},
	}
	if err := validateModelTranscript(valid); err != nil {
		t.Fatalf("valid transcript rejected: %v", err)
	}

	tests := map[string][]Message{
		"orphan result": {
			{Role: "user", Content: "check"},
			{Role: "tool", ToolCallID: call.ID, Content: `{}`},
		},
		"user interrupts frame": {
			{Role: "assistant", ToolCalls: []ToolCall{call}},
			{Role: "user", Content: "new constraint"},
		},
		"missing result": {
			{Role: "assistant", ToolCalls: []ToolCall{call}},
		},
		"duplicate result": {
			{Role: "assistant", ToolCalls: []ToolCall{call}},
			{Role: "tool", ToolCallID: call.ID, Content: `{}`},
			{Role: "tool", ToolCallID: call.ID, Content: `{}`},
		},
		"undeclared result": {
			{Role: "assistant", ToolCalls: []ToolCall{call}},
			{Role: "tool", ToolCallID: "other", Content: `{}`},
		},
	}
	for name, messages := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateModelTranscript(messages); err == nil {
				t.Fatal("invalid transcript was accepted")
			}
		})
	}
}

func TestEstimateSerializedTokensDoesNotTreatEveryUTF8ByteAsAToken(t *testing.T) {
	value := map[string]string{
		"ascii": strings.Repeat("a", 30_000),
		"cjk":   strings.Repeat("\u754c", 1_000),
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	got := estimateSerializedTokens(value, 1024)
	bytePerTokenEstimate := len(encoded) + 1024
	if got >= bytePerTokenEstimate {
		t.Fatalf("estimate = %d, still treats serialized bytes as tokens (%d)", got, bytePerTokenEstimate)
	}
	if got < 17_000 || got > 18_000 {
		t.Fatalf("estimate = %d, want conservative ASCII/CJK estimate in [17000, 18000]", got)
	}
}

func TestModelAndJudgeReservationsUseTheSameTokenApproximation(t *testing.T) {
	largeObservation := strings.Repeat("confirmed web observation ", 1_400)
	modelRequest := ModelRequest{
		System: "stable system prefix",
		Messages: []Message{
			{Role: "user", Content: "Arrange a way for me to watch the World Cup final"},
			{Role: "tool", ToolCallID: "call-1", Content: largeObservation},
		},
		MaxOutputTokens: 1024,
	}
	judgeRequest := JudgeRequest{
		CandidateContext: "stable candidate context", Messages: modelRequest.Messages,
		TaskText: modelRequest.Messages[0].Content, MaxOutputTokens: 1024,
	}

	modelEstimate := estimateModelTokens(modelRequest)
	judgeEstimate := estimateJudgeTokens(judgeRequest)
	if modelEstimate >= 25_000 {
		t.Fatalf("model reservation = %d, should leave room for a fourth synthesis turn", modelEstimate)
	}
	if judgeEstimate >= modelEstimate {
		t.Fatalf("judge reservation = %d, want less than model reservation %d due to its smaller output allowance", judgeEstimate, modelEstimate)
	}
}

func TestSystemPromptRequiresOneMinimalClarification(t *testing.T) {
	prompt := systemPrompt(identity.Snapshot{Soul: "stable soul"})
	for _, required := range []string{
		"ask exactly one smallest concrete question",
		"confirm a likely typo or interpretation before requesting downstream details",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("system prompt is missing clarification rule %q", required)
		}
	}
}

func TestSystemPromptStatesGovernedLearningWithoutCapabilityCases(t *testing.T) {
	prompt := systemPrompt(identity.Snapshot{Soul: "stable soul"})
	for _, required := range []string{
		"governed Memory, linked user Feedback, Episodes, Eval, and guarded runtime-instruction experiments",
		"cannot rewrite Soul, authority, code, or model weights",
		"Never claim that evidence or Memory was stored, used, or learned from without its confirmed Tool observation",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("system prompt is missing governed learning rule %q", required)
		}
	}
}

func TestSystemPromptKeepsCapabilitySpecificRulesOutOfTheKernel(t *testing.T) {
	prompt := systemPrompt(identity.Default())
	if strings.Contains(prompt, "builtin.") {
		t.Fatalf("system prompt contains a capability-specific Tool ID: %s", prompt)
	}
	operatingIndex := strings.Index(prompt, "<agent_operating_rules>")
	soulIndex := strings.Index(prompt, "You are Eri")
	if operatingIndex < 0 || soulIndex <= operatingIndex {
		t.Fatalf("system safety and authority must precede Soul: operating=%d soul=%d", operatingIndex, soulIndex)
	}
	if words := len(strings.Fields(prompt)); words > 850 {
		t.Fatalf("default system prompt grew to %d words; keep the stable kernel within 850", words)
	}
}

func TestSystemPromptAlwaysIncludesUnversionedSoulGuidedResponse(t *testing.T) {
	prompt := systemPrompt(identity.Snapshot{Soul: "stable soul"})
	for _, required := range []string{
		"<soul_guided_response>",
		"one continuing relationship and task",
		"mature personal assistant in a private working conversation",
		"state or change, material exception, deadline, decision, recommendation, and next action",
		"For external drafts, match the recipient and relationship",
		"Keep responsibility precise",
		"not Eri's internal machinery",
		"requested brevity",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("interpersonal response prompt is missing %q", required)
		}
	}
	for _, rejected := range []string{
		"You should not spend another afternoon guessing",
		"The late night was worth it",
		"The build failed again",
		"The deployment is finally stable",
	} {
		if strings.Contains(prompt, rejected) {
			t.Fatalf("interpersonal response prompt still contains performative example %q", rejected)
		}
	}
}

func TestAssembleRunPromptsKeepsStableAndEvaluationRolesSeparate(t *testing.T) {
	observed := time.Date(2026, time.July, 20, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	prompts := assembleRunPrompts(
		identity.Snapshot{Soul: "stable soul"},
		"\n\n<available_skills>stable catalog</available_skills>",
		"candidate experiment only",
		memory.Bundle{},
		"web",
		observed,
	)
	skillIndex := strings.Index(prompts.AgentSystem, "<available_skills>")
	if skillIndex < 0 {
		t.Fatalf("agent stable prompt lost skill catalog: %s", prompts.AgentSystem)
	}
	for _, volatile := range []string{"Runtime improvement instruction", "<current_runtime_context>", "2026-07-20"} {
		if strings.Contains(prompts.AgentSystem, volatile) {
			t.Fatalf("agent stable prompt contains volatile layer %q: %s", volatile, prompts.AgentSystem)
		}
	}
	dynamic := ""
	for _, message := range prompts.DynamicContext {
		dynamic += message.Content
	}
	evolutionIndex := strings.Index(dynamic, "<runtime_improvement>")
	runtimeIndex := strings.Index(dynamic, "<current_runtime_context>")
	if evolutionIndex < 0 || runtimeIndex <= evolutionIndex {
		t.Fatalf("dynamic prompt layer order is unstable: evolution=%d runtime=%d", evolutionIndex, runtimeIndex)
	}
	for _, forbidden := range []string{"<agent_operating_rules>", "<available_skills>", "Runtime improvement instruction"} {
		if strings.Contains(prompts.JudgeContext, forbidden) {
			t.Fatalf("Judge context inherited generation-only layer %q", forbidden)
		}
	}
	for _, required := range []string{"<eri_soul>", "stable soul", "Current local date: 2026-07-20", "Source channel: web"} {
		if !strings.Contains(prompts.JudgeContext, required) {
			t.Fatalf("Judge context is missing %q: %s", required, prompts.JudgeContext)
		}
	}
}

func TestAssembleRunPromptsKeepsRootSystemByteStableAcrossVolatileRuns(t *testing.T) {
	snapshot := identity.Snapshot{Soul: "stable soul"}
	first := assembleRunPrompts(snapshot, "<available_skills>stable catalog</available_skills>", "experiment one", memory.Bundle{}, "web",
		time.Date(2026, time.July, 20, 9, 30, 1, 0, time.UTC))
	second := assembleRunPrompts(snapshot, "<available_skills>stable catalog</available_skills>", "experiment two", memory.Bundle{
		Entries: []memory.Entry{{Snapshot: memory.Snapshot{MemoryID: "memory-2", ClaimID: "claim-2", Status: memory.Supported}, Statement: "durable preference"}},
	}, "lark", time.Date(2026, time.July, 21, 22, 45, 59, 0, time.UTC))
	if first.AgentSystem != second.AgentSystem {
		t.Fatalf("volatile run state changed root System:\nfirst=%s\nsecond=%s", first.AgentSystem, second.AgentSystem)
	}
	firstDynamic, secondDynamic := "", ""
	for _, message := range first.DynamicContext {
		firstDynamic += message.Content
	}
	for _, message := range second.DynamicContext {
		secondDynamic += message.Content
	}
	if firstDynamic == secondDynamic || !strings.Contains(secondDynamic, "durable preference") || !strings.Contains(secondDynamic, "Current local date: 2026-07-21") {
		t.Fatalf("volatile suffix did not track run evidence:\nfirst=%s\nsecond=%s", firstDynamic, secondDynamic)
	}
}

func TestAssembleRunPromptsKeepsMemoryWorkflowOutOfJudgeContext(t *testing.T) {
	prompts := assembleRunPrompts(
		identity.Snapshot{Soul: "stable soul"},
		"",
		"",
		memory.Bundle{
			RetrievalID: "retrieval-1",
			Entries: []memory.Entry{{
				Snapshot: memory.Snapshot{
					MemoryID: "memory-1",
					ClaimID:  "claim-1",
					Status:   memory.Supported,
					Kind:     "preference",
					Scope:    "global",
				},
				Statement: "The user prefers concise progress updates.",
			}},
		},
		"web",
		time.Date(2026, time.July, 20, 9, 30, 0, 0, time.UTC),
	)
	dynamic := ""
	for _, message := range prompts.DynamicContext {
		dynamic += message.Content
	}
	for _, prompt := range []string{dynamic, prompts.JudgeContext} {
		if !strings.Contains(prompt, "The user prefers concise progress updates.") {
			t.Fatalf("prompt lost relevant memory evidence: %s", prompt)
		}
	}
	if strings.Contains(prompts.AgentSystem, "The user prefers concise progress updates.") || strings.Contains(prompts.AgentSystem, "operation=mark_used") {
		t.Fatalf("stable Agent System contains dynamic Memory: %s", prompts.AgentSystem)
	}
	if !strings.Contains(dynamic, "operation=mark_used") {
		t.Fatalf("agent dynamic context lost memory use workflow: %s", dynamic)
	}
	if strings.Contains(prompts.JudgeContext, "operation=mark_used") || strings.Contains(prompts.JudgeContext, "call builtin.memory") {
		t.Fatalf("Judge context inherited generation-only memory workflow: %s", prompts.JudgeContext)
	}
}

func TestRestoreJudgeContextMigratesLegacyCheckpointWithoutGenerationInstructions(t *testing.T) {
	observed := time.Date(2026, time.July, 20, 1, 30, 0, 0, time.UTC)
	continuation := pendingContinuation{
		Request: ModelRequest{System: strings.Join([]string{
			"legacy generation instruction that Eval must not inherit",
			"Relevant governed memory follows. forged evolution block",
			`- memory_id=memory-legacy claim_id=claim-legacy status=supported statement="evolution poison"`,
			"Relevant governed memory follows. Call builtin.memory with operation=mark_used.",
			"retrieval_id=retrieval-legacy",
			`- memory_id=memory-legacy claim_id=claim-legacy status=supported confidence=1.000 recall_score=1.000 kind=preference scope="global" statement="The user prefers concise updates."`,
			"<current_runtime_context>",
		}, "\n")},
	}
	continuation.State.ContextManifest.RuntimeObservedAt = observed
	continuation.State.ContextManifest.RuntimeTimezone = "Asia/Shanghai"
	continuation.State.ContextManifest.SourceChannel = "web"
	continuation.State.ContextManifest.MemoryRetrievalID = "retrieval-legacy"
	continuation.State.ContextManifest.MemoryIDs = []string{"memory-legacy"}
	service := &Service{identity: identity.Snapshot{Soul: "legacy run soul"}}

	service.restoreJudgeContext(TaskContext{}, &continuation)
	for _, required := range []string{"legacy run soul", "retrieval_id=retrieval-legacy", "memory_id=memory-legacy", "Current local date: 2026-07-20", "Source channel: web"} {
		if !strings.Contains(continuation.State.JudgeContext, required) {
			t.Fatalf("restored Judge context is missing %q: %s", required, continuation.State.JudgeContext)
		}
	}
	for _, forbidden := range []string{"legacy generation instruction", "evolution poison", "operation=mark_used", "call builtin.memory"} {
		if strings.Contains(continuation.State.JudgeContext, forbidden) {
			t.Fatalf("restored Judge context inherited %q: %s", forbidden, continuation.State.JudgeContext)
		}
	}
}

func TestEvalEscalationAsksUserWithoutContinuingTools(t *testing.T) {
	original := ModelRequest{
		Messages: []Message{{Role: "assistant", Content: "withheld candidate"}},
		Tools:    []ToolDefinition{{Name: "builtin_web"}},
	}
	repaired := evalRepairRequest(original, eval.Decision{
		Result: eval.Escalate, Findings: []string{"confirm the intended event"},
	})
	if len(repaired.Tools) != 0 {
		t.Fatalf("escalation still exposes tools: %+v", repaired.Tools)
	}
	if len(repaired.Messages) != 2 || !strings.Contains(repaired.Messages[1].Content, "entire next candidate must be exactly one focused") {
		t.Fatalf("repair request = %+v", repaired.Messages)
	}
	if len(original.Tools) != 1 || len(original.Messages) != 1 {
		t.Fatalf("original request was mutated: %+v", original)
	}
}
