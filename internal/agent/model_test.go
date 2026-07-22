package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/identity"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/tool"
)

func TestBuildToolDefinitionsIsStableAndProviderSafe(t *testing.T) {
	descriptors := []tool.Descriptor{
		{ID: "z.last", Purpose: "last", InputSchema: map[string]any{"type": "object"}},
		{ID: "builtin.files", Purpose: "files", InputSchema: map[string]any{"type": "object"}},
		{ID: "builtin.commitments", Purpose: "schedule", InputSchema: map[string]any{"type": "object"}},
	}
	definitions, ids, err := buildToolDefinitions(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if definitions[0].Name != "schedule" || definitions[1].Name != "files" || definitions[2].Name != "z_last" {
		t.Fatalf("unstable definitions: %+v", definitions)
	}
	if ids["files"] != "builtin.files" {
		t.Fatalf("tool mapping: %+v", ids)
	}
	for _, definition := range definitions {
		if strings.HasPrefix(definition.Name, "builtin_") {
			t.Fatalf("model-visible Tool leaked internal namespace: %+v", definitions)
		}
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "allowed_effects") || strings.Contains(string(encoded), "cost_policy") {
		t.Fatalf("runtime-only metadata leaked into model tools: %s", encoded)
	}
}

func TestToolSurfaceTreatsAliasAsProviderProtocol(t *testing.T) {
	if !sameToolSurface(map[string]string{"files": "builtin.files"}, map[string]string{"files": "builtin.files"}) {
		t.Fatal("identical Tool surface was rejected")
	}
	if sameToolSurface(map[string]string{"builtin_files": "builtin.files"}, map[string]string{"files": "builtin.files"}) {
		t.Fatal("renamed model alias was accepted for in-flight checkpoint recovery")
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
		"The user does not need to say “remember”",
		"Current tasks and scheduled work belong to Runtime, not long-term Memory",
		"Never claim Memory was stored, changed, or forgotten without a confirmed Tool observation",
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
	systemIndex := strings.Index(prompt, "## System")
	soulIndex := strings.Index(prompt, "<soul>")
	if !strings.HasPrefix(prompt, "You are Eri") || systemIndex < 0 || soulIndex <= systemIndex {
		t.Fatalf("identity and System rules must precede Soul: system=%d soul=%d", systemIndex, soulIndex)
	}
	if words := len(strings.Fields(prompt)); words > 900 {
		t.Fatalf("default system prompt grew to %d words; keep the stable kernel within 900", words)
	}
}

func TestSystemPromptAlwaysIncludesStableSoul(t *testing.T) {
	prompt := systemPrompt(identity.Snapshot{Soul: "stable soul"})
	for _, required := range []string{
		"<soul>",
		"stable soul",
		"Match external drafts to their recipient",
		"never imply they were sent without a Receipt",
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
	snapshot := identity.Snapshot{Soul: "stable soul"}
	catalog := "\n\n<available_skills>stable catalog</available_skills>"
	prompts := assembleRunPrompts(
		snapshot,
		catalog,
		Experience{},
		memory.Bundle{},
		"web",
		observed,
	)
	stablePrefix := systemPrompt(snapshot) + catalog
	if !strings.HasPrefix(prompts.AgentSystem, stablePrefix) {
		t.Fatalf("agent System lost reusable prefix: %s", prompts.AgentSystem)
	}
	skillIndex := strings.Index(prompts.AgentSystem, "<available_skills>")
	runtimeIndex := strings.Index(prompts.AgentSystem, "<current_runtime_context>")
	if skillIndex < 0 || runtimeIndex <= skillIndex {
		t.Fatalf("agent stable prompt lost skill catalog: %s", prompts.AgentSystem)
	}
	for _, required := range []string{"<current_runtime_context>", "2026-07-20", "Source channel: web"} {
		if !strings.Contains(prompts.AgentSystem, required) {
			t.Fatalf("agent System is missing runtime fact %q: %s", required, prompts.AgentSystem)
		}
	}
	if prompts.MemoryContext != nil || strings.Contains(prompts.AgentSystem, "<runtime_improvement>") {
		t.Fatalf("ordinary prompt unexpectedly contains Memory or an experiment: %+v", prompts)
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

func TestAssembleRunPromptsKeepsReusablePrefixStableAcrossRuntimeChanges(t *testing.T) {
	snapshot := identity.Snapshot{Soul: "stable soul"}
	catalog := "<available_skills>stable catalog</available_skills>"
	stablePrefix := systemPrompt(snapshot) + catalog
	first := assembleRunPrompts(snapshot, catalog, Experience{}, memory.Bundle{}, "web",
		time.Date(2026, time.July, 20, 9, 30, 1, 0, time.UTC))
	second := assembleRunPrompts(snapshot, catalog, Experience{}, memory.Bundle{
		Entries: []memory.Entry{{Snapshot: memory.Snapshot{MemoryID: "memory-2", ClaimID: "claim-2", Status: memory.Supported}, Statement: "durable preference"}},
	}, "lark", time.Date(2026, time.July, 21, 22, 45, 59, 0, time.UTC))
	if !strings.HasPrefix(first.AgentSystem, stablePrefix) || !strings.HasPrefix(second.AgentSystem, stablePrefix) {
		t.Fatalf("runtime changes invalidated the reusable System prefix")
	}
	if first.AgentSystem == second.AgentSystem || !strings.Contains(second.AgentSystem, "Current local date: 2026-07-21") {
		t.Fatalf("runtime System tail did not track the current Run")
	}
	if first.MemoryContext != nil || second.MemoryContext == nil || !strings.Contains(second.MemoryContext.Content, "durable preference") {
		t.Fatalf("Memory was not kept in its separate message: first=%+v second=%+v", first.MemoryContext, second.MemoryContext)
	}
}

func TestAssembleRunPromptsAddsVersionedExperienceAfterStablePrefix(t *testing.T) {
	snapshot := identity.Snapshot{Soul: "stable soul"}
	catalog := "<available_skills>stable catalog</available_skills>"
	experience := Experience{ReleaseID: "experience-7", Version: 7, Text: "- Compare independent evidence before finalizing."}
	prompts := assembleRunPrompts(snapshot, catalog, experience, memory.Bundle{}, "web",
		time.Date(2026, time.July, 21, 9, 30, 0, 0, time.UTC))

	stablePrefix := systemPrompt(snapshot) + catalog
	experienceIndex := strings.Index(prompts.AgentSystem, `<eri_experience version="7">`)
	runtimeIndex := strings.Index(prompts.AgentSystem, "<current_runtime_context>")
	if !strings.HasPrefix(prompts.AgentSystem, stablePrefix) || experienceIndex < len(stablePrefix) || runtimeIndex <= experienceIndex {
		t.Fatalf("Experience is not a separate versioned System section after the reusable prefix: %s", prompts.AgentSystem)
	}
	if !strings.Contains(prompts.AgentSystem, experience.Text) || strings.Contains(prompts.JudgeContext, experience.Text) {
		t.Fatalf("Experience must guide generation without becoming the Judge rubric: %+v", prompts)
	}
}

func TestAssembleRunPromptsKeepsMemoryWorkflowOutOfJudgeContext(t *testing.T) {
	prompts := assembleRunPrompts(
		identity.Snapshot{Soul: "stable soul"},
		"",
		Experience{},
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
	if prompts.MemoryContext == nil {
		t.Fatal("Memory context is missing")
	}
	for _, prompt := range []string{prompts.MemoryContext.Content, prompts.JudgeContext} {
		if !strings.Contains(prompt, "The user prefers concise progress updates.") {
			t.Fatalf("prompt lost relevant memory evidence: %s", prompt)
		}
	}
	if strings.Contains(prompts.AgentSystem, "The user prefers concise progress updates.") || strings.Contains(prompts.AgentSystem, "operation=mark_used") {
		t.Fatalf("stable Agent System contains dynamic Memory: %s", prompts.AgentSystem)
	}
	if strings.Contains(prompts.MemoryContext.Content, "operation=mark_used") || strings.Contains(prompts.MemoryContext.Content, "call builtin.memory") {
		t.Fatalf("Memory evidence still carries a procedural mark-used instruction: %s", prompts.MemoryContext.Content)
	}
	if strings.Contains(prompts.MemoryContext.Content, "memory-1") || strings.Contains(prompts.MemoryContext.Content, "claim-1") {
		t.Fatalf("generation Memory exposed internal identities: %s", prompts.MemoryContext.Content)
	}
	if !strings.Contains(prompts.JudgeContext, "claim-1") {
		t.Fatalf("Judge context lost the bounded Claim identity: %s", prompts.JudgeContext)
	}
	if strings.Contains(prompts.JudgeContext, "operation=mark_used") || strings.Contains(prompts.JudgeContext, "call builtin.memory") {
		t.Fatalf("Judge context inherited generation-only memory workflow: %s", prompts.JudgeContext)
	}
}

func TestReplaceDeferredToolResultPreservesNativeToolFrame(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "Delegate the repository review."},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-delegate", Name: "delegate", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "call-delegate", Content: `{"success":true,"result":{"deferred":{"id":"delegation-1"}}}`},
	}
	if err := replaceDeferredToolResult(messages, "call-delegate", "engineering_team", "completed", []byte(`{"summary":"verified"}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateModelTranscript(messages); err != nil {
		t.Fatalf("replaced deferred result broke the native Tool frame: %v", err)
	}
	if messages[2].Role != "tool" || messages[2].ToolCallID != "call-delegate" || !strings.Contains(messages[2].Content, `"kind":"subagent_result"`) {
		t.Fatalf("unexpected resumed Tool result: %+v", messages[2])
	}
}

func TestReplaceSystemOverlaySupersedesOlderControlOfSameKind(t *testing.T) {
	messages := []Message{{Role: "user", Content: "continue"}}
	messages = replaceSystemOverlay(messages, "runtime_control", "change strategy")
	messages = replaceSystemOverlay(messages, "runtime_control", "synthesize now")
	messages = replaceSystemOverlay(messages, "evaluation_feedback", "missing evidence")
	if len(messages) != 3 || !strings.Contains(messages[1].Content, "synthesize now") || strings.Contains(messages[1].Content, "change strategy") {
		t.Fatalf("replaceable overlays accumulated or lost the latest state: %+v", messages)
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

func TestRestoreConversationWatermarkUsesLegacyInputAsConservativeBaseline(t *testing.T) {
	task := TaskContext{
		InputSequence: 9,
		CurrentTask:   execution.TaskCapsule{TaskID: "task", SourceRole: "user"},
	}
	continuation := pendingContinuation{State: loopState{InputSequence: 7}}
	restoreConversationWatermark(task, &continuation)
	if continuation.State.ConversationSequence != 7 || continuation.State.ContextManifest.ConversationSequence != 7 {
		t.Fatalf("legacy Conversation watermark=%+v", continuation.State)
	}

	continuation.State.ConversationSequence = 11
	continuation.State.ContextManifest.ConversationSequence = 11
	restoreConversationWatermark(task, &continuation)
	if continuation.State.ConversationSequence != 11 {
		t.Fatalf("current Conversation watermark was replaced: %+v", continuation.State)
	}
}

func TestEvalEscalationAsksUserWithoutContinuingTools(t *testing.T) {
	original := ModelRequest{
		Messages: []Message{{Role: "assistant", Content: "withheld candidate"}},
		Tools:    []ToolDefinition{{Name: "web"}},
	}
	repaired := evalRepairRequest(original, eval.Decision{
		Result: eval.Escalate, Findings: []string{"confirm the intended event"},
	})
	if len(repaired.Tools) != 0 {
		t.Fatalf("escalation still exposes tools: %+v", repaired.Tools)
	}
	if len(repaired.Messages) != 1 || repaired.Messages[0].Role != "system" || !strings.Contains(repaired.Messages[0].Content, "entire next candidate must be exactly one focused") {
		t.Fatalf("repair request = %+v", repaired.Messages)
	}
	if len(original.Tools) != 1 || len(original.Messages) != 1 {
		t.Fatalf("original request was mutated: %+v", original)
	}
}
