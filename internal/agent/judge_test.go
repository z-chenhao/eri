package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type judgeModelFunc func(context.Context, ModelRequest) (ModelResponse, error)

func (fn judgeModelFunc) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	return fn(ctx, request)
}

func (judgeModelFunc) Capabilities(context.Context) (ModelCapabilities, error) {
	return testModelCapabilities(), nil
}

func decodeJudgeEnvelope(t *testing.T, request ModelRequest) judgeEnvelope {
	t.Helper()
	if len(request.Messages) != 1 || request.Messages[0].Role != "user" {
		t.Fatalf("Judge messages = %+v, want one independent user evaluation envelope", request.Messages)
	}
	var envelope judgeEnvelope
	if err := json.Unmarshal([]byte(request.Messages[0].Content), &envelope); err != nil {
		t.Fatalf("decode Judge envelope: %v", err)
	}
	return envelope
}

func TestJudgeTreatsFocusedClarificationAsDeliverable(t *testing.T) {
	for _, required := range []string{
		"including a focused question that asks only for the smallest material missing input",
		"Never hold merely because input is required",
		"If it already asks the question cleanly, choose pass",
		"asks several downstream questions is not ready",
	} {
		if !strings.Contains(judgeSystemPrompt, required) {
			t.Fatalf("judge prompt is missing clarification rule %q", required)
		}
	}
}

func TestJudgeRequiresDurableReceiptsForExplicitFeedback(t *testing.T) {
	for _, required := range []string{
		"confirmed_tools includes feedback",
		"require memory too",
		"A prose acknowledgment or promise is not a Receipt",
		"never keyword matching",
		"lack of inspection is not proof that an action did not occur",
		"Treat requested brevity as a real constraint",
	} {
		if !strings.Contains(judgeSystemPrompt+interpersonalJudgePrompt, required) {
			t.Fatalf("judge prompt is missing feedback rule %q", required)
		}
	}
}

func TestModelJudgeUsesTranscriptSkillsAndConfirmedTools(t *testing.T) {
	model := judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		if len(request.Tools) != 0 || !request.JSONOutput || !strings.Contains(request.System, "<eri_eval_judge>") {
			t.Fatalf("judge request can call tools or lacks rubric: %+v", request)
		}
		if strings.Contains(request.System, "stable candidate context") || strings.Contains(request.System, "<agent_operating_rules>") {
			t.Fatalf("judge inherited the wrong system role: %s", request.System)
		}
		envelope := decodeJudgeEnvelope(t, request)
		if len(envelope.Transcript) != 2 || envelope.Transcript[len(envelope.Transcript)-1].Role != "assistant" || envelope.Transcript[len(envelope.Transcript)-1].Content != "choose A" ||
			envelope.CandidateContext != "stable candidate context" || len(envelope.EvaluationContext.SelectedSkills) != 1 || envelope.EvaluationContext.SelectedSkills[0] != "research-decision@1.0.0" ||
			len(envelope.EvaluationContext.ConfirmedTools) != 1 || envelope.EvaluationContext.ConfirmedTools[0] != "web" ||
			len(envelope.EvaluationContext.MemoryClaimIDs) != 1 || envelope.EvaluationContext.MemoryClaimIDs[0] != "claim-preference" {
			t.Fatalf("judge envelope=%+v system=%s", envelope, request.System)
		}
		return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"repair","tier":"substantive","findings":["The recommendation is not grounded in the confirmed observation."],"applied_memory_claims":["claim-preference"]}`}, Usage: Usage{ModelCalls: 1}}, nil
	})
	judge, err := NewModelJudge(model)
	if err != nil {
		t.Fatal(err)
	}
	decision, usage, err := judge.Evaluate(context.Background(), JudgeRequest{
		CandidateContext: "stable candidate context", TaskText: "compare options", SkillIDs: []string{"research-decision@1.0.0"},
		ConfirmedTools: []string{"builtin.web"}, MemoryClaimIDs: []string{"claim-preference"},
		Messages: []Message{{Role: "user", Content: "compare"}, {Role: "assistant", Content: "choose A"}},
	})
	if err != nil || decision.Result != "repair" || decision.Tier != "substantive" || len(decision.Findings) != 1 || len(decision.AppliedMemoryClaims) != 1 || usage.ModelCalls != 1 {
		t.Fatalf("decision=%+v usage=%+v err=%v", decision, usage, err)
	}
}

func TestModelJudgeCanEvaluateInterpersonalFitWithoutRequiringWarmthEverywhere(t *testing.T) {
	model := judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		for _, required := range []string{
			"<soul_guided_response_eval>",
			"Pass a direct, purely task-focused answer",
			"quiet, sincere, observant, low in dominance",
			"state or change, exception, deadline, decision, recommendation and next action",
			"Private replies should be compact",
			"never appear sent without a confirmed Receipt",
			"customer-service scripted",
			"never promise action it cannot perform",
			"Acknowledgment must not displace the next useful action",
			"Style never overrides facts",
		} {
			if !strings.Contains(request.System, required) {
				t.Fatalf("interpersonal judge prompt is missing %q", required)
			}
		}
		return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"pass","tier":"routine","findings":[]}`}}, nil
	})
	judge, err := NewModelJudge(model)
	if err != nil {
		t.Fatal(err)
	}
	decision, _, err := judge.Evaluate(context.Background(), JudgeRequest{
		CandidateContext: "stable candidate context", Messages: []Message{{Role: "user", Content: "Is it fixed?"}, {Role: "assistant", Content: "It is fixed and the tests pass."}},
		SoulGuidedResponse: true,
	})
	if err != nil || decision.Result != "pass" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestModelJudgeFailsClosedOnInvalidProtocol(t *testing.T) {
	tests := []struct {
		name     string
		response ModelResponse
	}{
		{name: "not json", response: ModelResponse{Message: Message{Content: "looks fine"}}},
		{name: "unknown result", response: ModelResponse{Message: Message{Content: `{"result":"maybe","tier":"routine","findings":[]}`}}},
		{name: "finding required", response: ModelResponse{Message: Message{Content: `{"result":"repair","tier":"routine","findings":[]}`}}},
		{name: "pass contradicts finding", response: ModelResponse{Message: Message{Content: `{"result":"pass","tier":"routine","findings":["The claim is not confirmed by the Tool result."]}`}}},
		{name: "unknown memory claim", response: ModelResponse{Message: Message{Content: `{"result":"pass","tier":"routine","findings":[],"applied_memory_claims":["claim-not-supplied"]}`}}},
		{name: "tool call", response: ModelResponse{Message: Message{ToolCalls: []ToolCall{{ID: "x", Name: "builtin.web", Arguments: []byte(`{}`)}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			judge, _ := NewModelJudge(judgeModelFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
				return test.response, nil
			}))
			if _, _, err := judge.Evaluate(context.Background(), JudgeRequest{Messages: []Message{{Role: "assistant", Content: "candidate"}}}); err == nil {
				t.Fatal("invalid LLM Judge output was accepted")
			}
		})
	}
}

