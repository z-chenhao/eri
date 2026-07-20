package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/observability"
)

type newsEvalRepairModel struct {
	integrationModelCapabilities
	mu         sync.Mutex
	agentCalls int
	judgeCalls int
}

func (m *newsEvalRepairModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage := agent.Usage{Provider: "fake", Model: "news-eval-repair", ModelCalls: 1}
	if strings.Contains(request.System, "<eri_eval_judge>") {
		m.judgeCalls++
		if m.judgeCalls == 1 {
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"result":"repair","tier":"substantive","findings":["The candidate promises ongoing delivery without a confirmed durable commitment."]}`}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"result":"pass","tier":"substantive","findings":[]}`}, FinishReason: "stop", Usage: usage}, nil
	}
	m.agentCalls++
	switch m.agentCalls {
	case 1:
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I will track AI news every day and send it to you."}, FinishReason: "stop", Usage: usage}, nil
	case 2:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "system" || !strings.Contains(last.Content, "durable commitment") {
			return agent.ModelResponse{}, fmt.Errorf("task Eval repair instruction missing: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "news-commitment", Name: "builtin_commitments", Arguments: json.RawMessage(`{
				"operation":"create","message":"Track important AI news and send summaries of material changes",
				"schedule":{"type":"daily","daily_time":"09:00","timezone":"Asia/Shanghai"},"importance":"normal",
				"delivery_route":"recent_channel"
			}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case 3:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "news-commitment" || !strings.Contains(last.Content, `"success":true`) {
			return agent.ModelResponse{}, fmt.Errorf("durable commitment result missing: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Daily AI news tracking is scheduled for 09:00 Asia/Shanghai; I will only send material changes."}, FinishReason: "stop", Usage: usage}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected news agent call %d", m.agentCalls)
	}
}

type evidenceScenarioModel struct {
	integrationModelCapabilities
	mu          sync.Mutex
	calls       int
	searchCount int
	final       string
}

type skillActivationModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
}

type explicitSkillModel struct{ integrationModelCapabilities }

