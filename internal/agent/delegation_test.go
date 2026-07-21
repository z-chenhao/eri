package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/subagent"
	"github.com/z-chenhao/eri/internal/tool"
)

type nativeTestRepository struct {
	queued subagent.Run
}

func (r *nativeTestRepository) QueueSubagentRun(_ context.Context, run subagent.Run) (subagent.Run, bool, error) {
	r.queued = run
	run.Status = "queued"
	return run, true, nil
}
func (*nativeTestRepository) LoadSubagentRun(context.Context, string) (subagent.Run, bool, error) {
	return subagent.Run{}, false, nil
}
func (*nativeTestRepository) MarkSubagentRunStarting(context.Context, string) error { return nil }
func (*nativeTestRepository) MarkSubagentRunRunning(context.Context, string, string) error {
	return nil
}
func (*nativeTestRepository) SaveSubagentRuntimeState(context.Context, string, content.Ref) error {
	return nil
}
func (*nativeTestRepository) SubagentRunCancellationRequested(context.Context, string) (bool, error) {
	return false, nil
}
func (*nativeTestRepository) CompleteSubagentRun(context.Context, string, string, string, content.Ref) (bool, error) {
	return true, nil
}

type nativeTestContent struct{ body []byte }

func (c *nativeTestContent) Put(_ context.Context, body []byte, _ content.Metadata) (content.Ref, error) {
	c.body = append([]byte(nil), body...)
	return content.Ref{ObjectID: "0123456789abcdef0123456789abcdef", Version: 1}, nil
}
func (c *nativeTestContent) Get(context.Context, content.Ref) ([]byte, error) {
	return append([]byte(nil), c.body...), nil
}
func (*nativeTestContent) Delete(context.Context, content.Ref) error { return nil }

type nativeTestModel struct{}

func (*nativeTestModel) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{Message: Message{Role: "assistant", Content: "done"}}, nil
}
func (*nativeTestModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return ModelCapabilities{Text: true, ToolCalling: true, ContextTokens: 32768, MaxOutputTokens: 4096}, nil
}

type compactionCapturingModel struct{ request ModelRequest }

func (m *compactionCapturingModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.request = request
	return ModelResponse{Message: Message{Role: "assistant", Content: "bounded summary"}}, nil
}

func (*compactionCapturingModel) Capabilities(context.Context) (ModelCapabilities, error) {
	return ModelCapabilities{Text: true, ToolCalling: true, ContextTokens: 16_384, MaxOutputTokens: 4096}, nil
}

func TestDelegationCompactionDoesNotPromoteReasoningContentIntoSummary(t *testing.T) {
	model := &compactionCapturingModel{}
	messages := []Message{{
		Role: "assistant", Content: strings.Repeat("evidence ", 800),
		ReasoningContent: "provider-private-reasoning-marker",
	}}
	for range 3 {
		messages = append(messages, Message{Role: "user", Content: strings.Repeat("older evidence ", 500)})
	}
	for range 7 {
		messages = append(messages, Message{Role: "user", Content: "recent evidence"})
	}
	messages = append(messages, Message{Role: "assistant", Content: "recent evidence", ReasoningContent: "recent-provider-continuation"})
	request := ModelRequest{Messages: messages, MaxOutputTokens: 1024}
	usage := Usage{}
	capabilities, _ := model.Capabilities(context.Background())
	if err := compactDelegationContext(context.Background(), model, capabilities, &request, &usage); err != nil {
		t.Fatal(err)
	}
	if len(model.request.Messages) != 1 || strings.Contains(model.request.Messages[0].Content, "provider-private-reasoning-marker") {
		t.Fatalf("compaction promoted reasoning_content: %+v", model.request.Messages)
	}
	if request.Messages[len(request.Messages)-1].ReasoningContent != "recent-provider-continuation" {
		t.Fatalf("compaction removed continuation from retained provider context: %+v", request.Messages)
	}
}

type nativeTestGateway struct{ descriptors []tool.Descriptor }

func (g *nativeTestGateway) Descriptors() []tool.Descriptor { return g.descriptors }
func (*nativeTestGateway) Invoke(context.Context, tool.Request) (tool.Outcome, error) {
	return tool.Outcome{}, nil
}

func TestNativeInternQueuesAsynchronousRun(t *testing.T) {
	repository := &nativeTestRepository{}
	contentStore := &nativeTestContent{}
	provider, err := NewNativeSubagent(repository, contentStore, &nativeTestModel{}, &nativeTestGateway{}, nil, 512, false, "test:model", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := provider.Prepare(context.Background(), subagent.Request{
		DelegationID: "delegation-1", TaskID: "task-1", RunID: "run-1", RoleID: "intern", ProviderID: "eri_native", Objective: "verify one fact",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := provider.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Deferred || outcome.Ticket == nil || outcome.Ticket.RoleID != "intern" || outcome.Ticket.ProviderID != "eri_native" || outcome.Ticket.Execution != subagent.Background {
		t.Fatalf("outcome = %+v", outcome)
	}
	if repository.queued.RoleID != "intern" || repository.queued.ProviderID != "eri_native" {
		t.Fatalf("queued run = %+v", repository.queued)
	}
	var stored subagent.Request
	if err := json.Unmarshal(contentStore.body, &stored); err != nil || stored.Context != "" {
		t.Fatalf("stored scoped request=%+v err=%v", stored, err)
	}
}

func TestNativeCapabilityViewKeepsMixedToolsButEnforcesReadOnlyScope(t *testing.T) {
	descriptors := nativeToolDescriptors([]tool.Descriptor{
		{ID: "builtin.files", AllowedEffects: []policy.EffectClass{policy.ReadOnly, policy.Reversible}},
		{ID: "builtin.terminal", AllowedEffects: []policy.EffectClass{policy.ReadOnly, policy.Privileged}},
		{ID: "builtin.delegate", AllowedEffects: []policy.EffectClass{policy.ReadOnly}},
		{ID: "builtin.notification", AllowedEffects: []policy.EffectClass{policy.Communication}},
		{ID: "builtin.memory", AllowedEffects: []policy.EffectClass{policy.ReadOnly, policy.Reversible}},
	})
	if len(descriptors) != 2 || descriptors[0].ID != "builtin.files" || descriptors[1].ID != "builtin.terminal" {
		t.Fatalf("native descriptors = %+v", descriptors)
	}
	scope := nativeCapabilityScope(descriptors)
	if scope.AllowApproval {
		t.Fatal("Intern scope can request approval")
	}
	if _, ok := scope.AllowedEffects[policy.ReadOnly]; !ok || len(scope.AllowedEffects) != 1 {
		t.Fatalf("allowed effects = %+v", scope.AllowedEffects)
	}
	for _, forbidden := range []string{"builtin.delegate", "builtin.notification", "builtin.memory"} {
		if _, ok := scope.AllowedToolIDs[forbidden]; ok {
			t.Fatalf("forbidden capability %q is present", forbidden)
		}
	}
}