func TestModelJudgeLetsTheModelRepairItsOwnInvalidProtocol(t *testing.T) {
	calls := 0
	judge, _ := NewModelJudge(judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		calls++
		if calls == 1 {
			return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"pass","tier":"substance","findings":[]}`}, Usage: Usage{ModelCalls: 1}}, nil
		}
		envelope := decodeJudgeEnvelope(t, request)
		last := envelope.Transcript[len(envelope.Transcript)-1]
		if last.Role != "assistant" || last.Content != "candidate" || !request.JSONOutput || !strings.Contains(request.System, "<judge_protocol_repair>") ||
			!strings.Contains(request.System, "required response protocol") || strings.Contains(request.System, "map substance to substantive") {
			t.Fatalf("generic Judge repair request = %+v", request)
		}
		return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"pass","tier":"substantive","findings":[]}`}, Usage: Usage{ModelCalls: 1}}, nil
	}))
	decision, usage, err := judge.Evaluate(context.Background(), JudgeRequest{Messages: []Message{{Role: "assistant", Content: "candidate"}}})
	if err != nil || decision.Result != "pass" || decision.Tier != "substantive" || calls != 2 || usage.ModelCalls != 2 {
		t.Fatalf("decision=%+v usage=%+v calls=%d err=%v", decision, usage, calls, err)
	}
}

