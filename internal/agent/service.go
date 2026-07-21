package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/identity"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/tool"
)

type ContextRecord struct {
	ID          string
	TaskID      string
	DeliveryID  string
	Kind        string
	Sequence    int64
	Role        string
	ContentRef  content.Ref
	Attachments []ContextAttachment
}

type ContextAttachment struct {
	ID         string
	Name       string
	MediaType  string
	SizeBytes  int64
	ContentRef content.Ref
}

type TaskContext struct {
	TaskID string
	RunID  string
	// ExecutionID is set only for a nested execution, such as a native
	// subagent. The primary Agent execution is identified by RunID.
	ExecutionID          string
	SourceChannel        string
	InputSequence        int64
	ConversationSequence int64
	Messages             []ContextRecord
	CheckpointRef        content.Ref
	CheckpointPhase      string
	CurrentTask          execution.TaskCapsule
	ObjectiveRef         content.Ref
}

func (t TaskContext) ExecutionKey() string {
	if strings.TrimSpace(t.ExecutionID) != "" {
		return t.ExecutionID
	}
	return t.RunID
}

type ContextCheckpoint struct {
	ID                string
	SummaryRef        content.Ref
	FirstKeptSequence int64
	SummarizedCount   int
	TokensBefore      int
	TokensAfter       int
	SourceIDs         []string
}

type Commit struct {
	TaskID          string
	RunID           string
	ArtifactID      string
	EvalID          string
	DeliveryID      string
	ArtifactKind    string
	ArtifactRef     content.Ref
	TraceRef        content.Ref
	Usage           Usage
	EvalResult      eval.Result
	EvalFindings    []string
	EvalFindingsRef content.Ref
	EvalTier        string
	EvalEvaluator   string
	TerminalStatus  string
	FailureCode     string
	// BasisInputSequence is the newest user input included when this result was
	// produced. BasisConversationSequence is the newest authoritative message
	// from another Task that was reconciled. Repository commits reject results
	// produced behind either causal frontier.
	BasisInputSequence        int64
	BasisConversationSequence int64
	Attachments               []ArtifactAttachment
}

type ProgressCommit struct {
	Commit
	ModelTurnID string
}

type ArtifactAttachment struct {
	ID         string
	Name       string
	MediaType  string
	SizeBytes  int64
	ContentRef content.Ref
}

type ApprovalCommit struct {
	TaskID          string
	RunID           string
	ApprovalID      string
	ArtifactID      string
	EvalID          string
	DeliveryID      string
	Intent          tool.Intent
	ArtifactRef     content.Ref
	ContinuationRef content.Ref
	EvalResult      eval.Result
	EvalFindings    []string
	EvalFindingsRef content.Ref
	EvalTier        string
	EvalEvaluator   string
	ExpiresAt       time.Time
}

type ApprovalResume struct {
	Task            TaskContext
	ApprovalID      string
	Decision        string
	Grant           *tool.Grant
	ContinuationRef content.Ref
}

type SubagentWaitCommit struct {
	TaskID          string
	RunID           string
	DelegationID    string
	RoleID          string
	ProviderID      string
	ArtifactID      string
	EvalID          string
	DeliveryID      string
	ArtifactRef     content.Ref
	TraceRef        content.Ref
	ContinuationRef content.Ref
	Usage           Usage
	EvalResult      eval.Result
	EvalFindings    []string
	EvalFindingsRef content.Ref
	EvalTier        string
	EvalEvaluator   string
}

type SubagentResume struct {
	Task            TaskContext
	DelegationID    string
	RoleID          string
	ProviderID      string
	Status          string
	ResultRef       content.Ref
	ContinuationRef content.Ref
}

var ErrSubagentProgressPending = errors.New("subagent progress delivery is still pending")

// ErrStaleTaskInput means a user message joined the active task after a model
// result was produced but before it crossed a durable side-effect boundary.
var ErrStaleTaskInput = errors.New("task input changed")

// ErrStaleConversationContext means another Task advanced the authoritative
// Conversation after the current Task assembled its context.
var ErrStaleConversationContext = errors.New("conversation context changed")

type Repository interface {
	ClaimTask(context.Context, string, string, time.Duration, string, string, string) (TaskContext, bool, error)
	MarkRunDispatched(context.Context, string) error
	CommitArtifact(context.Context, Commit) error
	CommitProgress(context.Context, ProgressCommit) (bool, error)
	PauseForApproval(context.Context, ApprovalCommit) error
	ClaimApprovalResume(context.Context, string, string, time.Duration) (ApprovalResume, bool, error)
	PauseForSubagent(context.Context, SubagentWaitCommit) error
	ClaimSubagentResume(context.Context, string, string, time.Duration) (SubagentResume, bool, error)
	UpdateRunContext(context.Context, string, string) error
	TaskCancelRequested(context.Context, string) (bool, error)
	CommitTaskCancellation(context.Context, string, string, content.Ref, Usage) error
	SaveContextCheckpoint(context.Context, string, string, ContextCheckpoint) error
	SaveAgentCheckpoint(context.Context, TaskContext, string, content.Ref) error
	LoadTaskInputsAfter(context.Context, string, int64) ([]ContextRecord, error)
	LoadConversationUpdatesAfter(context.Context, string, int64) ([]ContextRecord, error)
}

type ContentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	Get(context.Context, content.Ref) ([]byte, error)
	Delete(context.Context, content.Ref) error
}

type ToolGateway interface {
	Descriptors() []tool.Descriptor
	Invoke(context.Context, tool.Request) (tool.Outcome, error)
}

type MemoryRetriever interface {
	Recall(context.Context, memory.RecallRequest) (memory.Bundle, error)
}

type ModelBudget interface {
	Reserve(context.Context, string, int) (string, error)
	Settle(context.Context, string, int, bool) error
}

type SkillCatalog interface {
	Prompt(context.Context) (string, error)
}

type EvolutionSignal struct {
	RunID               string
	ExperienceReleaseID string
	Result              eval.Result
	Tier                string
	Findings            []string
}

type Experience struct {
	ReleaseID string
	Version   int
	Text      string
}

type EvolutionProvider interface {
	ExperienceForRun(context.Context, string) (Experience, bool, error)
	Observe(context.Context, EvolutionSignal) error
}

