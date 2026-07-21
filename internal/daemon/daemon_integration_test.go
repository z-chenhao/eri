package daemon

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/codex"
	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/plugin"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/scheduler"
	assistanttask "github.com/z-chenhao/eri/internal/task"
)

type testPassJudge struct{}

func TestPrepareDataRootRejectsFilesystemRootAndRepairsDirectoryPermissions(t *testing.T) {
	if err := PrepareDataRoot(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root was accepted as EriDataRoot")
	}
	root := filepath.Join(t.TempDir(), "eri-data")
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataRoot(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, logs, filepath.Join(root, "metadata")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat private data directory %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("private data directory %s mode=%v", path, info.Mode().Perm())
		}
	}
}

func TestPrepareDataRootRejectsGovernedDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission fixture is Unix-specific")
	}
	root := filepath.Join(t.TempDir(), "eri-data")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataRoot(root); err == nil {
		t.Fatal("governed data directory symlink was accepted")
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("outside directory permissions changed to %v", info.Mode().Perm())
	}
}

func TestDaemonInitializationFailureIsWrittenToStructuredLog(t *testing.T) {
	root := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	_, err := New(context.Background(), config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"),
		SocketPath: filepath.Join(root, "runtime", "eri.sock"), ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		ModelProvider: "ollama", Model: "test", WorkspaceRoot: t.TempDir(), MCPServersJSON: "not-json",
	}, Dependencies{MasterKey: bytes.Repeat([]byte{0x51}, 32), Model: integrationModel{}, Logger: logger})
	if err == nil {
		t.Fatal("invalid MCP configuration initialized successfully")
	}
	logged := logs.String()
	if !strings.Contains(logged, "Eri daemon initialization failed") || !strings.Contains(logged, `"component":"daemon"`) || !strings.Contains(logged, `"error_code":`) {
		t.Fatalf("initialization log is incomplete: %s", logged)
	}
}

func TestDaemonRunFailureIsWrittenToStructuredLog(t *testing.T) {
	root := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	d, err := New(context.Background(), config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"),
		SocketPath: filepath.Join(root, "runtime", "eri.sock"), ConversationAddr: "not-a-loopback-address", ObservatoryAddr: "127.0.0.1:0",
		ModelProvider: "ollama", Model: "test", WorkspaceRoot: t.TempDir(),
	}, Dependencies{MasterKey: bytes.Repeat([]byte{0x52}, 32), Model: integrationModel{}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("invalid runtime address started successfully")
	}
	logged := logs.String()
	for _, required := range []string{`"msg":"Eri daemon run failed"`, `"component":"daemon"`, `"error_code":"operation_failed"`, `"msg":"Eri daemon stopped"`} {
		if !strings.Contains(logged, required) {
			t.Fatalf("runtime failure log is missing %q: %s", required, logged)
		}
	}
}

func (testPassJudge) Evaluate(context.Context, agent.JudgeRequest) (eval.Decision, agent.Usage, error) {
	return eval.Decision{Result: eval.Pass, Tier: "routine"}, agent.Usage{Provider: "fake", Model: "judge", ModelCalls: 1}, nil
}

type integrationModelCapabilities struct{}

func (integrationModelCapabilities) Capabilities(context.Context) (agent.ModelCapabilities, error) {
	return agent.ModelCapabilities{
		Text: true, ToolCalling: true, ParallelToolCalls: true, Usage: true,
		Cancellation: true, ContextTokens: 32_768, MaxOutputTokens: 4_096,
		DataResidency: "test",
	}, nil
}

type integrationModel struct{ integrationModelCapabilities }

func (integrationModel) Complete(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{
		Message: agent.Message{Role: "assistant", Content: "Complete closed-loop response"}, FinishReason: "stop",
		Usage: agent.Usage{Provider: "fake", Model: "integration", InputTokens: 10, OutputTokens: 6, ModelCalls: 1},
	}, nil
}

type codexDelegationIntegrationModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
}

type nativeDelegationIntegrationModel struct {
	integrationModelCapabilities
	mu           sync.Mutex
	primaryCalls int
}

func (m *nativeDelegationIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "native-delegation", ModelCalls: 1}
	if strings.Contains(request.System, "private Intern") {
		for _, definition := range request.Tools {
			for _, forbidden := range []string{"builtin_delegate", "builtin_notification", "builtin_memory"} {
				if definition.Name == forbidden {
					return agent.ModelResponse{}, fmt.Errorf("Intern received forbidden capability %q", forbidden)
				}
			}
		}
		if strings.Contains(request.System, "soul_guided_response") || len(request.Messages) != 1 {
			return agent.ModelResponse{}, fmt.Errorf("Intern inherited primary conversation or Soul context")
		}
		return agent.ModelResponse{
			Message:      agent.Message{Role: "assistant", Content: "I checked the scoped material and found one consistent result."},
			FinishReason: "stop", Usage: usage,
		}, nil
	}
	m.mu.Lock()
	m.primaryCalls++
	call := m.primaryCalls
	m.mu.Unlock()
	switch call {
	case 1:
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "delegate-intern-1", Name: "builtin_delegate",
				Arguments: json.RawMessage(`{"objective":"check and summarize the supplied scoped material","assignee":"intern","access":"read_only"}`),
			}}}, FinishReason: "tool_calls", Usage: usage,
		}, nil
	case 2:
		if len(request.Tools) == 0 {
			return agent.ModelResponse{}, fmt.Errorf("primary Eri lost its provider tool protocol while the Intern was pending")
		}
		return agent.ModelResponse{
			Message:      agent.Message{Role: "assistant", Content: "I handed the check to an intern. It is running; I'll review the result when it returns."},
			FinishReason: "stop", Usage: usage,
		}, nil
	case 3:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "system" || !strings.Contains(last.Content, "intern has reached a terminal state") || !strings.Contains(last.Content, `"status":"completed"`) {
			return agent.ModelResponse{}, fmt.Errorf("terminal Intern result missing: %+v", last)
		}
		return agent.ModelResponse{
			Message:      agent.Message{Role: "assistant", Content: "The intern finished. I reviewed the result: the scoped material was consistent."},
			FinishReason: "stop", Usage: usage,
		}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected primary model call %d", call)
	}
}

func (m *codexDelegationIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	usage := agent.Usage{Provider: "fake", Model: "codex-delegation", ModelCalls: 1}
	switch call {
	case 1:
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "delegate-codex-1", Name: "builtin_delegate",
				Arguments: json.RawMessage(`{"objective":"inspect the workspace and return evidence","assignee":"engineering_team","access":"read_only"}`),
			}}}, FinishReason: "tool_calls", Usage: usage,
		}, nil
	case 2:
		if len(request.Tools) == 0 {
			return agent.ModelResponse{}, fmt.Errorf("primary Eri lost its provider tool protocol while the engineering team was pending")
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "delegate-codex-1" || !strings.Contains(last.Content, `"deferred"`) {
			return agent.ModelResponse{}, fmt.Errorf("queued engineering-team observation missing: %+v", last)
		}
		return agent.ModelResponse{
			Message:      agent.Message{Role: "assistant", Content: "I handed the workspace inspection to the engineering team. It is running; I'll review the result when it returns."},
			FinishReason: "stop", Usage: usage,
		}, nil
	case 3:
		if len(request.Tools) == 0 {
			return agent.ModelResponse{}, fmt.Errorf("primary Eri tools were not restored after engineering-team completion")
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "system" || !strings.Contains(last.Content, "engineering_team has reached a terminal state") || !strings.Contains(last.Content, `"status":"completed"`) {
			return agent.ModelResponse{}, fmt.Errorf("terminal engineering-team result missing: %+v", last)
		}
		return agent.ModelResponse{
			Message:      agent.Message{Role: "assistant", Content: "The engineering team finished the inspection. I reviewed its result: the workspace evidence was verified, and no files were changed."},
			FinishReason: "stop", Usage: usage,
		}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected main-agent model call %d", call)
	}
}

type blockingCodexRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingCodexRunner) Run(ctx context.Context, request codex.RunRequest, onStarted func(int) error, _ func(context.Context) (bool, error)) (codex.Result, error) {
	if request.Mode != codex.ReadOnly || !strings.Contains(request.Prompt, "inspect the workspace") {
		return codex.Result{}, fmt.Errorf("unexpected Codex request: %+v", request)
	}
	if err := onStarted(4242); err != nil {
		return codex.Result{}, err
	}
	r.once.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		return codex.Result{}, ctx.Err()
	case <-r.release:
		return codex.Result{
			Status: "completed", Summary: "workspace evidence verified",
			Evidence: []string{"workspace inspected"}, Changes: []string{}, Tests: []string{"read-only verification"}, RemainingRisk: []string{},
		}, nil
	}
}