func TestModelJudgeRepairsEmptyStructuredOutputWithModifiedStructuredPrompt(t *testing.T) {
	calls := 0
	initialSystem := ""
	initialEnvelope := ""
	messages := []Message{
		{Role: "user", Content: "record the inert marker"},
		{Role: "assistant", ReasoningContent: "the governed tool is required", ToolCalls: []ToolCall{{ID: "call-1", Name: "record_marker", Arguments: []byte(`{}`)}}},
		{Role: "tool", ToolCallID: "call-1", Content: `{"recorded":true}`},
		{Role: "assistant", Content: "candidate"},
	}
	judge, _ := NewModelJudge(judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		calls++
		envelope := decodeJudgeEnvelope(t, request)
		if len(envelope.Transcript) != len(messages) || envelope.Transcript[1].ReasoningContent != "" || len(envelope.Transcript[1].ToolCalls) != 1 {
			t.Fatalf("Judge evidence projection = %+v", envelope.Transcript)
		}
		if calls == 1 {
			if !request.JSONOutput {
				t.Fatal("first Judge attempt must request native structured output")
			}
			initialSystem = request.System
			initialEnvelope = request.Messages[0].Content
			return ModelResponse{Message: Message{Role: "assistant", ReasoningContent: "private provider reasoning"}, Usage: Usage{ModelCalls: 1}}, nil
		}
		if !request.JSONOutput {
			t.Fatal("empty native structured output must retain native JSON mode with thinking")
		}
		last := envelope.Transcript[len(envelope.Transcript)-1]
		if last.Role != "assistant" || last.Content != "candidate" || request.Messages[0].Content != initialEnvelope || request.System == initialSystem || !strings.Contains(request.System, "<judge_protocol_repair>") || !strings.Contains(request.System, "empty response") {
			t.Fatalf("Judge retry did not modify only the System repair overlay: %+v", request)
		}
		for _, message := range envelope.Transcript {
			if message.ReasoningContent == "private provider reasoning" {
				t.Fatal("Judge retry reused provider-private reasoning from the rejected response")
			}
		}
		return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"pass","tier":"routine","findings":[]}`}, Usage: Usage{ModelCalls: 1}}, nil
	}))
	decision, usage, err := judge.Evaluate(context.Background(), JudgeRequest{Messages: messages})
	if err != nil || decision.Result != "pass" || calls != 2 || usage.ModelCalls != 2 {
		t.Fatalf("decision=%+v usage=%+v calls=%d err=%v", decision, usage, calls, err)
	}
}

func TestModelJudgeFailsClosedAfterEmptyOutputRecoveryIsExhausted(t *testing.T) {
	calls := 0
	judge, _ := NewModelJudge(judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		calls++
		if !request.JSONOutput {
			t.Fatal("every Judge attempt must retain native structured output")
		}
		if calls > 1 && (!strings.Contains(request.System, "<judge_protocol_repair>") || !strings.Contains(request.System, "empty response")) {
			t.Fatal("empty structured output recovery must modify the Judge System prompt")
		}
		return ModelResponse{Message: Message{Role: "assistant"}, Usage: Usage{ModelCalls: 1}}, nil
	}))
	_, usage, err := judge.Evaluate(context.Background(), JudgeRequest{Messages: []Message{{Role: "assistant", Content: "candidate"}}})
	if err == nil || !strings.Contains(err.Error(), "empty response") || calls != judgeProtocolAttempts || usage.ModelCalls != judgeProtocolAttempts {
		t.Fatalf("usage=%+v calls=%d err=%v", usage, calls, err)
	}
}

func TestModelJudgeProjectsTranscriptAsReasoningFreeEvaluationEvidence(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "use the governed lookup"},
		{Role: "assistant", ReasoningContent: "tool continuation state", ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: []byte(`{}`)}}},
		{Role: "tool", ToolCallID: "call-1", Content: `{"confirmed":true}`},
		{Role: "assistant", Content: "intermediate answer", ReasoningContent: "private intermediate reasoning"},
		{Role: "user", Content: "give me the conclusion"},
		{Role: "assistant", Content: "candidate", ReasoningContent: "private candidate reasoning"},
	}
	judge, _ := NewModelJudge(judgeModelFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		envelope := decodeJudgeEnvelope(t, request)
		for _, index := range []int{1, 3, 5} {
			if got := envelope.Transcript[index].ReasoningContent; got != "" {
				t.Fatalf("reasoning_content at message %d reached Judge: %q", index, got)
			}
		}
		if len(envelope.Transcript[1].ToolCalls) != 1 || envelope.Transcript[2].ToolCallID != "call-1" {
			t.Fatalf("Judge evidence lost Tool frame: %+v", envelope.Transcript)
		}
		return ModelResponse{Message: Message{Role: "assistant", Content: `{"result":"pass","tier":"routine","findings":[],"applied_memory_claims":[]}`}}, nil
	}))
	if _, _, err := judge.Evaluate(context.Background(), JudgeRequest{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	if messages[3].ReasoningContent != "private intermediate reasoning" || messages[5].ReasoningContent != "private candidate reasoning" {
		t.Fatal("Judge projection mutated the encrypted provider transcript")
	}
}

func TestModelJudgeRejectsMissingCandidateBeforeCallingProvider(t *testing.T) {
	judge, _ := NewModelJudge(judgeModelFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
		return ModelResponse{}, errors.New("provider unavailable")
	}))
	if _, _, err := judge.Evaluate(context.Background(), JudgeRequest{}); err == nil || !strings.Contains(err.Error(), "final assistant Candidate") {
		t.Fatalf("judge error = %v", err)
	}
}