type LoopConfig struct {
	MaxEvalAttempts int
	MaxOutputTokens int
	ApprovalTTL     time.Duration
	ExternalModel   bool
	Budget          ModelBudget
	Skills          SkillCatalog
	Judge           Judge
	ModelTarget     string
	Evolution       EvolutionProvider
	Logger          *slog.Logger
}

const (
	minimumContextReserve = 4_096
	defaultRecentTokens   = 20_000
)

type modelTurnTrace struct {
	ID            string            `json:"id"`
	Ordinal       int               `json:"ordinal"`
	Trigger       string            `json:"trigger"`
	Status        string            `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       time.Time         `json:"ended_at"`
	Checkpoints   []string          `json:"checkpoints,omitempty"`
	InputSequence int64             `json:"input_sequence"`
	FinishReason  string            `json:"finish_reason,omitempty"`
	Request       modelRequestTrace `json:"request"`
	Message       Message           `json:"assistant"`
	Usage         Usage             `json:"usage"`
}

type modelRequestTrace struct {
	MessageCount         int            `json:"message_count"`
	MessageRoles         map[string]int `json:"message_roles"`
	ToolNames            []string       `json:"tool_names,omitempty"`
	MaxOutputTokens      int            `json:"max_output_tokens"`
	EstimatedInputTokens int            `json:"estimated_input_tokens"`
}

type activeTurnTrace struct {
	ID            string            `json:"id"`
	Ordinal       int               `json:"ordinal"`
	Trigger       string            `json:"trigger"`
	StartedAt     time.Time         `json:"started_at"`
	Checkpoints   []string          `json:"checkpoints,omitempty"`
	InputSequence int64             `json:"input_sequence"`
	Request       modelRequestTrace `json:"request"`
}

type toolResultTrace struct {
	ModelTurnID string      `json:"model_turn_id"`
	ToolCallID  string      `json:"tool_call_id"`
	ToolID      string      `json:"tool_id,omitempty"`
	IntentID    string      `json:"intent_id,omitempty"`
	Status      string      `json:"status"`
	ResultRef   content.Ref `json:"result_ref,omitempty"`
}

type runTrace struct {
	// ProviderTranscript is the authoritative provider-native request at the
	// durable boundary. It remains encrypted and user-owned with the Run trace;
	// Observatory decodes only the safe fields below.
	ProviderTranscript *ModelRequest     `json:"provider_transcript,omitempty"`
	ModelTurns         []modelTurnTrace  `json:"model_turns"`
	ToolCalls          []toolResultTrace `json:"tool_calls"`
	Evaluations        []evaluationTrace `json:"evaluations"`
	Progress           []progressTrace   `json:"progress,omitempty"`
	RuntimeStop        string            `json:"runtime_stop,omitempty"`
	FailureCause       string            `json:"failure_cause,omitempty"`
}

type progressTrace struct {
	ID          string    `json:"id"`
	ModelTurnID string    `json:"model_turn_id"`
	DeliveryID  string    `json:"delivery_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type evaluationTrace struct {
	ID          string      `json:"id"`
	ModelTurnID string      `json:"model_turn_id"`
	Attempt     int         `json:"attempt"`
	StartedAt   time.Time   `json:"started_at"`
	EndedAt     time.Time   `json:"ended_at"`
	Result      eval.Result `json:"result"`
	Tier        string      `json:"tier"`
	Findings    []string    `json:"findings,omitempty"`
	Usage       Usage       `json:"usage"`
}

type loopState struct {
	Trace                runTrace                  `json:"trace"`
	Usage                Usage                     `json:"usage"`
	ConfirmedEffects     int                       `json:"confirmed_effects"`
	TurnsUsed            int                       `json:"turns_used"`
	EvalAttempts         int                       `json:"eval_attempts"`
	SkillIDs             []string                  `json:"skill_ids"`
	TaskText             string                    `json:"task_text"`
	JudgeContext         string                    `json:"judge_context,omitempty"`
	InputSequence        int64                     `json:"input_sequence"`
	ConversationSequence int64                     `json:"conversation_sequence"`
	EvalFindings         []string                  `json:"eval_findings,omitempty"`
	Attachments          []ArtifactAttachment      `json:"attachments,omitempty"`
	ContextManifest      execution.ContextManifest `json:"context_manifest"`
	Capabilities         ModelCapabilities         `json:"capabilities"`
	ProgressDigest       string                    `json:"progress_digest,omitempty"`
	StagnantTurns        int                       `json:"stagnant_turns,omitempty"`
	SynthesisOnly        bool                      `json:"synthesis_only,omitempty"`
	LastProgressHash     string                    `json:"last_progress_hash,omitempty"`
	ActiveTurn           *activeTurnTrace          `json:"active_turn,omitempty"`
	NextTurnTrigger      string                    `json:"next_turn_trigger,omitempty"`
	PendingDeferred      *pendingDeferred          `json:"pending_deferred,omitempty"`
}

type pendingDeferred struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	RoleID     string `json:"role_id"`
	ProviderID string `json:"provider_id"`
	ToolCallID string `json:"tool_call_id"`
	IntentID   string `json:"intent_id"`
}

type pendingContinuation struct {
	Request      ModelRequest      `json:"request"`
	ModelToolIDs map[string]string `json:"model_tool_ids"`
	PendingCalls []ToolCall        `json:"pending_calls"`
	State        loopState         `json:"state"`
}

type Service struct {
	repository Repository
	content    ContentStore
	model      Model
	identity   identity.Snapshot
	tools      ToolGateway
	memory     MemoryRetriever
	owner      string
	lease      time.Duration
	loop       LoopConfig
	judge      Judge
	evolution  EvolutionProvider
	logger     *slog.Logger
}

func NewService(repository Repository, contentStore ContentStore, model Model, snapshot identity.Snapshot, owner string, gateway ToolGateway, memories MemoryRetriever, loop LoopConfig) *Service {
	if loop.MaxEvalAttempts <= 0 {
		loop.MaxEvalAttempts = 3
	}
	if loop.MaxOutputTokens <= 0 {
		loop.MaxOutputTokens = 1024
	}
	if loop.ApprovalTTL <= 0 {
		loop.ApprovalTTL = 15 * time.Minute
	}
	judge := loop.Judge
	if judge == nil {
		judge, _ = NewModelJudge(model)
	}
	logger := loop.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repository: repository, content: contentStore, model: model,
		identity: snapshot, tools: gateway, memory: memories, owner: owner, lease: 2 * time.Minute, loop: loop, judge: judge, evolution: loop.Evolution, logger: logger,
	}
}