func (*blockingCodexRunner) Recover(context.Context, codex.RunRequest, func(context.Context) (bool, error)) (codex.Result, error) {
	return codex.Result{}, codex.ErrUnknown
}

type introductionIntegrationModel struct{ integrationModelCapabilities }

func (introductionIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	foundTrigger := false
	for _, message := range request.Messages {
		if message.Role == "system" && strings.Contains(message.Content, "first authenticated connection") {
			foundTrigger = true
			break
		}
	}
	if !foundTrigger {
		return agent.ModelResponse{}, fmt.Errorf("introduction trigger was not assembled into model context")
	}
	return agent.ModelResponse{
		Message:      agent.Message{Role: "assistant", Content: "Hello. I'm Eri, and I'll help you carry work through to a reliable result."},
		FinishReason: "stop",
		Usage:        agent.Usage{Provider: "fake", Model: "introduction", ModelCalls: 1},
	}, nil
}

type attachmentIntegrationModel struct{ integrationModelCapabilities }

type longArtifactModel struct {
	integrationModelCapabilities
	body string
}

func (m longArtifactModel) Complete(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: m.body}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "long-artifact", ModelCalls: 1}}, nil
}

func (attachmentIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	if len(request.Messages) == 0 {
		return agent.ModelResponse{}, fmt.Errorf("attachment context is empty")
	}
	attachmentContext := ""
	for _, message := range request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, `USER ATTACHMENT`) {
			attachmentContext = message.Content
			break
		}
	}
	if !strings.Contains(attachmentContext, `attachment-private-sigma`) || !strings.Contains(attachmentContext, `notes.txt`) {
		return agent.ModelResponse{}, fmt.Errorf("attachment was not assembled as untrusted context: %q", attachmentContext)
	}
	return agent.ModelResponse{
		Message: agent.Message{Role: "assistant", Content: "I read the attachment contents."}, FinishReason: "stop",
		Usage: agent.Usage{Provider: "fake", Model: "attachment", ModelCalls: 1},
	}, nil
}

type toolIntegrationModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
}

type approvalIntegrationModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
	hash  string
}

type memoryBehaviorModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
}

type reminderBehaviorModel struct {
	integrationModelCapabilities
	mu    sync.Mutex
	calls int
	at    time.Time
}

type proactiveConsentModel struct{ integrationModelCapabilities }

func (proactiveConsentModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "proactive-consent", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	last := request.Messages[len(request.Messages)-1]
	if strings.Contains(latestUser, "I keep researching AI") {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I noticed this is becoming a stable interest. Would you like me to create a daily 09:00 AI progress brief? I will not start it unless you agree."}, FinishReason: "stop", Usage: usage}, nil
	}
	if !strings.Contains(latestUser, "yes create that daily brief") {
		return agent.ModelResponse{}, fmt.Errorf("unexpected proactive scenario %q", latestUser)
	}
	if last.Role == "tool" && last.ToolCallID == "create-ai-brief" {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "The daily AI brief is now active for 09:00 Asia/Shanghai."}, FinishReason: "stop", Usage: usage}, nil
	}
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
		ID: "create-ai-brief", Name: "builtin_commitments", Arguments: json.RawMessage(`{"operation":"create","message":"Track material AI developments, deduplicate events, and deliver only high-value changes.","importance":"normal","delivery_route":"recent_channel","schedule":{"type":"daily","daily_time":"09:00","timezone":"Asia/Shanghai"}}`),
	}}}, FinishReason: "tool_calls", Usage: usage}, nil
}

type cancelBehaviorModel struct {
	integrationModelCapabilities
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type userDataBehaviorModel struct{ integrationModelCapabilities }

type feedbackBehaviorModel struct{ integrationModelCapabilities }

func (feedbackBehaviorModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "feedback", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	last := request.Messages[len(request.Messages)-1]
	if strings.Contains(latestUser, "original itinerary request") {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "The train leaves on Thursday."}, FinishReason: "stop", Usage: usage}, nil
	}
	if !strings.Contains(latestUser, "explicit correction") {
		return agent.ModelResponse{}, fmt.Errorf("unexpected feedback scenario %q", latestUser)
	}
	if last.Role == "tool" && last.ToolCallID == "record-correction" {
		if !strings.Contains(last.Content, `"kind":"correction"`) || !strings.Contains(last.Content, `"source_task_id"`) {
			return agent.ModelResponse{}, fmt.Errorf("feedback receipt missing: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "You're right—the confirmed departure is Friday. I recorded the correction against my previous answer."}, FinishReason: "stop", Usage: usage}, nil
	}
	return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
		ID: "record-correction", Name: "builtin_feedback", Arguments: json.RawMessage(`{"kind":"correction","statement":"The prior Thursday departure was wrong; the confirmed departure is Friday."}`),
	}}}, FinishReason: "tool_calls", Usage: usage}, nil
}

type calendarSearchInput struct {
	Query string `json:"query" jsonschema:"date window query"`
}

type calendarSearchOutput struct {
	Windows []string `json:"windows"`
	Receipt string   `json:"receipt"`
}

type emailSendInput struct {
	To      string `json:"to" jsonschema:"recipient address"`
	Subject string `json:"subject" jsonschema:"message subject"`
	Body    string `json:"body" jsonschema:"message body"`
}

type emailSendOutput struct {
	MessageID string `json:"message_id"`
	Receipt   string `json:"receipt"`
}

func TestCalendarPluginHelperProcess(t *testing.T) {
	if os.Getenv("ERI_CALENDAR_PLUGIN_HELPER") != "1" {
		return
	}
	server := protocol.NewServer(&protocol.Implementation{Name: "eri-reference-calendar", Version: "2.0.0"}, nil)
	protocol.AddTool(server, &protocol.Tool{Name: "search_windows", Description: "Search available calendar windows."},
		func(_ context.Context, _ *protocol.CallToolRequest, input calendarSearchInput) (*protocol.CallToolResult, calendarSearchOutput, error) {
			return nil, calendarSearchOutput{Windows: []string{"2026-08-10T09:00:00+08:00", "2026-08-11T14:00:00+08:00"}, Receipt: "calendar-read-receipt"}, nil
		})
	_ = server.Run(context.Background(), &protocol.StdioTransport{})
	os.Exit(0)
}

func TestEmailPluginHelperProcess(t *testing.T) {
	if os.Getenv("ERI_EMAIL_PLUGIN_HELPER") != "1" {
		return
	}
	server := protocol.NewServer(&protocol.Implementation{Name: "eri-reference-email", Version: "1.0.0"}, nil)
	protocol.AddTool(server, &protocol.Tool{Name: "send_email", Description: "Send one email and return the provider receipt."},
		func(_ context.Context, _ *protocol.CallToolRequest, input emailSendInput) (*protocol.CallToolResult, emailSendOutput, error) {
			if input.To != "alice@example.com" || input.Subject == "" || input.Body == "" {
				return nil, emailSendOutput{}, fmt.Errorf("invalid reference email")
			}
			return nil, emailSendOutput{MessageID: "msg-reference-001", Receipt: "accepted_by_reference_email_provider"}, nil
		})
	_ = server.Run(context.Background(), &protocol.StdioTransport{})
	os.Exit(0)
}

type referenceCalendarModel struct{ integrationModelCapabilities }

type referenceEmailModel struct{ integrationModelCapabilities }