func (explicitSkillModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "explicit-skill", ModelCalls: 1}
	if strings.Contains(request.System, "<eri_eval_judge>") {
		last := request.Messages[len(request.Messages)-1].Content
		if !strings.Contains(last, `"selected_skills":["writing-delivery"]`) {
			return agent.ModelResponse{}, fmt.Errorf("Judge did not receive explicitly activated skill: %s", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"result":"pass","tier":"routine","findings":[]}`}, FinishReason: "stop", Usage: usage}, nil
	}
	found := false
	for _, message := range request.Messages {
		if message.Role == "system" && strings.Contains(message.Content, "The user explicitly activated") && strings.Contains(message.Content, "Infer or establish purpose") {
			found = true
		}
	}
	if !found {
		return agent.ModelResponse{}, fmt.Errorf("explicit SKILL.md content was not injected")
	}
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "This is a usable draft produced with the requested writing skill."}, FinishReason: "stop", Usage: usage}, nil
}

func (m *skillActivationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage := agent.Usage{Provider: "fake", Model: "skill-activation", ModelCalls: 1}
	if strings.Contains(request.System, "<eri_eval_judge>") {
		last := request.Messages[len(request.Messages)-1].Content
		if !strings.Contains(last, `"selected_skills":["research-decision"]`) {
			return agent.ModelResponse{}, fmt.Errorf("Judge did not receive activated skill: %s", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"result":"pass","tier":"substantive","findings":[]}`}, FinishReason: "stop", Usage: usage}, nil
	}
	m.calls++
	switch m.calls {
	case 1:
		if !strings.Contains(request.System, "<available_skills>") || !strings.Contains(request.System, "Evidence-led comparison") {
			return agent.ModelResponse{}, fmt.Errorf("skill catalog metadata missing from system prompt")
		}
		if strings.Contains(request.System, "Turn the decision into hard constraints") {
			return agent.ModelResponse{}, fmt.Errorf("SKILL.md body was eagerly injected")
		}
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Name == "builtin_skills"
		}
		if !found {
			return agent.ModelResponse{}, fmt.Errorf("generic skill loader tool is missing")
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "load-research", Name: "builtin_skills", Arguments: json.RawMessage(`{"operation":"load","name":"research-decision"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case 2:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "load-research" || !strings.Contains(last.Content, "Turn the decision into hard constraints") {
			return agent.ModelResponse{}, fmt.Errorf("activated skill body missing from tool result: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I will expand the candidate set, compare constraints, cost, risk, and ROI, then recommend the best options."}, FinishReason: "stop", Usage: usage}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected skill activation call %d", m.calls)
	}
}

func (m *evidenceScenarioModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage := agent.Usage{Provider: "fake", Model: "evidence-scenario", ModelCalls: 1}
	if strings.Contains(request.System, "<eri_eval_judge>") {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: `{"result":"pass","tier":"substantive","findings":[]}`}, FinishReason: "stop", Usage: usage}, nil
	}
	m.calls++
	if m.calls == 1 {
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Name == "builtin_web"
		}
		if !found {
			return agent.ModelResponse{}, fmt.Errorf("web descriptor missing")
		}
		calls := make([]agent.ToolCall, 0, m.searchCount)
		for index := 0; index < m.searchCount; index++ {
			arguments, err := json.Marshal(map[string]any{"operation": "search", "query": fmt.Sprintf("comparison window %d", index+1), "limit": 5})
			if err != nil {
				return agent.ModelResponse{}, err
			}
			calls = append(calls, agent.ToolCall{ID: fmt.Sprintf("web-%d", index+1), Name: "builtin_web", Arguments: arguments})
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls", Usage: usage}, nil
	}
	confirmed := 0
	for _, message := range request.Messages {
		if message.Role == "tool" && strings.Contains(message.Content, `"success":true`) {
			confirmed++
		}
	}
	if confirmed != m.searchCount {
		return agent.ModelResponse{}, fmt.Errorf("confirmed web observations = %d, want %d", confirmed, m.searchCount)
	}
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: m.final}, FinishReason: "stop", Usage: usage}, nil
}

func TestDaemonNewsTrackingEvalRepairsBeforeDelivery(t *testing.T) {
	model := &newsEvalRepairModel{}
	d, client, stop := startEvalScenario(t, model, nil, false, "")
	defer stop()
	sent, timeline := sendEvalScenario(t, client, "Track AI news every day and send me updates")
	if len(timeline.Messages) != 2 || strings.Contains(timeline.Messages[1].Content, "I will track") {
		t.Fatalf("withheld candidate leaked into conversation: %+v", timeline.Messages)
	}
	detail := loadScenarioRun(t, d, sent.TaskID)
	if detail.Run.ModelCalls != 5 || len(detail.Effects) != 1 || detail.Effects[0].ToolID != "builtin.commitments" || detail.Effects[0].Status != "confirmed" {
		t.Fatalf("news run did not repair through the ordinary Agent Loop: %+v", detail)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Eval != "pass" || detail.Artifacts[0].EvalTier != "substantive" || detail.Artifacts[0].EvalEvaluator != "llm_judge" {
		t.Fatalf("news task Eval record = %+v", detail.Artifacts)
	}
	if detail.Artifacts[0].EvalFindingCount == 0 {
		t.Fatalf("repair history was not retained in final Eval record: %+v", detail.Artifacts[0])
	}
	assertDataRootDoesNotContain(t, d.config.DataRoot, []byte("The candidate promises ongoing delivery without a confirmed durable commitment."))
}

func TestDaemonModelActivatesStandardAgentSkillOnDemand(t *testing.T) {
	model := &skillActivationModel{}
	d, client, stop := startEvalScenario(t, model, nil, false, "")
	defer stop()
	sent, timeline := sendEvalScenario(t, client, "Compare three options and recommend the one with the highest ROI")
	if len(timeline.Messages) != 2 || !strings.Contains(timeline.Messages[1].Content, "ROI") {
		t.Fatalf("unexpected delivered result: %+v", timeline.Messages)
	}
	detail := loadScenarioRun(t, d, sent.TaskID)
	if detail.Run.ModelCalls != 3 || len(detail.Effects) != 1 || detail.Effects[0].ToolID != "builtin.skills" || detail.Effects[0].Status != "confirmed" {
		t.Fatalf("skill activation run = %+v", detail)
	}
	if len(detail.Invocations) != 1 {
		t.Fatalf("invocations = %+v", detail.Invocations)
	}
	activated := detail.Invocations[0].ContextManifest.SkillIDs
	if len(activated) != 1 || activated[0] != "research-decision" {
		t.Fatalf("activated skills manifest = %+v", detail.Invocations[0].ContextManifest)
	}
}

func TestDaemonUserCanExplicitlyActivateStandardAgentSkill(t *testing.T) {
	d, client, stop := startEvalScenario(t, explicitSkillModel{}, nil, false, "")
	defer stop()
	sent, timeline := sendEvalScenario(t, client, "Use $writing-delivery to turn these points into an email")
	if len(timeline.Messages) != 2 || !strings.Contains(timeline.Messages[1].Content, "usable draft") {
		t.Fatalf("unexpected delivered result: %+v", timeline.Messages)
	}
	detail := loadScenarioRun(t, d, sent.TaskID)
	if detail.Run.ModelCalls != 2 || len(detail.Effects) != 0 {
		t.Fatalf("explicit skill should not require a loader tool call: %+v", detail)
	}
}

func TestDaemonResearchAndTravelUseMultipleConfirmedWebObservations(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search" || request.Header.Get("Authorization") != "Bearer test-tavily-key" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var payload struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "test-request-" + payload.Query,
			"results": []map[string]any{
				{"title": "Primary source", "url": "https://example.com/primary-" + payload.Query, "content": "Primary evidence for " + payload.Query, "score": 0.9},
				{"title": "Independent source", "url": "https://example.org/independent-" + payload.Query, "content": "Independent evidence for " + payload.Query, "score": 0.8},
			},
		})
	}))
	defer search.Close()

	tests := []struct {
		name        string
		prompt      string
		searchCount int
		final       string
	}{
		{
			name: "research decision", prompt: "Research and compare three personal knowledge-base options and recommend the highest-ROI choice", searchCount: 2,
			final: "I verified two independent sources. Option A currently has the highest ROI; migration cost is the main objection, and B is better if collaboration becomes a hard requirement. Sources: https://example.com/primary-a and https://example.org/independent-b",
		},
		{
			name: "travel global search", prompt: "Compare multiple date windows and recommend flight and hotel combinations from Shanghai to Tokyo", searchCount: 3,
			final: "I compared three date windows and their hotel combinations. Window two has the best ROI across total cost, door-to-door time, and comfort; window one is cheaper but requires an early flight. Prices must be rechecked before payment. Sources: https://example.com/primary-travel and https://example.org/independent-hotel",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &evidenceScenarioModel{searchCount: test.searchCount, final: test.final}
			d, client, stop := startEvalScenario(t, model, search.Client(), true, search.URL)
			defer stop()
			sent, timeline := sendEvalScenario(t, client, test.prompt)
			if len(timeline.Messages) != 2 || timeline.Messages[1].Content != test.final {
				t.Fatalf("unexpected delivered result: %+v", timeline.Messages)
			}
			detail := loadScenarioRun(t, d, sent.TaskID)
			if detail.Run.ModelCalls != 3 || len(detail.Effects) != test.searchCount {
				t.Fatalf("run evidence counts = %+v", detail)
			}
			for _, effect := range detail.Effects {
				if effect.ToolID != "builtin.web" || effect.Status != "confirmed" {
					t.Fatalf("unconfirmed web evidence: %+v", effect)
				}
			}
			if len(detail.Artifacts) != 1 || detail.Artifacts[0].Eval != "pass" || detail.Artifacts[0].EvalTier != "substantive" {
				t.Fatalf("substantive Eval record = %+v", detail.Artifacts)
			}
		})
	}
}

func startEvalScenario(t *testing.T, model agent.Model, webClient *http.Client, allowPrivate bool, tavilyEndpoint string) (*Daemon, *http.Client, func()) {
	t.Helper()
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-eval-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", Model: "fake",
		ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
		TavilyKeySet: tavilyEndpoint != "", TavilySearchDepth: "basic", TavilyExtractDepth: "basic",
	}
	if tavilyEndpoint != "" {
		t.Setenv("TAVILY_API_KEY", "test-tavily-key")
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x73}, 32), Model: model, WebClient: webClient,
		TavilyEndpoint: tavilyEndpoint, AllowPrivateWeb: allowPrivate,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
		}
		d.Close()
	}
	return d, integrationUnixClient(socketPath), stop
}

func sendEvalScenario(t *testing.T, client *http.Client, prompt string) (channel.SendResult, struct {
	Messages []channel.Message `json:"messages"`
}) {
	t.Helper()
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": prompt}, &sent)
	deadline := time.Now().Add(5 * time.Second)
	var status channel.TaskStatus
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
		if status.Status == "completed" || status.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "completed" {
		t.Fatalf("scenario task = %+v", status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	return sent, timeline
}

func loadScenarioRun(t *testing.T, d *Daemon, taskID string) observability.RunDetail {
	t.Helper()
	runs, err := d.store.ListRuns(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.TaskID != taskID {
			continue
		}
		detail, found, err := d.store.LoadRun(context.Background(), run.ID)
		if err != nil || !found {
			t.Fatalf("load run %s: found=%t err=%v", run.ID, found, err)
		}
		return detail
	}
	t.Fatalf("run for task %s not found", taskID)
	return observability.RunDetail{}
}