func (s *Service) HandleWake(ctx context.Context, item runtime.OutboxItem) error {
	return s.ProcessTask(ctx, item.AggregateID)
}

func (s *Service) HandleApprovalResume(ctx context.Context, item runtime.OutboxItem) error {
	started := time.Now()
	resume, claimed, err := s.repository.ClaimApprovalResume(ctx, item.AggregateID, s.owner, s.lease)
	if err != nil || !claimed {
		if err != nil {
			s.logger.Error("approval resume failed", "component", "agent", "approval_id", item.AggregateID, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		}
		return err
	}
	s.logger.Info("approval resumed", "component", "agent", "approval_id", resume.ApprovalID, "task_id", resume.Task.TaskID, "run_id", resume.Task.RunID, "decision", resume.Decision)
	body, err := s.content.Get(ctx, resume.ContinuationRef)
	if err != nil {
		return fmt.Errorf("read approval continuation: %w", err)
	}
	var continuation pendingContinuation
	if err := json.Unmarshal(body, &continuation); err != nil {
		return fmt.Errorf("decode approval continuation: %w", err)
	}
	s.restoreJudgeContext(resume.Task, &continuation)
	if len(continuation.PendingCalls) == 0 {
		return fmt.Errorf("approval %s has no pending tool call", resume.ApprovalID)
	}
	conversationChanged, err := s.refreshConversationUpdates(ctx, resume.Task, &continuation.Request, &continuation.State)
	if err != nil {
		return err
	}
	if conversationChanged && resume.Decision == "approved" {
		// The grant remains truthful evidence, but its planned call was based on an
		// older Conversation. Re-enter the Loop before any side effect so the model
		// can reconcile the new context and request fresh authority if still needed.
		return s.continueAfterInterruptedToolFrame(ctx, resume.Task, &continuation, "approval_conversation_update")
	}
	call := continuation.PendingCalls[0]
	continuation.PendingCalls = continuation.PendingCalls[1:]
	if resume.Decision != "approved" {
		reason := "the user denied this exact action"
		if resume.Decision == "expired" {
			reason = "approval expired before the action was authorized"
		}
		continuation.Request.Messages = append(continuation.Request.Messages, toolErrorMessage(call, reason))
		continuation.State.Trace.ToolCalls = append(continuation.State.Trace.ToolCalls, toolResultTrace{
			ModelTurnID: latestModelTurnID(&continuation.State), ToolCallID: call.ID, Status: "user_denied",
		})
		if err := s.saveToolBatchCheckpoint(ctx, resume.Task, continuation.Request, continuation.PendingCalls, continuation.ModelToolIDs, continuation.State); err != nil {
			return err
		}
	} else {
		paused, err := s.executeCalls(ctx, resume.Task, &continuation.Request, []ToolCall{call}, continuation.ModelToolIDs, &continuation.State, resume.Grant)
		if errors.Is(err, ErrStaleTaskInput) || errors.Is(err, ErrStaleConversationContext) {
			return s.continueAfterInterruptedToolFrame(ctx, resume.Task, &continuation, "approval_resume")
		}
		if err != nil || paused {
			return err
		}
	}
	if len(continuation.PendingCalls) > 0 {
		paused, err := s.executeCalls(ctx, resume.Task, &continuation.Request, continuation.PendingCalls, continuation.ModelToolIDs, &continuation.State, nil)
		if errors.Is(err, ErrStaleTaskInput) || errors.Is(err, ErrStaleConversationContext) {
			return s.continueAfterInterruptedToolFrame(ctx, resume.Task, &continuation, "approval_resume")
		}
		if err != nil || paused {
			return err
		}
	}
	continuation.State.NextTurnTrigger = "tool_observations"
	err = s.continueLoop(ctx, resume.Task, continuation.Request, continuation.ModelToolIDs, continuation.State)
	s.logger.Info("approval continuation finished", "component", "agent", "approval_id", resume.ApprovalID, "task_id", resume.Task.TaskID, "duration_ms", time.Since(started).Milliseconds(), "error_code", observability.ErrorCode(err))
	return err
}

func (s *Service) HandleSubagentResume(ctx context.Context, item runtime.OutboxItem) error {
	started := time.Now()
	resume, claimed, err := s.repository.ClaimSubagentResume(ctx, item.AggregateID, s.owner, s.lease)
	if err != nil || !claimed {
		return err
	}
	continuationBody, err := s.content.Get(ctx, resume.ContinuationRef)
	if err != nil {
		return fmt.Errorf("read delegation continuation: %w", err)
	}
	var continuation pendingContinuation
	if err := json.Unmarshal(continuationBody, &continuation); err != nil {
		return fmt.Errorf("decode delegation continuation: %w", err)
	}
	s.restoreJudgeContext(resume.Task, &continuation)
	if continuation.State.PendingDeferred == nil || continuation.State.PendingDeferred.ID != resume.DelegationID ||
		continuation.State.PendingDeferred.Kind != "subagent" || continuation.State.PendingDeferred.RoleID != resume.RoleID ||
		continuation.State.PendingDeferred.ProviderID != resume.ProviderID {
		return fmt.Errorf("subagent delegation %s does not match its continuation", resume.DelegationID)
	}
	resultBody, err := s.content.Get(ctx, resume.ResultRef)
	if err != nil {
		return fmt.Errorf("read delegation result: %w", err)
	}
	pending := *continuation.State.PendingDeferred
	continuation.State.PendingDeferred = nil
	continuation.State.NextTurnTrigger = "subagent_result"
	if _, err := s.refreshConversationUpdates(ctx, resume.Task, &continuation.Request, &continuation.State); err != nil {
		return err
	}
	if err := replaceDeferredToolResult(continuation.Request.Messages, pending.ToolCallID, resume.RoleID, resume.Status, resultBody); err != nil {
		return err
	}
	continuation.Request.Messages = append(continuation.Request.Messages, Message{
		Role: "system",
		Content: fmt.Sprintf(
			"<system_event type=\"subagent.terminal\" role_id=\"%s\" status=\"%s\">\nThe delegated work reached a terminal state. Its governed result is now in the original builtin.delegate tool observation. Review that observation and continue the user's task.\n</system_event>",
			resume.RoleID, resume.Status,
		),
	})
	err = s.continueLoop(ctx, resume.Task, continuation.Request, continuation.ModelToolIDs, continuation.State)
	s.logger.Info("delegation continuation finished", "component", "agent", "delegation_id", resume.DelegationID, "task_id", resume.Task.TaskID, "status", resume.Status, "duration_ms", time.Since(started).Milliseconds(), "error_code", observability.ErrorCode(err))
	return err
}

func replaceDeferredToolResult(messages []Message, toolCallID, roleID, status string, resultBody []byte) error {
	if strings.TrimSpace(toolCallID) == "" {
		return fmt.Errorf("deferred subagent tool call id is required")
	}
	var result any
	if err := json.Unmarshal(resultBody, &result); err != nil {
		return fmt.Errorf("decode subagent result: %w", err)
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != "tool" || messages[index].ToolCallID != toolCallID {
			continue
		}
		observation := map[string]any{
			"tool_id": "builtin.delegate",
			"status":  status,
			"success": status == "completed",
			"result": map[string]any{
				"kind": "subagent_result", "role_id": roleID, "output": result,
			},
		}
		encoded, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		messages[index].Content = string(encoded)
		return nil
	}
	return fmt.Errorf("deferred subagent tool result %s is missing from continuation", toolCallID)
}

func (s *Service) ProcessTask(ctx context.Context, taskID string) error {
	started := time.Now()
	s.logger.Info("task processing started", "component", "agent", "task_id", taskID)
	descriptors := []tool.Descriptor{}
	if s.tools != nil {
		descriptors = s.tools.Descriptors()
	}
	manifestValue := execution.ContextManifest{
		IdentityID:       s.identity.ID,
		SoulVersion:      s.identity.Version,
		MemoryIDs:        []string{},
		SkillIDs:         []string{},
		ToolIDs:          []string{},
		ExternalDataSent: s.loop.ExternalModel,
		ResponseProfile:  "soul_guided",
	}
	manifest, err := json.Marshal(manifestValue)
	if err != nil {
		return fmt.Errorf("encode context manifest: %w", err)
	}
	task, claimed, err := s.repository.ClaimTask(ctx, taskID, s.owner, s.lease, s.identity.Version, string(manifest), s.loop.ModelTarget)
	if err != nil {
		return err
	}
	if !claimed {
		s.logger.Debug("task claim skipped", "component", "agent", "task_id", taskID)
		return nil
	}
	descriptors = descriptorsForTask(descriptors, task.CurrentTask)
	toolIDs := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		toolIDs = append(toolIDs, descriptor.ID+"@"+descriptor.Version)
	}
	definitions, modelToolIDs, err := buildToolDefinitions(descriptors)
	if err != nil {
		return err
	}
	manifestValue.ToolIDs = toolIDs
	observedAt := time.Now()
	manifestValue.SourceChannel = task.SourceChannel
	manifestValue.RuntimeObservedAt = observedAt.UTC()
	manifestValue.RuntimeTimezone = observedAt.Location().String()
	if task.CurrentTask.TaskID != "" {
		currentTask := task.CurrentTask
		manifestValue.CurrentTask = &currentTask
	}
	s.logger.Info("run claimed", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey())
	if task.CheckpointRef.ObjectID != "" {
		if err := s.repository.MarkRunDispatched(ctx, task.RunID); err != nil {
			return err
		}
		return s.resumeAgentCheckpoint(ctx, task, modelToolIDs)
	}
	experience := Experience{}
	if s.evolution != nil {
		selected, found, err := s.evolution.ExperienceForRun(ctx, task.RunID)
		if err != nil {
			return s.commitFailure(ctx, task, Usage{}, "experience_unavailable")
		}
		if found {
			experience = selected
			manifestValue.ExperienceReleaseID = selected.ReleaseID
			manifestValue.ExperienceReleaseVersion = selected.Version
		}
	}
	capabilities, err := capabilitiesFor(ctx, s.model)
	if err != nil {
		return s.commitFailure(ctx, task, Usage{}, "provider_capabilities_unavailable")
	}
	messages, err := s.buildMessages(ctx, task, capabilities)
	if err != nil {
		return s.commitFailure(ctx, task, Usage{}, "context_unavailable")
	}
	taskText := latestTaskContentForTask(messages, task.Messages, task.TaskID)
	if strings.TrimSpace(taskText) == "" {
		taskText = latestTaskContent(messages)
	}
	if strings.TrimSpace(taskText) == "" && task.ObjectiveRef.ObjectID != "" {
		body, err := s.content.Get(ctx, task.ObjectiveRef)
		if err != nil {
			return s.commitFailure(ctx, task, Usage{}, "task_objective_unavailable")
		}
		taskText = string(body)
	}
	if err := s.repository.MarkRunDispatched(ctx, task.RunID); err != nil {
		return err
	}
	memoryBundle := memory.Bundle{}
	skillContext := ""
	if s.loop.Skills != nil {
		skillContext, err = s.loop.Skills.Prompt(ctx)
		if err != nil {
			return s.commitFailure(ctx, task, Usage{}, "skill_catalog_unavailable")
		}
	}
	if s.memory != nil {
		bundle, err := s.memory.Recall(ctx, memory.RecallRequest{
			Query: memoryAttentionCue(taskText, messages), RunID: task.RunID,
			SourceInteractionID: task.CurrentTask.SourceInteractionID, Limit: 5,
		})
		if err != nil {
			return s.commitFailure(ctx, task, Usage{}, "memory_unavailable")
		}
		memoryIDs := make([]string, 0, len(bundle.Entries))
		memoryBundle = bundle
		for _, entry := range bundle.Entries {
			memoryIDs = append(memoryIDs, entry.MemoryID)
		}
		manifestValue.MemoryChecked = true
		manifestValue.MemoryRetrievalID = bundle.RetrievalID
		manifestValue.RetrievedMemoryIDs = append([]string(nil), bundle.RetrievedIDs...)
		manifestValue.MemoryIDs = memoryIDs
		if s.loop.ExternalModel && len(memoryIDs) > 0 {
			manifestValue.ExternalMemoryIDs = append([]string(nil), memoryIDs...)
		}
	}
	manifestValue.ProviderCapabilities = capabilities
	manifestValue.MessageIDs = contextRecordIDs(task.Messages, "")
	conversationSequence := int64(0)
	if task.CurrentTask.SourceRole == "user" {
		conversationSequence = task.ConversationSequence
		manifestValue.ConversationSequence = conversationSequence
	}
	manifestValue.AttachmentIDs = contextAttachmentIDs(task.Messages)
	manifestValue.ContextWindowTokens = capabilities.ContextTokens
	manifestValue.OutputReservedTokens = s.loop.MaxOutputTokens
	if s.loop.ExternalModel {
		manifestValue.ExternalData = &execution.ExternalData{
			MessageIDs: contextRecordIDs(task.Messages, "context_checkpoint"),
			MemoryIDs:  append([]string(nil), manifestValue.MemoryIDs...),
			SkillIDs:   append([]string(nil), manifestValue.SkillIDs...),
			Categories: []string{"conversation", "selected_memory", "tool_schemas"},
		}
	}
	prompts := assembleRunPrompts(s.identity, skillContext, experience, memoryBundle, task.SourceChannel, observedAt)
	request := ModelRequest{
		System:   prompts.AgentSystem,
		Messages: append([]Message(nil), messages...), Tools: definitions,
		MaxOutputTokens: minPositive(s.loop.MaxOutputTokens, capabilities.MaxOutputTokens),
	}
	request, compactionUsage, err := s.compactPersistentContext(ctx, task, request, capabilities, &manifestValue)
	if err != nil {
		s.logger.Warn("persistent context compaction failed", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
		return s.commitFailure(ctx, task, compactionUsage, "context_compaction_failed")
	}
	if prompts.MemoryContext != nil {
		request.Messages = insertBeforeSourceInteraction(
			request.Messages,
			task.Messages,
			task.CurrentTask.SourceInteractionID,
			manifestValue.Compression.SummarizedCount,
			*prompts.MemoryContext,
		)
	}
	manifestValue.EstimatedInputTokens = estimateModelInputTokens(request)
	updatedManifest, err := json.Marshal(manifestValue)
	if err != nil {
		return err
	}
	if err := s.repository.UpdateRunContext(ctx, task.RunID, string(updatedManifest)); err != nil {
		return err
	}
	state := loopState{
		Trace: runTrace{ModelTurns: []modelTurnTrace{}, ToolCalls: []toolResultTrace{}, Evaluations: []evaluationTrace{}}, Usage: compactionUsage,
		SkillIDs: append([]string(nil), manifestValue.SkillIDs...), TaskText: taskText, JudgeContext: prompts.JudgeContext,
		InputSequence: task.InputSequence, ConversationSequence: conversationSequence, ContextManifest: manifestValue,
		Capabilities: capabilities,
	}
	err = s.continueLoop(ctx, task, request, modelToolIDs, state)
	s.logger.Info("task processing finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "duration_ms", time.Since(started).Milliseconds(), "error_code", observability.ErrorCode(err))
	return err
}

// descriptorsForTask applies phase authority before Tool schemas enter model
// context. A due commitment is already registered durable work, so its
// fulfillment run cannot mutate the scheduler that caused it to exist.
func descriptorsForTask(descriptors []tool.Descriptor, task execution.TaskCapsule) []tool.Descriptor {
	if task.ExecutionPhase != execution.TaskPhaseFulfillment || task.TriggerEvent != execution.TriggerEventCommitmentDue {
		return descriptors
	}
	filtered := make([]tool.Descriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.ID != "builtin.commitments" {
			filtered = append(filtered, descriptor)
		}
	}
	return filtered
}

func (s *Service) resumeAgentCheckpoint(ctx context.Context, task TaskContext, currentToolIDs map[string]string) error {
	body, err := s.content.Get(ctx, task.CheckpointRef)
	if err != nil {
		return fmt.Errorf("read agent checkpoint: %w", err)
	}
	var continuation pendingContinuation
	if err := json.Unmarshal(body, &continuation); err != nil {
		return fmt.Errorf("decode agent checkpoint: %w", err)
	}
	restoreConversationWatermark(task, &continuation)
	s.restoreJudgeContext(task, &continuation)
	if !sameStringMap(continuation.ModelToolIDs, currentToolIDs) {
		continuation.State.Trace.RuntimeStop = "tool_surface_changed_during_recovery"
		return s.commitFailure(ctx, task, continuation.State.Usage, "tool_surface_changed_during_recovery", traceWithProviderTranscript(continuation.State.Trace, continuation.Request))
	}
	switch task.CheckpointPhase {
	case "ready_for_model":
		if _, err := s.refreshConversationUpdates(ctx, task, &continuation.Request, &continuation.State); err != nil {
			return err
		}
		return s.continueLoop(ctx, task, continuation.Request, continuation.ModelToolIDs, continuation.State)
	case "model_received":
		if latestToolFrameAssistantIndex(continuation.Request.Messages) < 0 {
			return fmt.Errorf("recovered model checkpoint has no assistant tool-call frame")
		}
		if changed, err := s.refreshConversationUpdates(ctx, task, &continuation.Request, &continuation.State); err != nil {
			return err
		} else if changed {
			return s.continueAfterInterruptedToolFrame(ctx, task, &continuation, "checkpoint_conversation_update")
		}
		paused, err := s.executeRecoveredCalls(ctx, task, &continuation.Request, continuation.PendingCalls, continuation.ModelToolIDs, &continuation.State, nil)
		if errors.Is(err, ErrStaleTaskInput) || errors.Is(err, ErrStaleConversationContext) {
			return s.continueAfterInterruptedToolFrame(ctx, task, &continuation, "checkpoint_recovery")
		}
		if err != nil || paused {
			return err
		}
		if continuation.State.PendingDeferred == nil && len(continuation.Request.Messages) > 0 {
			message := continuation.Request.Messages[latestToolFrameAssistantIndex(continuation.Request.Messages)]
			if strings.TrimSpace(message.Content) != "" {
				if err := s.commitIntermediateProgress(ctx, task, continuation.Request, &continuation.State, message.Content); err != nil {
					s.logger.Warn("recovered progress message was withheld", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
				}
			}
		}
		continuation.State.NextTurnTrigger = "tool_observations"
		return s.continueLoop(ctx, task, continuation.Request, continuation.ModelToolIDs, continuation.State)
	case "candidate_received":
		next, again, err := s.evaluateAndCommitCandidate(ctx, task, continuation.Request, continuation.ModelToolIDs, &continuation.State)
		if err != nil || !again {
			return err
		}
		return s.continueLoop(ctx, task, next, continuation.ModelToolIDs, continuation.State)
	default:
		return fmt.Errorf("unsupported recovered agent checkpoint phase %q", task.CheckpointPhase)
	}
}

func restoreConversationWatermark(task TaskContext, continuation *pendingContinuation) {
	if task.CurrentTask.SourceRole != "user" || continuation.State.ConversationSequence > 0 {
		return
	}
	baseline := continuation.State.InputSequence
	if baseline <= 0 {
		baseline = task.InputSequence
	}
	if baseline <= 0 {
		return
	}
	continuation.State.ConversationSequence = baseline
	continuation.State.ContextManifest.ConversationSequence = baseline
}

func (s *Service) restoreJudgeContext(task TaskContext, continuation *pendingContinuation) {
	if continuation == nil || strings.TrimSpace(continuation.State.JudgeContext) != "" {
		return
	}
	manifest := continuation.State.ContextManifest
	observedAt := manifest.RuntimeObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	} else if timezone := strings.TrimSpace(manifest.RuntimeTimezone); timezone != "" {
		if location, err := time.LoadLocation(timezone); err == nil {
			observedAt = observedAt.In(location)
		}
	}
	sourceChannel := strings.TrimSpace(task.SourceChannel)
	if sourceChannel == "" {
		sourceChannel = strings.TrimSpace(manifest.SourceChannel)
	}
	continuation.State.JudgeContext = candidateEvaluationContextFromEvidence(
		s.identity,
		recoverPromptMemoryEvidence(continuation.Request.System, manifest.MemoryRetrievalID, manifest.MemoryIDs),
		sourceChannel,
		observedAt,
	)
}

func (s *Service) continueAfterInterruptedToolFrame(ctx context.Context, task TaskContext, continuation *pendingContinuation, source string) error {
	assistantIndex := latestToolFrameAssistantIndex(continuation.Request.Messages)
	retained, err := closeInterruptedToolFrame(&continuation.Request, assistantIndex, continuation.ModelToolIDs, &continuation.State)
	if err != nil {
		return err
	}
	s.logger.Info("tool turn interrupted by newer input", "component", "agent", "source", source, "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "input_sequence", continuation.State.InputSequence, "protocol_frame_retained", retained)
	continuation.State.NextTurnTrigger = "user_input"
	continuation.PendingCalls = nil
	if err := s.saveAgentCheckpoint(ctx, task, "ready_for_model", *continuation); err != nil {
		return fmt.Errorf("save interrupted-tool checkpoint: %w", err)
	}
	return s.continueLoop(ctx, task, continuation.Request, continuation.ModelToolIDs, continuation.State)
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (s *Service) saveAgentCheckpoint(ctx context.Context, task TaskContext, phase string, continuation pendingContinuation) error {
	body, err := json.Marshal(continuation)
	if err != nil {
		return fmt.Errorf("encode agent checkpoint: %w", err)
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "agent_checkpoint",
		PrivacyClass: "private", RetentionPolicy: "until_task_complete", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store agent checkpoint: %w", err)
	}
	return s.repository.SaveAgentCheckpoint(ctx, task, phase, ref)
}

func (s *Service) commitEvaluatedReply(ctx context.Context, task TaskContext, trace any, usage Usage, body, kind, tier string, findings []string, attachments []ArtifactAttachment, basisInputSequence, basisConversationSequence int64) error {
	result, gateFindings := eval.Routine(body)
	if result != eval.Pass {
		return s.commitFailure(ctx, task, usage, "routine_eval_failed")
	}
	if len([]byte(body)) > 12*1024 && kind == "text" {
		attachmentID, err := identifier.New()
		if err != nil {
			return err
		}
		attachmentRef, err := s.content.Put(ctx, []byte(body), content.Metadata{
			MediaType: "text/markdown; charset=utf-8", EncryptionDomain: "attachment",
			PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
		})
		if err != nil {
			return fmt.Errorf("store long-form attachment: %w", err)
		}
		attachments = append(attachments, ArtifactAttachment{
			ID: attachmentID, Name: "eri-result-" + shortID(task.TaskID) + ".md", MediaType: "text/markdown; charset=utf-8",
			SizeBytes: int64(len([]byte(body))), ContentRef: attachmentRef,
		})
	}
	ref, err := s.content.Put(ctx, []byte(body), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "conversation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store candidate artifact: %w", err)
	}
	traceRef, err := s.storeTrace(ctx, task.ExecutionKey(), trace)
	if err != nil {
		return err
	}
	commit, err := newCommit(task, kind, ref)
	if err != nil {
		return err
	}
	commit.TraceRef = traceRef
	commit.Usage = usage
	commit.EvalResult = result
	commit.EvalFindings = append([]string(nil), gateFindings...)
	commit.EvalTier = tier
	commit.EvalEvaluator = "llm_judge"
	commit.EvalFindings = append(commit.EvalFindings, findings...)
	commit.TerminalStatus = "completed"
	commit.BasisInputSequence = basisInputSequence
	commit.BasisConversationSequence = basisConversationSequence
	commit.Attachments = attachments
	commit.EvalFindingsRef, err = s.storeEvalFindings(ctx, commit.EvalID, commit.EvalFindings)
	if err != nil {
		return err
	}
	if err := s.repository.CommitArtifact(ctx, commit); err != nil {
		_ = s.content.Delete(context.Background(), ref)
		_ = s.content.Delete(context.Background(), traceRef)
		_ = s.content.Delete(context.Background(), commit.EvalFindingsRef)
		return err
	}
	return nil
}

func (s *Service) commitIntermediateProgress(ctx context.Context, task TaskContext, request ModelRequest, state *loopState, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	if digest == state.LastProgressHash {
		return nil
	}
	if result, _ := eval.Routine(body); result != eval.Pass {
		return fmt.Errorf("progress message failed the deterministic delivery gate")
	}
	confirmedTools := make([]string, 0)
	for _, call := range state.Trace.ToolCalls {
		if call.Status == string(tool.IntentConfirmed) {
			confirmedTools = append(confirmedTools, call.ToolID)
		}
	}
	judgeMessages := append([]Message(nil), request.Messages...)
	// The progress Candidate is evaluated after the complete Tool frame. Keep
	// every governed observation and Receipt in the transcript, then append the
	// user-visible text as the exact Candidate under review.
	judgeMessages = append(judgeMessages, Message{Role: "assistant", Content: body})
	startedAt := time.Now().UTC()
	decision, judgeUsage, err := s.evaluateCandidate(ctx, task.TaskID, JudgeRequest{
		CandidateContext: state.JudgeContext, Messages: judgeMessages, TaskText: state.TaskText,
		SkillIDs: state.SkillIDs, ConfirmedTools: confirmedTools, MaxOutputTokens: s.loop.MaxOutputTokens,
		SoulGuidedResponse: true, Purpose: "progress",
	})
	state.Usage = mergeUsage(state.Usage, judgeUsage)
	if err != nil {
		return fmt.Errorf("evaluate progress message: %w", err)
	}
	if decision.Result != eval.Pass {
		s.logger.Info("progress message withheld by Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", latestModelTurnID(state), "result", decision.Result, "finding_count", len(decision.Findings))
		return nil
	}
	ref, err := s.content.Put(ctx, []byte(body), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "conversation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store progress artifact: %w", err)
	}
	commit, err := newCommit(task, "progress", ref)
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return err
	}
	progress := progressTrace{
		ID: commit.ArtifactID, ModelTurnID: latestModelTurnID(state), DeliveryID: commit.DeliveryID,
		Status: "queued", CreatedAt: startedAt,
	}
	state.Trace.Progress = append(state.Trace.Progress, progress)
	traceRef, err := s.storeTrace(ctx, task.ExecutionKey(), state.Trace)
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		return err
	}
	findingsRef, err := s.storeEvalFindings(ctx, commit.EvalID, decision.Findings)
	if err != nil {
		_ = s.content.Delete(context.Background(), ref)
		_ = s.content.Delete(context.Background(), traceRef)
		return err
	}
	commit.TraceRef = traceRef
	commit.Usage = judgeUsage
	commit.EvalResult = decision.Result
	commit.EvalFindings = append([]string(nil), decision.Findings...)
	commit.EvalFindingsRef = findingsRef
	commit.EvalTier = decision.Tier
	commit.EvalEvaluator = "llm_judge_progress"
	commit.TerminalStatus = "completed"
	commit.BasisInputSequence = state.InputSequence
	if state.ContextManifest.CurrentTask != nil && state.ContextManifest.CurrentTask.SourceRole == "user" {
		commit.BasisConversationSequence = state.ConversationSequence
	}
	created, err := s.repository.CommitProgress(ctx, ProgressCommit{Commit: commit, ModelTurnID: progress.ModelTurnID})
	if err != nil || !created {
		_ = s.content.Delete(context.Background(), ref)
		_ = s.content.Delete(context.Background(), traceRef)
		_ = s.content.Delete(context.Background(), findingsRef)
	}
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	state.LastProgressHash = digest
	s.logger.Info("progress message queued", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", progress.ModelTurnID, "delivery_id", commit.DeliveryID)
	return nil
}

func (s *Service) pauseForSubagent(ctx context.Context, task TaskContext, state loopState, request ModelRequest, modelToolIDs map[string]string, body, tier string) error {
	if state.PendingDeferred == nil || state.PendingDeferred.ID == "" || state.PendingDeferred.Kind != "subagent" || state.PendingDeferred.RoleID == "" || state.PendingDeferred.ProviderID == "" {
		return fmt.Errorf("unsupported deferred subagent continuation")
	}
	result, gateFindings := eval.Routine(body)
	if result != eval.Pass {
		return s.commitFailure(ctx, task, state.Usage, "subagent_progress_eval_failed", traceWithProviderTranscript(state.Trace, request))
	}
	artifactID, err := identifier.New()
	if err != nil {
		return err
	}
	evalID, err := identifier.New()
	if err != nil {
		return err
	}
	deliveryID, err := identifier.New()
	if err != nil {
		return err
	}
	artifactRef, err := s.content.Put(ctx, []byte(body), content.Metadata{
		MediaType: "text/plain; charset=utf-8", EncryptionDomain: "conversation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store delegation progress artifact: %w", err)
	}
	traceRef, err := s.storeTrace(ctx, task.ExecutionKey(), state.Trace)
	if err != nil {
		return err
	}
	continuationBody, err := json.Marshal(pendingContinuation{
		Request: request, ModelToolIDs: modelToolIDs, State: state,
	})
	if err != nil {
		return fmt.Errorf("encode delegation continuation: %w", err)
	}
	continuationRef, err := s.content.Put(ctx, continuationBody, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "runtime",
		PrivacyClass: "private", RetentionPolicy: "until_task_complete", ProvenanceRef: state.PendingDeferred.ID,
	})
	if err != nil {
		return fmt.Errorf("store delegation continuation: %w", err)
	}
	allFindings := append([]string(nil), gateFindings...)
	allFindings = append(allFindings, state.EvalFindings...)
	findingsRef, err := s.storeEvalFindings(ctx, evalID, allFindings)
	if err != nil {
		return err
	}
	return s.repository.PauseForSubagent(ctx, SubagentWaitCommit{
		TaskID: task.TaskID, RunID: task.RunID,
		DelegationID: state.PendingDeferred.ID, RoleID: state.PendingDeferred.RoleID, ProviderID: state.PendingDeferred.ProviderID,
		ArtifactID: artifactID, EvalID: evalID, DeliveryID: deliveryID,
		ArtifactRef: artifactRef, TraceRef: traceRef, ContinuationRef: continuationRef, Usage: state.Usage,
		EvalResult: result, EvalFindings: allFindings, EvalFindingsRef: findingsRef,
		EvalTier: tier, EvalEvaluator: "llm_judge",
	})
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Service) commitFailureAfterEffect(ctx context.Context, task TaskContext, usage Usage, cause string, trace runTrace) error {
	trace.FailureCause = cause
	trace.RuntimeStop = "post_effect_synthesis_failed"
	return s.commitFailure(ctx, task, usage, "post_effect_synthesis_failed", trace)
}