func (referenceEmailModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "reference-email", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	last := request.Messages[len(request.Messages)-1]
	switch {
	case strings.Contains(latestUser, "install email capability"):
		if last.Role == "tool" && last.ToolCallID == "install-email" {
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Email capability is installed and healthy."}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "install-email", Name: "builtin_plugins", Arguments: json.RawMessage(`{"operation":"install","manifest_path":"email-v1.json"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case strings.Contains(latestUser, "send approved email"):
		if last.Role == "tool" && last.ToolCallID == "send-email" {
			if !strings.Contains(last.Content, "msg-reference-001") || !strings.Contains(last.Content, "accepted_by_reference_email_provider") {
				return agent.ModelResponse{}, fmt.Errorf("email provider receipt missing: %+v", last)
			}
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "The email was accepted by the provider as msg-reference-001."}, FinishReason: "stop", Usage: usage}, nil
		}
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Name == "mcp_email_send_email"
		}
		if !found {
			return agent.ModelResponse{}, fmt.Errorf("installed email tool is not visible")
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "send-email", Name: "mcp_email_send_email", Arguments: json.RawMessage(`{"to":"alice@example.com","subject":"Confirmed plan","body":"The plan is approved."}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected email scenario %q", latestUser)
	}
}

func (referenceCalendarModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "reference-calendar", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	last := request.Messages[len(request.Messages)-1]
	switch {
	case strings.Contains(latestUser, "install calendar v1"):
		if last.Role == "tool" && last.ToolCallID == "install-calendar-v1" {
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Calendar access is installed and healthy."}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "install-calendar-v1", Name: "builtin_plugins", Arguments: json.RawMessage(`{"operation":"install","manifest_path":"calendar-v1.json"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case strings.Contains(latestUser, "find calendar windows"):
		if last.Role == "tool" && last.ToolCallID == "calendar-search" {
			if !strings.Contains(last.Content, "2026-08-10") || !strings.Contains(last.Content, "calendar-read-receipt") {
				return agent.ModelResponse{}, fmt.Errorf("calendar receipt missing: %+v", last)
			}
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I found two verified calendar windows and recommend August 10 at 09:00."}, FinishReason: "stop", Usage: usage}, nil
		}
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Name == "mcp_calendar_search_windows"
		}
		if !found {
			return agent.ModelResponse{}, fmt.Errorf("installed calendar tool is not visible")
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "calendar-search", Name: "mcp_calendar_search_windows", Arguments: json.RawMessage(`{"query":"next week"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case strings.Contains(latestUser, "upgrade calendar v2"):
		if last.Role == "tool" && last.ToolCallID == "upgrade-calendar-v2" {
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Calendar was upgraded to version 2 after the expanded permission was approved."}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "upgrade-calendar-v2", Name: "builtin_plugins", Arguments: json.RawMessage(`{"operation":"install","manifest_path":"calendar-v2.json"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected calendar scenario %q", latestUser)
	}
}

func (userDataBehaviorModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "user-data", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	last := request.Messages[len(request.Messages)-1]
	switch {
	case strings.Contains(latestUser, "seed private data"):
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Seed data recorded."}, FinishReason: "stop", Usage: usage}, nil
	case strings.Contains(latestUser, "export all my data"):
		if last.Role == "tool" && last.ToolCallID == "export-all-data" {
			if !strings.Contains(last.Content, `"attachments"`) || !strings.Contains(last.Content, `eri-user-data-export`) {
				return agent.ModelResponse{}, fmt.Errorf("portable export result missing: %+v", last)
			}
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Your complete Eri data export is attached."}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "export-all-data", Name: "builtin_user_data", Arguments: json.RawMessage(`{"operation":"export"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case strings.Contains(latestUser, "delete all my data"):
		if last.Role == "tool" && last.ToolCallID == "delete-all-data" {
			if !strings.Contains(last.Content, `awaiting_delivery`) {
				return agent.ModelResponse{}, fmt.Errorf("erasure scheduling receipt missing: %+v", last)
			}
			return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Deletion is confirmed. After this message is accepted, Eri will permanently erase all local user content and derived data."}, FinishReason: "stop", Usage: usage}, nil
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "delete-all-data", Name: "builtin_user_data", Arguments: json.RawMessage(`{"operation":"delete_all"}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	case strings.Contains(latestUser, "fresh clean start"):
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "Fresh state is ready."}, FinishReason: "stop", Usage: usage}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected user-data scenario %q", latestUser)
	}
}

func (m *cancelBehaviorModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	usage := agent.Usage{Provider: "fake", Model: "cancel", ModelCalls: 1}
	latestUser := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			latestUser = request.Messages[index].Content
			break
		}
	}
	if strings.Contains(latestUser, "long cancellation target") {
		m.once.Do(func() { close(m.started) })
		<-m.release
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "This result must not be delivered after cancellation."}, FinishReason: "stop", Usage: usage}, nil
	}
	if !strings.Contains(latestUser, "cancel the long task") {
		return agent.ModelResponse{}, fmt.Errorf("unexpected cancellation scenario: %q", latestUser)
	}
	var lastTool *agent.Message
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "tool" {
			lastTool = &request.Messages[index]
			break
		}
	}
	if lastTool == nil {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "list-tasks", Name: "builtin_tasks", Arguments: json.RawMessage(`{"operation":"list","limit":20}`),
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	}
	if lastTool.ToolCallID == "list-tasks" {
		var observation struct {
			Result struct {
				Output json.RawMessage `json:"output"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(lastTool.Content), &observation); err != nil {
			return agent.ModelResponse{}, err
		}
		var records []assistanttask.Record
		if err := json.Unmarshal(observation.Result.Output, &records); err != nil {
			return agent.ModelResponse{}, err
		}
		targetID := ""
		for _, record := range records {
			if strings.Contains(record.Objective, "long cancellation target") {
				targetID = record.ID
				break
			}
		}
		if targetID == "" {
			return agent.ModelResponse{}, fmt.Errorf("long-running target was not listed: %s", observation.Result.Output)
		}
		arguments, _ := json.Marshal(map[string]string{"operation": "cancel", "task_id": targetID})
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "cancel-task", Name: "builtin_tasks", Arguments: arguments,
		}}}, FinishReason: "tool_calls", Usage: usage}, nil
	}
	if lastTool.ToolCallID == "cancel-task" && strings.Contains(lastTool.Content, `"success":true`) {
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "The long task has been asked to stop; confirmed side effects, if any, were not undone."}, FinishReason: "stop", Usage: usage}, nil
	}
	return agent.ModelResponse{}, fmt.Errorf("unexpected task tool result: %+v", lastTool)
}

type recordingNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recordingNotifier) Send(_ context.Context, title, body string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, title+"\n"+body)
	return "accepted_by_test_notification_center", nil
}

func (n *recordingNotifier) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

func (m *reminderBehaviorModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	switch m.calls {
	case 1:
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Name == "builtin_commitments"
		}
		if !found {
			return agent.ModelResponse{}, fmt.Errorf("commitment descriptor missing")
		}
		arguments, err := json.Marshal(map[string]any{
			"operation": "create", "message": "Check travel documents", "importance": "important",
			"schedule": map[string]any{"type": "once", "at": m.at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			return agent.ModelResponse{}, err
		}
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "commitment-call", Name: "builtin_commitments", Arguments: arguments,
			}}}, FinishReason: "tool_calls", Usage: agent.Usage{Provider: "fake", Model: "reminder", ModelCalls: 1},
		}, nil
	case 2:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "commitment-call" || !strings.Contains(last.Content, `"success":true`) {
			return agent.ModelResponse{}, fmt.Errorf("commitment result missing: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I will remind you to check your travel documents on time."}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "reminder", ModelCalls: 1}}, nil
	case 3:
		foundTrigger := false
		foundOccurredEventFrame := false
		for _, message := range request.Messages {
			foundTrigger = foundTrigger || strings.Contains(message.Content, "A durable commitment is due") && strings.Contains(message.Content, "Check travel documents")
			foundOccurredEventFrame = foundOccurredEventFrame || strings.Contains(message.Content, `"trigger_event":"commitment.due"`) &&
				strings.Contains(message.Content, `"trigger_state":"occurred"`) &&
				strings.Contains(message.Content, "not unfinished work to replay")
		}
		if !foundTrigger {
			return agent.ModelResponse{}, fmt.Errorf("durable trigger context missing: %+v", request.Messages)
		}
		if !foundOccurredEventFrame {
			return agent.ModelResponse{}, fmt.Errorf("occurred event task frame missing: %+v", request.Messages)
		}
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "notification-call", Name: "builtin_notification", Arguments: json.RawMessage(`{"title":"Eri reminder","body":"It is time to check your travel documents"}`),
			}}}, FinishReason: "tool_calls", Usage: agent.Usage{Provider: "fake", Model: "reminder", ModelCalls: 1},
		}, nil
	case 4:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "notification-call" || !strings.Contains(last.Content, `"success":true`) {
			return agent.ModelResponse{}, fmt.Errorf("notification result missing: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "It is time to check your travel documents."}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "reminder", ModelCalls: 1}}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected reminder model call %d", m.calls)
	}
}