func (s *Service) pauseForApproval(ctx context.Context, task TaskContext, outcome tool.Outcome, continuation pendingContinuation) error {
	approvalID, err := identifier.New()
	if err != nil {
		return err
	}
	artifactID, err := identifier.New()
	if err != nil {
		return err
	}
	evalID, err := identifier.New()
	if err != nil {
		return err
	}
	deliveryID, err := identifier.New()
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(s.loop.ApprovalTTL)
	s.logger.Info("approval requested", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "approval_id", approvalID, "intent_id", outcome.Intent.ID, "tool_id", outcome.Intent.ToolID, "control", outcome.Control, "expires_at", expiresAt)
	payload, err := json.Marshal(map[string]any{
		"approval_id": approvalID,
		"control":     outcome.Control, "tool_id": outcome.Intent.ToolID,
		"effect": outcome.Intent.Effect, "target": outcome.Intent.Target,
		"parameters_hash": outcome.Intent.ParametersHash, "expires_at": expiresAt,
		"parameters": func() json.RawMessage {
			if len(continuation.PendingCalls) == 0 {
				return json.RawMessage(`{}`)
			}
			return continuation.PendingCalls[0].Arguments
		}(),
	})
	if err != nil {
		return err
	}
	result, findings := eval.Routine(string(payload))
	if result != eval.Pass {
		return fmt.Errorf("approval card failed deterministic schema gate")
	}
	findingsRef, err := s.storeEvalFindings(ctx, evalID, findings)
	if err != nil {
		return err
	}
	artifactRef, err := s.content.Put(ctx, payload, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "conversation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store approval artifact: %w", err)
	}
	continuationBody, err := json.Marshal(continuation)
	if err != nil {
		return fmt.Errorf("encode approval continuation: %w", err)
	}
	continuationRef, err := s.content.Put(ctx, continuationBody, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "runtime",
		PrivacyClass: "private", RetentionPolicy: "until_task_complete", ProvenanceRef: approvalID,
	})
	if err != nil {
		return fmt.Errorf("store approval continuation: %w", err)
	}
	return s.repository.PauseForApproval(ctx, ApprovalCommit{
		TaskID: task.TaskID, RunID: task.RunID,
		ApprovalID: approvalID, ArtifactID: artifactID, EvalID: evalID, DeliveryID: deliveryID,
		Intent: outcome.Intent, ArtifactRef: artifactRef, ContinuationRef: continuationRef,
		EvalResult: result, EvalFindings: findings, EvalFindingsRef: findingsRef,
		EvalTier: "system", EvalEvaluator: "approval_schema_gate", ExpiresAt: expiresAt,
	})
}

func (s *Service) commitFailure(ctx context.Context, task TaskContext, usage Usage, code string, traces ...runTrace) error {
	trace := runTrace{ModelTurns: []modelTurnTrace{}, ToolCalls: []toolResultTrace{}, RuntimeStop: code}
	if len(traces) > 0 {
		trace = traces[0]
		trace.RuntimeStop = code
	}
	s.logger.Error("run failed", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "error_code", code, "failure_cause", trace.FailureCause)
	disclosure := map[string]any{
		"code": code, "task_id": task.TaskID, "run_id": task.RunID, "execution_id": task.ExecutionKey(),
	}
	if trace.FailureCause != "" {
		disclosure["cause"] = trace.FailureCause
	}
	body, err := json.Marshal(disclosure)
	if err != nil {
		return err
	}
	result, findings := eval.Routine(string(body))
	if result != eval.Pass {
		return fmt.Errorf("runtime error card failed deterministic schema gate")
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "conversation",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: task.ExecutionKey(),
	})
	if err != nil {
		return fmt.Errorf("store failure disclosure: %w", err)
	}
	commit, err := newCommit(task, "runtime_error", ref)
	if err != nil {
		return err
	}
	traceRef, err := s.storeTrace(ctx, task.ExecutionKey(), trace)
	if err != nil {
		return err
	}
	commit.TraceRef = traceRef
	commit.Usage = usage
	commit.EvalResult = result
	commit.EvalFindings = findings
	commit.EvalTier = "system"
	commit.EvalEvaluator = "runtime_schema_gate"
	commit.TerminalStatus = "failed"
	commit.FailureCode = code
	commit.EvalFindingsRef, err = s.storeEvalFindings(ctx, commit.EvalID, commit.EvalFindings)
	if err != nil {
		return err
	}
	return s.repository.CommitArtifact(ctx, commit)
}