func (m *memoryBehaviorModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	switch m.calls {
	case 1:
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "remember-call", Name: "builtin_memory", Arguments: json.RawMessage(`{
					"operation":"record","statement":"The user prefers hotel rooms with a window","kind":"preference","scope":"travel",
					"relation":"supports","explicit_user_memory":true
				}`),
			}}}, FinishReason: "tool_calls", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1},
		}, nil
	case 2:
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, `"success":true`) {
			return agent.ModelResponse{}, fmt.Errorf("memory record was not confirmed")
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I will remember that."}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1}}, nil
	case 3:
		if strings.Contains(request.System, "The user prefers hotel rooms with a window") || strings.Contains(request.System, "memory_id=") {
			return agent.ModelResponse{}, fmt.Errorf("governed memory invalidated the stable System prefix: %s", request.System)
		}
		dynamicContext := ""
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "<relevant_memory_context>") {
				dynamicContext += message.Content
			}
		}
		if !strings.Contains(dynamicContext, "The user prefers hotel rooms with a window") || !strings.Contains(dynamicContext, "memory_id=") {
			return agent.ModelResponse{}, fmt.Errorf("governed memory was not assembled in the dynamic suffix: %s", dynamicContext)
		}
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "apply-memory-call", Name: "builtin_files", Arguments: json.RawMessage(`{"operation":"create","path":"hotel-choice.txt","content":"window"}`),
			}}}, FinishReason: "tool_calls", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1},
		}, nil
	case 4:
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, `"success":true`) {
			return agent.ModelResponse{}, fmt.Errorf("memory-derived action was not confirmed")
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I prepared the options using your window preference."}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1}}, nil
	case 5:
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "export-memory-call", Name: "builtin_memory", Arguments: json.RawMessage(`{"operation":"export"}`),
		}}}, FinishReason: "tool_calls", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1}}, nil
	case 6:
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "export-memory-call" || !strings.Contains(last.Content, `"attachments"`) || strings.Contains(last.Content, "The user prefers hotel rooms with a window") {
			return agent.ModelResponse{}, fmt.Errorf("memory export was not passed as attachment metadata: %+v", last)
		}
		return agent.ModelResponse{Message: agent.Message{Role: "assistant", Content: "I exported the memory data as a JSON attachment."}, FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "memory", ModelCalls: 1}}, nil
	default:
		return agent.ModelResponse{}, fmt.Errorf("unexpected model call %d", m.calls)
	}
}

func (m *approvalIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		arguments, _ := json.Marshal(map[string]string{
			"operation": "write", "path": "plan.txt", "content": "approved-content", "expected_sha256": m.hash,
		})
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "approval-call", Name: "builtin_files", Arguments: arguments,
			}}},
			FinishReason: "tool_calls",
			Usage:        agent.Usage{Provider: "fake", Model: "approval", ModelCalls: 1},
		}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "approval-call" || !strings.Contains(last.Content, `"success":true`) {
		return agent.ModelResponse{}, fmt.Errorf("approved result was not resumed: %+v", last)
	}
	return agent.ModelResponse{
		Message:      agent.Message{Role: "assistant", Content: "I updated plan.txt with the version you approved."},
		FinishReason: "stop", Usage: agent.Usage{Provider: "fake", Model: "approval", ModelCalls: 1},
	}, nil
}

func (d *toolIntegrationModel) Complete(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls == 1 {
		foundFiles := false
		for _, definition := range request.Tools {
			foundFiles = foundFiles || definition.Name == "builtin_files"
		}
		if !foundFiles {
			return agent.ModelResponse{}, fmt.Errorf("file descriptor missing from request: %+v", request.Tools)
		}
		return agent.ModelResponse{
			Message: agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
				ID: "call-1", Name: "builtin_files", Arguments: json.RawMessage(`{"operation":"read","path":"brief.txt"}`),
			}}},
			FinishReason: "tool_calls",
			Usage:        agent.Usage{Provider: "fake", Model: "integration", InputTokens: 10, OutputTokens: 6, ModelCalls: 1},
		}, nil
	}
	if len(request.Messages) < 2 || request.Messages[len(request.Messages)-1].Role != "tool" || request.Messages[len(request.Messages)-1].ToolCallID != "call-1" || !strings.Contains(request.Messages[len(request.Messages)-1].Content, "tool-private-omega") {
		return agent.ModelResponse{}, fmt.Errorf("confirmed tool observation missing: %+v", request.Messages)
	}
	return agent.ModelResponse{
		Message: agent.Message{Role: "assistant", Content: "I read and verified brief.txt."}, FinishReason: "stop",
		Usage: agent.Usage{Provider: "fake", Model: "integration", InputTokens: 20, OutputTokens: 8, ModelCalls: 1},
	}, nil
}