func (s *Service) storeEvalFindings(ctx context.Context, evalID string, findings []string) (content.Ref, error) {
	body, err := json.Marshal(findings)
	if err != nil {
		return content.Ref{}, err
	}
	ref, err := s.content.Put(ctx, body, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "eval", PrivacyClass: "private",
		RetentionPolicy: "user_owned", ProvenanceRef: evalID,
	})
	if err != nil {
		return content.Ref{}, fmt.Errorf("store Eval findings: %w", err)
	}
	return ref, nil
}

func (s *Service) storeTrace(ctx context.Context, invocationID string, trace any) (content.Ref, error) {
	encoded, err := json.Marshal(trace)
	if err != nil {
		return content.Ref{}, fmt.Errorf("encode model/runtime trace: %w", err)
	}
	ref, err := s.content.Put(ctx, encoded, content.Metadata{
		MediaType: "application/json", EncryptionDomain: "runtime",
		PrivacyClass: "private", RetentionPolicy: "user_owned", ProvenanceRef: invocationID,
	})
	if err != nil {
		return content.Ref{}, fmt.Errorf("store model/runtime trace: %w", err)
	}
	return ref, nil
}

func newCommit(task TaskContext, kind string, ref content.Ref) (Commit, error) {
	artifactID, err := identifier.New()
	if err != nil {
		return Commit{}, err
	}
	evalID, err := identifier.New()
	if err != nil {
		return Commit{}, err
	}
	deliveryID, err := identifier.New()
	if err != nil {
		return Commit{}, err
	}
	return Commit{
		TaskID: task.TaskID, RunID: task.RunID,
		ArtifactID: artifactID, EvalID: evalID, DeliveryID: deliveryID,
		ArtifactKind: kind, ArtifactRef: ref,
	}, nil
}