func TestDaemonReliableReplyEndToEnd(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-integration-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot:         root,
		DatabasePath:     filepath.Join(root, "metadata", "eri.db"),
		SocketPath:       socketPath,
		ConversationAddr: "127.0.0.1:0",
		ObservatoryAddr:  "127.0.0.1:0",
		OllamaURL:        "http://127.0.0.1:11434",
		Model:            "fake",
		ModelTimeout:     time.Second,
		PollInterval:     5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	masterKey := bytes.Repeat([]byte{0x62}, 32)
	d, err := New(ctx, cfg, Dependencies{MasterKey: masterKey, Model: integrationModel{}, Judge: testPassJudge{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, cfg.SocketPath)
	client := integrationUnixClient(cfg.SocketPath)

	privateInput := "integrated-private-ζ"
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": privateInput}, &sent)
	deadline := time.Now().Add(5 * time.Second)
	var status channel.TaskStatus
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
		if status.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "completed" {
		t.Fatalf("task status = %q, want completed", status.Status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	approvalDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(approvalDeadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		if len(timeline.Messages) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(timeline.Messages) != 2 {
		t.Fatalf("timeline size = %d, want 2", len(timeline.Messages))
	}
	if timeline.Messages[0].Content != privateInput || timeline.Messages[1].Content != "Complete closed-loop response" {
		t.Fatalf("unexpected timeline: %+v", timeline.Messages)
	}
	if timeline.Messages[1].Receipt != "accepted_by_channel" {
		t.Fatalf("receipt = %q", timeline.Messages[1].Receipt)
	}
	var episodes []episode.Record
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		episodes, err = d.store.ListEpisodes(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(episodes) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(episodes) != 1 || episodes[0].TaskID != sent.TaskID {
		t.Fatalf("episodes = %+v", episodes)
	}
	contentStore, err := content.New(filepath.Join(root, "content"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	episodeService := episode.NewService(d.store, contentStore)
	manifest, found, err := episodeService.Export(ctx, episodes[0].ID)
	if err != nil || !found || manifest.TaskID != sent.TaskID || len(manifest.Invocations) != 1 || len(manifest.Artifacts) != 1 {
		t.Fatalf("episode manifest = %+v, found = %v, err = %v", manifest, found, err)
	}
	encodedManifest, _ := json.Marshal(manifest)
	if bytes.Contains(encodedManifest, []byte(privateInput)) || bytes.Contains(encodedManifest, []byte("Complete closed-loop response")) {
		t.Fatal("episode manifest contains private message bodies")
	}
	events, err := d.store.ListEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	exportAudited := false
	for _, event := range events {
		exportAudited = exportAudited || (event.Type == "episode.exported" && event.AggregateID == episodes[0].ID)
	}
	if !exportAudited {
		t.Fatal("episode export audit event missing")
	}
	candidate, err := episodeService.Promote(ctx, episodes[0].ID)
	if err != nil || candidate.Status != "candidate" {
		t.Fatalf("dataset candidate = %+v, err = %v", candidate, err)
	}
	runs, err := d.store.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	runDetail, found, err := d.store.LoadRun(ctx, runs[0].ID)
	if err != nil || !found || len(runDetail.Events) == 0 || len(runDetail.Artifacts) != 1 {
		t.Fatalf("run detail = %+v, found = %v, err = %v", runDetail, found, err)
	}

	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Stat(cfg.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
	logBody, err := os.ReadFile(filepath.Join(root, "logs", "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"msg":"durable runtime recovery completed"`, `"running_tasks":0`, `"outbox_items":0`, `"ambiguous_effects":0`,
		`"msg":"Eri daemon started"`, `"msg":"model call started"`, `"msg":"evaluation finished"`,
		`"msg":"outbox dispatch completed"`, `"msg":"Eri daemon stopped"`,
		`"task_id":"` + sent.TaskID + `"`, `"run_id":"`, `"invocation_id":"`, `"duration_ms":`,
	} {
		if !bytes.Contains(logBody, []byte(required)) {
			t.Fatalf("daemon log is missing %q: %s", required, logBody)
		}
	}
	if bytes.Contains(logBody, []byte(privateInput)) || bytes.Contains(logBody, []byte("Complete closed-loop response")) {
		t.Fatal("daemon log contains private conversation content")
	}
	assertDataRootDoesNotContain(t, root, []byte(privateInput))
	assertDataRootDoesNotContain(t, root, []byte("Complete closed-loop response"))
}

func TestDaemonDelegatesToNativeInternAsynchronouslyAndResumesMainEri(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-native-delegation-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: filepath.Join(root, "data"), DatabasePath: filepath.Join(root, "data", "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", WorkspaceRoot: workspace,
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x73}, 32), Model: &nativeDelegationIntegrationModel{}, Judge: testPassJudge{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Please have someone check this scoped material and summarize it."}, &sent)
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	var taskStatus channel.TaskStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &taskStatus)
		if len(timeline.Messages) == 3 && taskStatus.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if taskStatus.Status != "completed" || len(timeline.Messages) != 3 {
		t.Fatalf("native resumed task=%+v timeline=%+v", taskStatus, timeline.Messages)
	}
	if !strings.Contains(timeline.Messages[1].Content, "It is running") || !strings.Contains(timeline.Messages[2].Content, "I reviewed the result") {
		t.Fatalf("native delivery order = %+v", timeline.Messages)
	}
	events, err := d.store.ListEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	completed, resumed := false, false
	for _, event := range events {
		if event.Type == "subagent.completed" {
			completed = event.Data["role_id"] == "intern" && event.Data["provider_id"] == "eri_native"
		}
		resumed = resumed || (event.Type == "task.resumed" && event.AggregateID == sent.TaskID && event.Data["role_id"] == "intern" && event.Data["provider_id"] == "eri_native")
	}
	if !completed || !resumed {
		t.Fatalf("native role/provider completion facts missing: completed=%v resumed=%v", completed, resumed)
	}
	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonDelegatesToCodexAsynchronouslyAndResumesMainEri(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-codex-delegation-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: filepath.Join(root, "data"), DatabasePath: filepath.Join(root, "data", "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", WorkspaceRoot: workspace,
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, CodexTimeout: time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &blockingCodexRunner{started: make(chan struct{}), release: make(chan struct{})}
	model := &codexDelegationIntegrationModel{}
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x74}, 32), Model: model, Judge: testPassJudge{}, CodexRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Please inspect this workspace with Codex and report back."}, &sent)
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Codex External Agent did not start")
	}

	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		if len(timeline.Messages) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(timeline.Messages) != 2 || !strings.Contains(timeline.Messages[1].Content, "It is running") {
		t.Fatalf("progress was not delivered while Codex was still blocked: %+v", timeline.Messages)
	}
	var taskStatus channel.TaskStatus
	integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &taskStatus)
	if taskStatus.Status != "waiting" {
		t.Fatalf("parent task status before Codex completion = %q", taskStatus.Status)
	}

	close(runner.release)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &taskStatus)
		if len(timeline.Messages) == 3 && taskStatus.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if taskStatus.Status != "completed" || len(timeline.Messages) != 3 {
		t.Fatalf("resumed task=%+v timeline=%+v", taskStatus, timeline.Messages)
	}
	if !strings.Contains(timeline.Messages[2].Content, "I reviewed its result") || timeline.Messages[2].Receipt != "accepted_by_channel" {
		t.Fatalf("final delivery = %+v", timeline.Messages[2])
	}
	events, err := d.store.ListEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	completedEvent, resumedEvent := false, false
	for _, event := range events {
		completedEvent = completedEvent || (event.Type == "subagent.completed" && event.Data["role_id"] == "engineering_team" && event.Data["provider_id"] == "codex")
		resumedEvent = resumedEvent || (event.Type == "task.resumed" && event.AggregateID == sent.TaskID && event.Data["role_id"] == "engineering_team" && event.Data["provider_id"] == "codex")
	}
	if !completedEvent || !resumedEvent {
		t.Fatalf("completion/resume events missing: completed=%v resumed=%v", completedEvent, resumedEvent)
	}
	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonDelegatesToRealLocalCodexEndToEnd(t *testing.T) {
	if os.Getenv("ERI_E2E_CODEX") != "1" {
		t.Skip("set ERI_E2E_CODEX=1 to exercise the authenticated local Codex installation")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, found, err := codex.DiscoverExecutable(os.Getenv("ERI_CODEX_PATH"))
	if err != nil || !found {
		t.Fatalf("discover local Codex: found=%v err=%v", found, err)
	}
	dataRoot := filepath.Join(root, "data")
	runner, err := codex.NewLocalRunner(executable, dataRoot, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-real-codex-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", WorkspaceRoot: workspace,
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, CodexTimeout: 3 * time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x75}, 32), Model: &codexDelegationIntegrationModel{},
		Judge: testPassJudge{}, CodexRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Inspect this empty workspace with my local Codex and report back."}, &sent)
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	var taskStatus channel.TaskStatus
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &taskStatus)
		if len(timeline.Messages) == 3 && taskStatus.Status == "completed" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if taskStatus.Status != "completed" || len(timeline.Messages) != 3 {
		t.Fatalf("real Codex task=%+v timeline=%+v", taskStatus, timeline.Messages)
	}
	if !strings.Contains(timeline.Messages[1].Content, "It is running") || !strings.Contains(timeline.Messages[2].Content, "I reviewed its result") {
		t.Fatalf("real Codex delivery order = %+v", timeline.Messages)
	}
	events, err := d.store.ListEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	foundCompletion := false
	for _, event := range events {
		foundCompletion = foundCompletion || event.Type == "subagent.completed"
	}
	if !foundCompletion {
		t.Fatal("real Codex completion event is missing")
	}
	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonFirstConnectionIntroducesEriExactlyOnceThroughDelivery(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-introduction-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", OllamaURL: "http://127.0.0.1:11434",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x63}, 32), Model: introductionIntegrationModel{}, Judge: testPassJudge{},
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		d.Close()
	})
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)

	var first channel.ConnectResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/conversation/connect", channel.ConnectRequest{Locale: "en-US", Timezone: "Asia/Shanghai"}, &first)
	if !first.IntroductionStarted || first.TaskID == "" {
		t.Fatalf("first connection = %+v", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	var status channel.TaskStatus
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+first.TaskID, nil, &status)
		if status.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "completed" {
		t.Fatalf("introduction task status = %q", status.Status)
	}

	var second channel.ConnectResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/conversation/connect", channel.ConnectRequest{Locale: "en-US", Timezone: "Asia/Shanghai"}, &second)
	if second.IntroductionStarted || second.TaskID != first.TaskID {
		t.Fatalf("repeat connection = %+v, first = %+v", second, first)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	if len(timeline.Messages) != 1 || timeline.Messages[0].Role != "assistant" || timeline.Messages[0].TaskID != first.TaskID || timeline.Messages[0].Receipt != "accepted_by_channel" {
		t.Fatalf("introduction timeline = %+v", timeline.Messages)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonRecordsCorrectiveFeedbackAndBuildsReplacementDatasetCandidate(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-feedback-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		OllamaURL: "http://127.0.0.1:11434", Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, Dependencies{
		MasterKey: bytes.Repeat([]byte{0x63}, 32), Model: feedbackBehaviorModel{}, Judge: testPassJudge{},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, cfg.SocketPath)
	client := integrationUnixClient(cfg.SocketPath)
	waitCompleted := func(taskID string) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var status channel.TaskStatus
			integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+taskID, nil, &status)
			if status.Status == "completed" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s did not complete", taskID)
	}
	waitEpisodes := func(count int) []episode.Record {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			records, listErr := d.store.ListEpisodes(ctx, 10)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(records) >= count {
				return records
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("episodes did not reach %d", count)
		return nil
	}

	var original channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "original itinerary request"}, &original)
	waitCompleted(original.TaskID)
	originalEpisodes := waitEpisodes(1)
	originalEpisodeID := originalEpisodes[0].ID
	if _, err := d.store.PromoteEpisodeCandidate(ctx, originalEpisodeID); err != nil {
		t.Fatal(err)
	}

	var corrective channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{
		"text": "explicit correction: your Thursday answer was wrong; the confirmed departure is Friday",
	}, &corrective)
	waitCompleted(corrective.TaskID)
	allEpisodes := waitEpisodes(2)

	events, err := d.store.ListEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	feedbackEvents := 0
	for _, event := range events {
		if event.Type != "feedback.recorded" {
			continue
		}
		feedbackEvents++
		if event.Data["source_task_id"] != original.TaskID || event.Data["feedback_task_id"] != corrective.TaskID || event.Data["kind"] != "correction" {
			t.Fatalf("feedback event = %+v", event)
		}
	}
	if feedbackEvents != 1 {
		t.Fatalf("feedback events = %d", feedbackEvents)
	}
	oldStatus := ""
	for _, record := range allEpisodes {
		if record.ID == originalEpisodeID {
			oldStatus = record.Status
		}
	}
	if oldStatus != "invalidated" {
		t.Fatalf("old episode status=%q", oldStatus)
	}
	if _, err := d.store.PromoteEpisodeCandidate(ctx, originalEpisodeID); err == nil {
		t.Fatal("invalidated old episode remained promotable")
	}
	replacementEpisodeID := ""
	for _, record := range allEpisodes {
		if record.TaskID == corrective.TaskID {
			replacementEpisodeID = record.ID
		}
	}
	if replacementEpisodeID == "" {
		t.Fatal("replacement feedback episode missing")
	}
	replacementCandidate, err := d.store.PromoteEpisodeCandidate(ctx, replacementEpisodeID)
	if err != nil || replacementCandidate.Status != "candidate" {
		t.Fatalf("replacement candidate=%+v err=%v", replacementCandidate, err)
	}

	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonAttachmentIsEncryptedDownloadableAndAvailableToAgent(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-attachment-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x65}, 32), Model: attachmentIntegrationModel{}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	privateBody := []byte("attachment-private-sigma")
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	if err := writer.WriteField("text", "Please read the attachment"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("files", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(privateBody); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", &requestBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var sent channel.SendResult
	if err := json.NewDecoder(response.Body).Decode(&sent); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
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
		t.Fatalf("attachment task status = %q, error = %q", status.Status, status.ErrorCode)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	if len(timeline.Messages) != 2 || len(timeline.Messages[0].Attachments) != 1 {
		t.Fatalf("attachment timeline = %+v", timeline.Messages)
	}
	attachment := timeline.Messages[0].Attachments[0]
	download, err := client.Get("http://eri.local/api/v1/attachments/" + attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil || !bytes.Equal(downloaded, privateBody) {
		t.Fatalf("downloaded attachment = %q, err = %v", downloaded, err)
	}
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
	assertDataRootDoesNotContain(t, root, privateBody)
}

func TestDaemonLongResultPreservesTheJudgedReplyAndAddsAnEncryptedAttachment(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-long-artifact-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	privateMarker := "long-private-deliverable-phi"
	longBody := "# Complete research report\n\n" + strings.Repeat("This is a complete verified paragraph. ", 1_100) + privateMarker
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x68}, 32), Model: longArtifactModel{body: longBody}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Please deliver the complete report"}, &sent)
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
		t.Fatalf("long artifact task = %+v", status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	if len(timeline.Messages) != 2 || len(timeline.Messages[1].Attachments) != 1 || timeline.Messages[1].Content != longBody {
		t.Fatalf("long artifact timeline = %+v", timeline.Messages)
	}
	download, err := client.Get("http://eri.local/api/v1/attachments/" + timeline.Messages[1].Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil || string(downloaded) != longBody {
		t.Fatalf("long artifact download size = %d, err = %v", len(downloaded), err)
	}
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
	assertDataRootDoesNotContain(t, root, []byte(privateMarker))
}

func TestDaemonAgentLoopUsesEncryptedLocalToolResult(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	privateToolBody := "tool-private-omega"
	if err := os.WriteFile(filepath.Join(workspace, "brief.txt"), []byte(privateToolBody), 0o600); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-tool-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, WorkspaceRoot: workspace,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &toolIntegrationModel{}
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x63}, 32), Model: model, Judge: testPassJudge{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Please read brief.txt"}, &sent)
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
		t.Fatalf("tool task status = %q, error = %q", status.Status, status.ErrorCode)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	if len(timeline.Messages) != 2 || timeline.Messages[1].Content != "I read and verified brief.txt." {
		t.Fatalf("unexpected tool timeline: %+v", timeline.Messages)
	}
	integrationJSON(t, client, http.MethodPost, "/api/v1/system/stop", map[string]string{}, &map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	assertDataRootDoesNotContain(t, dataRoot, []byte(privateToolBody))
}

func TestDaemonApprovalResumesExactToolCallAfterRestart(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("original-content")
	filePath := filepath.Join(workspace, "plan.txt")
	if err := os.WriteFile(filePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	model := &approvalIntegrationModel{hash: fmt.Sprintf("%x", sum)}
	key := bytes.Repeat([]byte{0x64}, 32)
	socketFile, err := os.CreateTemp("", "eri-approval-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, WorkspaceRoot: workspace,
	}

	start := func() (context.CancelFunc, *Daemon, <-chan error, *http.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		d, err := New(ctx, cfg, Dependencies{MasterKey: key, Model: model, Judge: testPassJudge{}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			connection, dialErr := net.DialTimeout("unix", socketPath, 20*time.Millisecond)
			if dialErr == nil {
				connection.Close()
				return cancel, d, done, integrationUnixClient(socketPath)
			}
			select {
			case runErr := <-done:
				cancel()
				t.Fatalf("daemon exited before socket was ready: %v", runErr)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		t.Fatal("daemon socket did not become ready")
		return cancel, d, done, integrationUnixClient(socketPath)
	}
	stop := func(cancel context.CancelFunc, d *Daemon, done <-chan error) {
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

	cancel, first, done, client := start()
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Please update plan.txt"}, &sent)
	var status channel.TaskStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
		if status.Status == "waiting" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "waiting" {
		t.Fatalf("task status = %q, want waiting", status.Status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		if len(timeline.Messages) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(timeline.Messages) != 2 || timeline.Messages[1].Kind != "approval_request" || timeline.Messages[1].Role != "system" || timeline.Messages[1].Content != "" {
		t.Fatalf("approval was not delivered: %+v", timeline.Messages)
	}
	approvalID, ok := timeline.Messages[1].Data["approval_id"].(string)
	if !ok || approvalID == "" {
		t.Fatalf("approval id missing: %+v", timeline.Messages[1].Data)
	}
	if body, _ := os.ReadFile(filePath); !bytes.Equal(body, original) {
		t.Fatalf("file changed before approval: %q", body)
	}
	stop(cancel, first, done)

	cancel, second, done, client := start()
	defer stop(cancel, second, done)
	var decision map[string]any
	integrationJSON(t, client, http.MethodPost, "/api/v1/approvals/"+approvalID, map[string]string{"decision": "approve"}, &decision)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
		if status.Status == "completed" || status.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "completed" {
		t.Fatalf("resumed task status = %q, error = %q", status.Status, status.ErrorCode)
	}
	if body, _ := os.ReadFile(filePath); string(body) != "approved-content" {
		t.Fatalf("approved file body = %q", body)
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	if len(timeline.Messages) != 3 || timeline.Messages[2].Content != "I updated plan.txt with the version you approved." {
		t.Fatalf("unexpected resumed timeline: %+v", timeline.Messages)
	}
	assertDataRootDoesNotContain(t, dataRoot, []byte("approved-content"))
}

func TestDaemonMemoryChangesRealToolParameters(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-memory-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond, WorkspaceRoot: workspace,
	}
	ctx, cancel := context.WithCancel(context.Background())
	model := &memoryBehaviorModel{}
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x66}, 32), Model: model, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	sendAndWait := func(text string) channel.SendResult {
		var sent channel.SendResult
		integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": text}, &sent)
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
			t.Fatalf("task %s status = %q, error = %q", sent.TaskID, status.Status, status.ErrorCode)
		}
		return sent
	}
	sendAndWait("Remember that I prefer hotel rooms with a window")
	sendAndWait("Prepare a choice file using my hotel preference")
	sendAndWait("Export the data you remember about me")
	if body, err := os.ReadFile(filepath.Join(workspace, "hotel-choice.txt")); err != nil || string(body) != "window" {
		t.Fatalf("memory-derived file = %q, err = %v", body, err)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=20", nil, &timeline)
	last := timeline.Messages[len(timeline.Messages)-1]
	if last.Content != "I exported the memory data as a JSON attachment." || len(last.Attachments) != 1 || last.Attachments[0].Name != "eri-memory-export.json" {
		t.Fatalf("memory export delivery = %+v", last)
	}
	download, err := client.Get("http://eri.local/api/v1/attachments/" + last.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil || !json.Valid(exported) || !bytes.Contains(exported, []byte("The user prefers hotel rooms with a window")) {
		t.Fatalf("memory export body = %q err=%v", exported, err)
	}
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
	assertDataRootDoesNotContain(t, dataRoot, []byte("The user prefers hotel rooms with a window"))
}

func TestDaemonCommitmentSurvivesRestartAndDeliversProactiveReminder(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	socketFile, err := os.CreateTemp("", "eri-reminder-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	model := &reminderBehaviorModel{at: time.Now().UTC().Add(750 * time.Millisecond)}
	notifier := &recordingNotifier{}
	key := bytes.Repeat([]byte{0x67}, 32)
	start := func() (context.CancelFunc, *Daemon, <-chan error, *http.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		d, err := New(ctx, cfg, Dependencies{MasterKey: key, Model: model, Notifier: notifier, Judge: testPassJudge{}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		waitForSocket(t, socketPath)
		return cancel, d, done, integrationUnixClient(socketPath)
	}
	stop := func(cancel context.CancelFunc, d *Daemon, done <-chan error) {
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

	cancel, first, done, client := start()
	var sent channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "Remind me later to check my travel documents"}, &sent)
	deadline := time.Now().Add(5 * time.Second)
	var task channel.TaskStatus
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &task)
		if task.Status == "completed" || task.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task.Status != "completed" {
		t.Fatalf("commitment creation task = %q, error = %q", task.Status, task.ErrorCode)
	}
	stop(cancel, first, done)
	if delay := time.Until(model.at.Add(50 * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}

	cancel, second, done, client := start()
	defer stop(cancel, second, done)
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
		if len(timeline.Messages) == 3 && len(notifier.snapshot()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(timeline.Messages) != 3 || timeline.Messages[2].Content != "It is time to check your travel documents." {
		t.Fatalf("proactive reminder timeline = %+v", timeline.Messages)
	}
	notifications := notifier.snapshot()
	if len(notifications) != 1 || notifications[0] != "Eri reminder\nIt is time to check your travel documents" {
		t.Fatalf("notifications = %#v", notifications)
	}
	commitments, err := second.store.ListCommitments(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commitments) != 1 || commitments[0].Status != "completed" {
		t.Fatalf("event delivery replayed the historical scheduling request: %+v", commitments)
	}
	assertDataRootDoesNotContain(t, dataRoot, []byte("Check travel documents"))
}

func TestDaemonProposesRecurringWorkBeforeCreatingItAfterConsent(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-proactive-consent-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x68}, 32), Model: proactiveConsentModel{}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	sendAndWait := func(text string) channel.SendResult {
		var sent channel.SendResult
		integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": text}, &sent)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var status channel.TaskStatus
			integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+sent.TaskID, nil, &status)
			if status.Status == "completed" {
				return sent
			}
			if status.Status == "failed" {
				t.Fatalf("task failed: %+v", status)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s did not complete", sent.TaskID)
		return sent
	}

	proposal := sendAndWait("I keep researching AI every day")
	commitments, err := d.store.ListCommitments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commitments) != 0 {
		t.Fatalf("proposal task %s created work without consent: %+v", proposal.TaskID, commitments)
	}
	accepted := sendAndWait("yes create that daily brief")
	commitments, err = d.store.ListCommitments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commitments) != 1 || commitments[0].Schedule.Type != "daily" || commitments[0].Schedule.DailyTime != "09:00" || commitments[0].Schedule.Timezone != "Asia/Shanghai" || commitments[0].Target.RoutingMode != scheduler.DeliveryRouteRecent {
		t.Fatalf("accepted task %s commitments=%+v", accepted.TaskID, commitments)
	}
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

func TestDaemonCanCancelARunningTaskFromTheSameConversation(t *testing.T) {
	root := t.TempDir()
	socketFile, err := os.CreateTemp("", "eri-cancel-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: root, DatabasePath: filepath.Join(root, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	model := &cancelBehaviorModel{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x69}, 32), Model: model, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	var target channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "start the long cancellation target"}, &target)
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("long model call did not start")
	}
	var cancellation channel.SendResult
	integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": "cancel the long task"}, &cancellation)
	if cancellation.TaskID != target.TaskID {
		cancel()
		t.Fatalf("cancellation message task = %q, want active task %q", cancellation.TaskID, target.TaskID)
	}
	// Ordinary conversation input is a soft interruption: the paid model call
	// finishes, its stale candidate is fenced, and the same Loop then admits the
	// cancellation message. The explicit task-cancel API remains the separate
	// durable control-plane surface.
	close(model.release)
	var targetStatus channel.TaskStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+target.TaskID, nil, &targetStatus)
		if targetStatus.Status == "canceled" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if targetStatus.Status != "canceled" {
		cancel()
		t.Fatalf("target task = %+v", targetStatus)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=10", nil, &timeline)
	for _, message := range timeline.Messages {
		if strings.Contains(message.Content, "must not be delivered") {
			cancel()
			t.Fatalf("canceled candidate leaked into conversation: %+v", timeline.Messages)
		}
	}
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

func TestDaemonExportsAndStronglyErasesAllUserDataThroughConversation(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	socketFile, err := os.CreateTemp("", "eri-userdata-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: dataRoot, DatabasePath: filepath.Join(dataRoot, "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0",
		Model: "fake", ModelTimeout: time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x71}, 32), Model: userDataBehaviorModel{}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)

	send := func(text string) channel.SendResult {
		var sent channel.SendResult
		integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": text}, &sent)
		return sent
	}
	waitStatus := func(taskID string, accepted ...string) channel.TaskStatus {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var status channel.TaskStatus
			integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+taskID, nil, &status)
			for _, candidate := range accepted {
				if status.Status == candidate {
					return status
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s did not reach %v", taskID, accepted)
		return channel.TaskStatus{}
	}

	privateMarker := "seed private data omega"
	seed := send(privateMarker)
	if status := waitStatus(seed.TaskID, "completed", "failed"); status.Status != "completed" {
		t.Fatalf("seed task = %+v", status)
	}
	exported := send("export all my data")
	if status := waitStatus(exported.TaskID, "completed", "failed"); status.Status != "completed" {
		t.Fatalf("export task = %+v", status)
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=20", nil, &timeline)
	exportMessage := timeline.Messages[len(timeline.Messages)-1]
	if exportMessage.Content != "Your complete Eri data export is attached." || len(exportMessage.Attachments) != 1 || exportMessage.Attachments[0].Name != "eri-user-data-export.zip" {
		t.Fatalf("export delivery = %+v", exportMessage)
	}
	download, err := client.Get("http://eri.local/api/v1/attachments/" + exportMessage.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBody, err := io.ReadAll(download.Body)
	download.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(archiveBody), int64(len(archiveBody)))
	if err != nil {
		t.Fatal(err)
	}
	foundManifest, foundPrivateData := false, false
	for _, file := range archive.File {
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(entry)
		entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		foundManifest = foundManifest || file.Name == "manifest.json" && bytes.Contains(body, []byte("eri-user-data-export"))
		foundPrivateData = foundPrivateData || bytes.Contains(body, []byte(privateMarker))
	}
	if !foundManifest || !foundPrivateData {
		t.Fatalf("portable export manifest=%v private_data=%v", foundManifest, foundPrivateData)
	}

	erase := send("delete all my data")
	if status := waitStatus(erase.TaskID, "waiting", "failed"); status.Status != "waiting" {
		t.Fatalf("erasure approval task = %+v", status)
	}
	var approvalMessage channel.Message
	approvalDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(approvalDeadline) {
		integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=30", nil, &timeline)
		for index := len(timeline.Messages) - 1; index >= 0; index-- {
			candidate := timeline.Messages[index]
			if candidate.TaskID == erase.TaskID && candidate.Kind == "approval_request" {
				approvalMessage = candidate
				break
			}
		}
		if approvalMessage.ID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approvalMessage.ID == "" {
		t.Fatalf("approval message not delivered for task %s: %+v", erase.TaskID, timeline.Messages)
	}
	approvalID, _ := approvalMessage.Data["approval_id"].(string)
	if approvalID == "" || approvalMessage.Data["control"] != string(policy.StrongApproval) {
		t.Fatalf("strong approval data = %+v", approvalMessage.Data)
	}
	var approvalResult approval.Result
	integrationJSON(t, client, http.MethodPost, "/api/v1/approvals/"+approvalID, map[string]string{"decision": "approve"}, &approvalResult)
	if approvalResult.Status != "approved" {
		t.Fatalf("approval result = %+v", approvalResult)
	}
	if status := waitStatus(erase.TaskID, "completed", "failed"); status.Status != "completed" {
		t.Fatalf("erasure confirmation task = %+v", status)
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=30", nil, &timeline)
	confirmation := timeline.Messages[len(timeline.Messages)-1]
	if !strings.Contains(confirmation.Content, "permanently erase all local user content") {
		t.Fatalf("erasure confirmation = %+v", confirmation)
	}

	database, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	deadline := time.Now().Add(8 * time.Second)
	erasureStatus := ""
	for time.Now().Before(deadline) {
		err = database.QueryRow(`SELECT status FROM data_erasure_jobs ORDER BY created_at DESC LIMIT 1`).Scan(&erasureStatus)
		if err == nil && erasureStatus == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if erasureStatus != "completed" {
		t.Fatalf("erasure status = %q err=%v", erasureStatus, err)
	}
	for _, table := range []string{"interactions", "tasks", "memory_items", "episodes", "dataset_candidates", "feedback_records", "eval_records", "content_objects"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after erasure count=%d err=%v", table, count, err)
		}
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=20", nil, &timeline)
	if len(timeline.Messages) != 0 {
		t.Fatalf("conversation survived erasure: %+v", timeline.Messages)
	}

	fresh := send("fresh clean start")
	if status := waitStatus(fresh.TaskID, "completed", "failed"); status.Status != "completed" {
		t.Fatalf("fresh task = %+v", status)
	}
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

func TestDaemonInstallsInvokesAndStronglyUpgradesReferenceCalendarPlugin(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERI_CALENDAR_PLUGIN_HELPER", "1")
	writePluginManifest := func(name, version string, expanded bool) {
		environment := map[string]string{"ERI_CALENDAR_PLUGIN_HELPER": "1"}
		permissions := plugin.Permissions{
			DefaultEffect: policy.ReadOnly, ToolEffects: map[string]policy.EffectClass{"search_windows": policy.ReadOnly},
			DataCategories: []string{"calendar_availability"},
		}
		if expanded {
			environment["CALENDAR_WRITE_MODE"] = "enabled"
			permissions.SendsDataExternally = true
			permissions.NetworkDomains = []string{"calendar.example"}
			permissions.DataCategories = append(permissions.DataCategories, "calendar_events")
		}
		manifest := plugin.Manifest{
			SchemaVersion: 1, ID: "calendar", Name: "Reference Calendar", Version: version, Protocol: "mcp_stdio",
			Runtime:     plugin.Runtime{Command: executable, Arguments: []string{"-test.run=TestCalendarPluginHelperProcess"}, Environment: environment},
			Permissions: permissions,
		}
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePluginManifest("calendar-v1.json", "1.0.0", false)
	writePluginManifest("calendar-v2.json", "2.0.0", true)
	socketFile, err := os.CreateTemp("", "eri-calendar-plugin-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	socketFile.Close()
	os.Remove(socketPath)
	t.Cleanup(func() { os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: filepath.Join(root, "data"), DatabasePath: filepath.Join(root, "data", "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", WorkspaceRoot: workspace,
		Model: "fake", ModelTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x72}, 32), Model: referenceCalendarModel{}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	send := func(text string) channel.SendResult {
		var sent channel.SendResult
		integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": text}, &sent)
		return sent
	}
	waitFor := func(taskID string, statuses ...string) channel.TaskStatus {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			var status channel.TaskStatus
			integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+taskID, nil, &status)
			for _, expected := range statuses {
				if status.Status == expected {
					return status
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s did not reach %v", taskID, statuses)
		return channel.TaskStatus{}
	}
	approvePending := func(taskID string, expectedControl policy.ControlLevel) {
		if status := waitFor(taskID, "waiting", "failed"); status.Status != "waiting" {
			t.Fatalf("approval task=%+v", status)
		}
		var message channel.Message
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var timeline struct {
				Messages []channel.Message `json:"messages"`
			}
			integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=50", nil, &timeline)
			for index := len(timeline.Messages) - 1; index >= 0; index-- {
				if timeline.Messages[index].TaskID == taskID && timeline.Messages[index].Kind == "approval_request" {
					message = timeline.Messages[index]
					break
				}
			}
			if message.ID != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		approvalID, _ := message.Data["approval_id"].(string)
		if message.Kind != "approval_request" || approvalID == "" || message.Data["control"] != string(expectedControl) {
			t.Fatalf("approval=%+v", message)
		}
		var result approval.Result
		integrationJSON(t, client, http.MethodPost, "/api/v1/approvals/"+approvalID, map[string]string{"decision": "approve"}, &result)
		if result.Status != "approved" {
			t.Fatalf("approval result=%+v", result)
		}
		if status := waitFor(taskID, "completed", "failed"); status.Status != "completed" {
			t.Fatalf("resumed task=%+v", status)
		}
	}

	install := send("install calendar v1")
	approvePending(install.TaskID, policy.StrongApproval)
	search := send("find calendar windows")
	if status := waitFor(search.TaskID, "completed", "failed"); status.Status != "completed" {
		t.Fatalf("calendar search=%+v", status)
	}
	upgrade := send("upgrade calendar v2")
	approvePending(upgrade.TaskID, policy.StrongApproval)
	activeBody, err := os.ReadFile(filepath.Join(cfg.DataRoot, "plugins", "calendar", "active.json"))
	if err != nil || !bytes.Contains(activeBody, []byte(`"version":"2.0.0"`)) {
		t.Fatalf("active manifest=%s err=%v", activeBody, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataRoot, "plugins", "calendar", "versions", "1.0.0", "manifest.json")); err != nil {
		t.Fatalf("versioned v1 artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataRoot, "plugins", "calendar", "versions", "2.0.0", "manifest.json")); err != nil {
		t.Fatalf("versioned v2 artifact: %v", err)
	}
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

func TestDaemonInstallsAndSendsThroughReferenceEmailPluginWithSeparateApprovals(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERI_EMAIL_PLUGIN_HELPER", "1")
	manifest := plugin.Manifest{
		SchemaVersion: 1, ID: "email", Name: "Reference Email", Version: "1.0.0", Protocol: "mcp_stdio",
		Runtime: plugin.Runtime{Command: executable, Arguments: []string{"-test.run=TestEmailPluginHelperProcess"}, Environment: map[string]string{"ERI_EMAIL_PLUGIN_HELPER": "1"}},
		Permissions: plugin.Permissions{
			DefaultEffect: policy.Communication, ToolEffects: map[string]policy.EffectClass{"send_email": policy.Communication},
			SendsDataExternally: true, NetworkDomains: []string{"mail.example"}, DataCategories: []string{"recipient", "email_subject", "email_body"},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "email-v1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "eri-email-plugin-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg := config.Config{
		DataRoot: filepath.Join(root, "data"), DatabasePath: filepath.Join(root, "data", "metadata", "eri.db"), SocketPath: socketPath,
		ConversationAddr: "127.0.0.1:0", ObservatoryAddr: "127.0.0.1:0", WorkspaceRoot: workspace,
		Model: "fake", ModelTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg, Dependencies{MasterKey: bytes.Repeat([]byte{0x73}, 32), Model: referenceEmailModel{}, Judge: testPassJudge{}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForSocket(t, socketPath)
	client := integrationUnixClient(socketPath)
	send := func(text string) channel.SendResult {
		var sent channel.SendResult
		integrationJSON(t, client, http.MethodPost, "/api/v1/messages", map[string]string{"text": text}, &sent)
		return sent
	}
	waitFor := func(taskID string, statuses ...string) channel.TaskStatus {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			var status channel.TaskStatus
			integrationJSON(t, client, http.MethodGet, "/api/v1/tasks/"+taskID, nil, &status)
			for _, expected := range statuses {
				if status.Status == expected {
					return status
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s did not reach %v", taskID, statuses)
		return channel.TaskStatus{}
	}
	approve := func(taskID string, expectedControl policy.ControlLevel) string {
		if status := waitFor(taskID, "waiting", "failed"); status.Status != "waiting" {
			t.Fatalf("approval task=%+v", status)
		}
		var approvalID string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && approvalID == "" {
			var timeline struct {
				Messages []channel.Message `json:"messages"`
			}
			integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=50", nil, &timeline)
			for _, message := range timeline.Messages {
				if message.TaskID == taskID && message.Kind == "approval_request" {
					approvalID, _ = message.Data["approval_id"].(string)
					if message.Data["control"] != string(expectedControl) {
						t.Fatalf("unexpected email control=%+v", message.Data)
					}
				}
			}
			if approvalID == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if approvalID == "" {
			t.Fatal("approval message missing")
		}
		var result approval.Result
		integrationJSON(t, client, http.MethodPost, "/api/v1/approvals/"+approvalID, map[string]string{"decision": "approve"}, &result)
		if result.Status != "approved" {
			t.Fatalf("approval result=%+v", result)
		}
		if status := waitFor(taskID, "completed", "failed"); status.Status != "completed" {
			t.Fatalf("resumed task=%+v", status)
		}
		return approvalID
	}

	install := send("install email capability")
	installApproval := approve(install.TaskID, policy.StrongApproval)
	sendEmail := send("send approved email to alice@example.com")
	sendApproval := approve(sendEmail.TaskID, policy.OrdinaryConfirm)
	if installApproval == sendApproval {
		t.Fatal("plugin installation and represented communication reused one approval")
	}
	var timeline struct {
		Messages []channel.Message `json:"messages"`
	}
	integrationJSON(t, client, http.MethodGet, "/api/v1/messages?after=0&limit=50", nil, &timeline)
	foundReceipt := false
	for _, message := range timeline.Messages {
		foundReceipt = foundReceipt || (message.TaskID == sendEmail.TaskID && strings.Contains(message.Content, "msg-reference-001"))
	}
	if !foundReceipt {
		t.Fatalf("provider-confirmed delivery message missing: %+v", timeline.Messages)
	}
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

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("unix", path, 20*time.Millisecond)
		if err == nil {
			connection.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s did not become ready", path)
}

func integrationUnixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}, Timeout: 5 * time.Second}
}

func integrationJSON(t *testing.T, client *http.Client, method, path string, requestBody, responseBody any) {
	t.Helper()
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, "http://eri.local"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s: HTTP %d: %s", method, path, response.StatusCode, payload)
	}
	if responseBody != nil {
		if err := json.NewDecoder(response.Body).Decode(responseBody); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDataRootDoesNotContain(t *testing.T, root string, needle []byte) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // SQLite may remove a transient WAL/SHM file after WalkDir observed it.
			}
			return err
		}
		if bytes.Contains(body, needle) {
			return fmt.Errorf("plaintext found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
