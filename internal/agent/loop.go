package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/budget"
	"github.com/z-chenhao/eri/internal/eval"
	"github.com/z-chenhao/eri/internal/execution"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/tool"
)

// loopDriver is the single model -> tool -> governed observation -> model
// mechanism used by both primary Eri and restricted asynchronous agents. A
// profile supplies persistence, capability scope and its terminal sink; the
// cognitive loop itself is shared.
type loopDriver struct {
	model  Model
	tools  ToolGateway
	loop   LoopConfig
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	}
	compact    func(context.Context, TaskContext, ModelRequest, ModelCapabilities, *loopState) (ModelRequest, Usage, error)
	refresh    func(context.Context, TaskContext, *ModelRequest, *loopState) (bool, error)
	cancel     func(context.Context, TaskContext, ModelRequest, *loopState) (bool, error)
	checkpoint func(context.Context, TaskContext, string, pendingContinuation) error
	progress   func(context.Context, TaskContext, ModelRequest, *loopState, string) error
	execute    func(context.Context, TaskContext, *ModelRequest, []ToolCall, map[string]string, *loopState, *tool.Grant) (bool, error)
	candidate  func(context.Context, TaskContext, ModelRequest, map[string]string, *loopState) (ModelRequest, bool, error)
	fail       func(context.Context, TaskContext, ModelRequest, Usage, string, runTrace) error
}

func (s *Service) continueLoop(ctx context.Context, task TaskContext, request ModelRequest, modelToolIDs map[string]string, state loopState) error {
	driver := loopDriver{
		model: s.model, tools: s.tools, loop: s.loop, logger: s.logger,
		compact: s.compactLoopContext, refresh: s.refreshTaskInputs, cancel: s.cancelIfRequested, checkpoint: s.saveAgentCheckpoint,
		progress: s.commitIntermediateProgress,
		execute:  s.executeCalls, candidate: s.evaluateAndCommitCandidate,
		fail: func(ctx context.Context, task TaskContext, request ModelRequest, usage Usage, code string, trace runTrace) error {
			trace = traceWithProviderTranscript(trace, request)
			if trace.RuntimeStop == "" {
				trace.RuntimeStop = code
			}
			confirmedEffect := false
			for _, call := range trace.ToolCalls {
				confirmedEffect = confirmedEffect || call.Status == string(tool.IntentConfirmed)
			}
			if trace.RuntimeStop != "user_canceled" && confirmedEffect && code != "post_effect_synthesis_failed" {
				return s.commitFailureAfterEffect(ctx, task, usage, code, trace)
			}
			return s.commitFailure(ctx, task, usage, code, trace)
		},
	}
	return runAgentLoop(ctx, driver, task, request, modelToolIDs, state)
}

func runAgentLoop(ctx context.Context, driver loopDriver, task TaskContext, request ModelRequest, modelToolIDs map[string]string, state loopState) error {
	for {
		if driver.refresh != nil {
			changed, err := driver.refresh(ctx, task, &request, &state)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "task_input_unavailable", state.Trace)
			}
			if changed {
				state.NextTurnTrigger = "user_input"
			}
		}
		if state.Capabilities.ContextTokens == 0 {
			capabilities, err := capabilitiesFor(ctx, driver.model)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "provider_capabilities_unavailable", state.Trace)
			}
			state.Capabilities = capabilities
		}
		compacted, compactUsage, err := driver.compact(ctx, task, request, state.Capabilities, &state)
		state.Usage = mergeUsage(state.Usage, compactUsage)
		if err != nil {
			driver.logger.Warn("in-run context compaction failed", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			state.Trace.RuntimeStop = "context_compaction_failed"
			return driver.fail(ctx, task, request, state.Usage, "context_compaction_failed", state.Trace)
		}
		request = compacted
		canceled, err := driver.cancel(ctx, task, request, &state)
		if err != nil || canceled {
			return err
		}
		if err := validateModelTranscript(request.Messages); err != nil {
			state.Trace.RuntimeStop = "invalid_model_transcript"
			driver.logger.Warn("model request withheld because its tool protocol is invalid", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "error_code", "invalid_model_transcript", "error", err.Error())
			return driver.fail(ctx, task, request, state.Usage, "invalid_model_transcript", state.Trace)
		}
		ensureActiveTurn(task, &state)
		turnOrdinal := state.ActiveTurn.Ordinal
		appendActiveTurnCheckpoint(&state, "ready_for_model")
		if err := driver.checkpoint(ctx, task, "ready_for_model", pendingContinuation{
			Request: request, ModelToolIDs: modelToolIDs, State: state,
		}); err != nil {
			return fmt.Errorf("save ready-for-model checkpoint: %w", err)
		}
		reservationID := ""
		if driver.loop.ExternalModel && driver.loop.Budget != nil {
			var err error
			reservationID, err = driver.loop.Budget.Reserve(ctx, task.TaskID, estimateModelTokens(request))
			if err != nil {
				finishActiveTurn(&state, nil, "blocked")
				state.Trace.RuntimeStop = "model_budget_unavailable"
				if errors.Is(err, budget.ErrExhausted) {
					state.Trace.RuntimeStop = "model_budget_exhausted"
					return driver.fail(ctx, task, request, state.Usage, "model_budget_exhausted", state.Trace)
				}
				return driver.fail(ctx, task, request, state.Usage, "model_budget_unavailable", state.Trace)
			}
		}
		modelStarted := time.Now()
		driver.logger.Info("model call started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "provider", driver.loop.ModelTarget)
		// Keep the declared tool surface while a deferred result is pending. The
		// transcript still contains the assistant Tool Call and its Tool result;
		// stripping definitions here makes that valid protocol frame unacceptable
		// to providers such as DeepSeek. The Loop rejects any new call while the
		// deferred result is pending and asks the model only for progress text.
		dispatchRequest := request
		state.ActiveTurn.Request = summarizeModelRequest(dispatchRequest)
		response, callErr := driver.model.Complete(ctx, dispatchRequest)
		if reservationID != "" {
			actual := response.Usage.InputTokens + response.Usage.OutputTokens
			if err := driver.loop.Budget.Settle(ctx, reservationID, actual, callErr == nil); err != nil {
				state.Trace.RuntimeStop = "budget_accounting_failed"
				return driver.fail(ctx, task, request, state.Usage, "budget_accounting_failed", state.Trace)
			}
		}
		state.TurnsUsed++
		state.Usage = mergeUsage(state.Usage, recordModelCall(response.Usage))
		if callErr != nil {
			finishActiveTurn(&state, &response, "failed")
		} else {
			response.Message.Role = "assistant"
			finishActiveTurn(&state, &response, "succeeded")
		}
		driver.logger.Info("model call finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "duration_ms", time.Since(modelStarted).Milliseconds(), "input_tokens", response.Usage.InputTokens, "output_tokens", response.Usage.OutputTokens, "cache_hit_tokens", response.Usage.CacheHitTokens, "cache_miss_tokens", response.Usage.CacheMissTokens, "error_code", observability.ErrorCode(callErr), "error", observability.SafeError(callErr))
		canceled, cancelErr := driver.cancel(ctx, task, request, &state)
		if cancelErr != nil || canceled {
			return cancelErr
		}
		if callErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			state.Trace.RuntimeStop = "model_unavailable"
			return driver.fail(ctx, task, request, state.Usage, "model_unavailable", state.Trace)
		}
		if driver.refresh != nil {
			changed, err := driver.refresh(ctx, task, &request, &state)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "task_input_unavailable", state.Trace)
			}
			if changed {
				markLatestTurnSuperseded(&state)
				driver.logger.Info("model result superseded by newer user input", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "input_sequence", state.InputSequence)
				state.NextTurnTrigger = "user_input"
				if err := driver.checkpoint(ctx, task, "ready_for_model", pendingContinuation{
					Request: request, ModelToolIDs: modelToolIDs, State: state,
				}); err != nil {
					return fmt.Errorf("save superseded-turn checkpoint: %w", err)
				}
				continue
			}
		}
		if err := validateAssistantMessage(response.Message); err != nil {
			state.Trace.ModelTurns[len(state.Trace.ModelTurns)-1].Status = "failed"
			state.Trace.RuntimeStop = "invalid_model_response"
			return driver.fail(ctx, task, request, state.Usage, "invalid_model_response", state.Trace)
		}
		assistantIndex := len(request.Messages)
		request.Messages = append(request.Messages, response.Message)
		if driver.refresh != nil {
			changed, err := driver.refresh(ctx, task, &request, &state)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "task_input_unavailable", state.Trace)
			}
			if changed {
				removeMessageAt(&request, assistantIndex)
				markLatestTurnSuperseded(&state)
				driver.logger.Info("model result superseded by newer user input", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "input_sequence", state.InputSequence)
				state.NextTurnTrigger = "user_input"
				continue
			}
		}
		if len(response.Message.ToolCalls) == 0 {
			appendCompletedTurnCheckpoint(&state, "candidate_received")
			if err := driver.checkpoint(ctx, task, "candidate_received", pendingContinuation{
				Request: request, ModelToolIDs: modelToolIDs, State: state,
			}); err != nil {
				return fmt.Errorf("save candidate checkpoint: %w", err)
			}
			next, again, err := driver.candidate(ctx, task, request, modelToolIDs, &state)
			if err != nil || !again {
				return err
			}
			request = next
			continue
		}
		if state.PendingDeferred != nil {
			for _, call := range response.Message.ToolCalls {
				request.Messages = append(request.Messages, toolErrorMessage(call, "the private subagent is already running; provide a brief progress update and wait for Runtime to resume this task"))
				state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
					ModelTurnID: latestModelTurnID(&state), ToolCallID: call.ID, Status: "rejected_deferred_pending",
				})
			}
			state.NextTurnTrigger = "subagent_progress_required"
			continue
		}
		if driver.tools == nil {
			state.Trace.RuntimeStop = "tool_gateway_unavailable"
			return driver.fail(ctx, task, request, state.Usage, "tool_gateway_unavailable", state.Trace)
		}
		if response.FinishReason == "length" {
			for _, call := range response.Message.ToolCalls {
				request.Messages = append(request.Messages, toolErrorMessage(call, "tool call was not executed because its arguments may have been truncated by the model output limit"))
				state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{ModelTurnID: latestModelTurnID(&state), ToolCallID: call.ID, Status: "rejected_truncated"})
			}
			state.NextTurnTrigger = "tool_observations"
			continue
		}
		appendCompletedTurnCheckpoint(&state, "model_received")
		if err := driver.checkpoint(ctx, task, "model_received", pendingContinuation{
			Request: request, ModelToolIDs: modelToolIDs,
			PendingCalls: append([]ToolCall(nil), response.Message.ToolCalls...), State: state,
		}); err != nil {
			return fmt.Errorf("save model-response checkpoint: %w", err)
		}
		if strings.TrimSpace(response.Message.Content) != "" {
			if err := driver.checkpoint(ctx, task, "model_received", pendingContinuation{
				Request: request, ModelToolIDs: modelToolIDs,
				PendingCalls: append([]ToolCall(nil), response.Message.ToolCalls...), State: state,
			}); err != nil {
				return fmt.Errorf("save progress-aware model checkpoint: %w", err)
			}
		}
		if driver.refresh != nil {
			changed, err := driver.refresh(ctx, task, &request, &state)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "task_input_unavailable", state.Trace)
			}
			if changed {
				if _, err := closeInterruptedToolFrame(&request, assistantIndex, modelToolIDs, &state); err != nil {
					return driver.fail(ctx, task, request, state.Usage, "invalid_model_transcript", state.Trace)
				}
				driver.logger.Info("tool turn superseded before effect dispatch", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "input_sequence", state.InputSequence)
				state.NextTurnTrigger = "user_input"
				if err := driver.checkpoint(ctx, task, "ready_for_model", pendingContinuation{
					Request: request, ModelToolIDs: modelToolIDs, State: state,
				}); err != nil {
					return fmt.Errorf("save superseded-tool checkpoint: %w", err)
				}
				continue
			}
		}
		observationStart := len(request.Messages)
		paused, err := driver.execute(ctx, task, &request, response.Message.ToolCalls, modelToolIDs, &state, nil)
		if errors.Is(err, ErrStaleTaskInput) {
			retained, closeErr := closeInterruptedToolFrame(&request, assistantIndex, modelToolIDs, &state)
			if closeErr != nil {
				return driver.fail(ctx, task, request, state.Usage, "invalid_model_transcript", state.Trace)
			}
			driver.logger.Info("tool turn interrupted at effect fence", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "input_sequence", state.InputSequence, "protocol_frame_retained", retained)
			state.NextTurnTrigger = "user_input"
			if err := driver.checkpoint(ctx, task, "ready_for_model", pendingContinuation{
				Request: request, ModelToolIDs: modelToolIDs, State: state,
			}); err != nil {
				return fmt.Errorf("save interrupted-tool checkpoint: %w", err)
			}
			continue
		}
		if err != nil || paused {
			return err
		}
		// Progress paired with a Tool Call is not delivered until Runtime knows
		// what actually happened. A deferred result gets one separately synthesized
		// progress artifact at the atomic wait boundary, avoiding duplicate or
		// pre-effect claims.
		if state.PendingDeferred == nil && strings.TrimSpace(response.Message.Content) != "" && driver.progress != nil {
			if err := driver.progress(ctx, task, request, &state, response.Message.Content); err != nil {
				driver.logger.Warn("progress message was withheld", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "turn", turnOrdinal, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
		stagnant := updateLoopProgress(&state, response.Message.ToolCalls, request.Messages[observationStart:])
		if stagnant >= 4 {
			if state.ConfirmedEffects > 0 && !state.SynthesisOnly {
				request.Tools = nil
				request.Messages = append(request.Messages, Message{Role: "system", Content: "The governed no-progress check found repeated tool work. Stop calling tools. Use only confirmed observations already in this transcript to give the user the best supported result now, state any material limitation plainly, and do not claim missing work succeeded."})
				state.SynthesisOnly = true
				state.StagnantTurns = 0
				state.ProgressDigest = ""
				state.NextTurnTrigger = "no_progress_synthesis"
				continue
			}
			state.Trace.RuntimeStop = "agent_loop_no_progress"
			return driver.fail(ctx, task, request, state.Usage, "agent_loop_no_progress", state.Trace)
		}
		if stagnant == 2 {
			request.Messages = append(request.Messages, Message{Role: "system", Content: "The last native tool action and its observation repeated without new information. Reflect on the unmet success criteria and change strategy, tool, query, or scope; do not repeat the same call again."})
			state.NextTurnTrigger = "strategy_reflection"
		} else {
			state.NextTurnTrigger = "tool_observations"
		}
	}
}

func markLatestTurnSuperseded(state *loopState) {
	if len(state.Trace.ModelTurns) == 0 {
		return
	}
	turn := &state.Trace.ModelTurns[len(state.Trace.ModelTurns)-1]
	turn.Status = "superseded"
	if !contains(turn.Checkpoints, "newer_user_input") {
		turn.Checkpoints = append(turn.Checkpoints, "newer_user_input")
	}
}

func removeMessageAt(request *ModelRequest, index int) {
	if index < 0 || index >= len(request.Messages) {
		return
	}
	request.Messages = append(request.Messages[:index], request.Messages[index+1:]...)
}

// closeInterruptedToolFrame preserves the provider protocol when newer user
// input arrives between sibling tool calls. Before any tool observation exists
// the whole stale assistant turn can be removed. Once a result exists, the
// assistant message and completed results are immutable protocol history; each
// unstarted sibling receives a governed skipped observation before the newer
// user message is admitted.
func closeInterruptedToolFrame(request *ModelRequest, assistantIndex int, modelToolIDs map[string]string, state *loopState) (bool, error) {
	if assistantIndex < 0 || assistantIndex >= len(request.Messages) {
		return false, fmt.Errorf("interrupted tool frame has no assistant message")
	}
	assistant := request.Messages[assistantIndex]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) == 0 {
		return false, fmt.Errorf("interrupted tool frame does not start with assistant tool calls")
	}
	declared := make(map[string]ToolCall, len(assistant.ToolCalls))
	for _, call := range assistant.ToolCalls {
		declared[call.ID] = call
	}
	results := make(map[string]Message, len(assistant.ToolCalls))
	trailing := make([]Message, 0, len(request.Messages)-assistantIndex-1)
	for _, message := range request.Messages[assistantIndex+1:] {
		if message.Role == "tool" {
			if _, ok := declared[message.ToolCallID]; ok {
				if _, duplicate := results[message.ToolCallID]; duplicate {
					return false, fmt.Errorf("interrupted tool frame has duplicate result %q", message.ToolCallID)
				}
				results[message.ToolCallID] = message
				continue
			}
		}
		trailing = append(trailing, message)
	}
	if len(results) == 0 {
		rebuilt := append([]Message(nil), request.Messages[:assistantIndex]...)
		request.Messages = append(rebuilt, trailing...)
		markLatestTurnSuperseded(state)
		return false, nil
	}

	rebuilt := append([]Message(nil), request.Messages[:assistantIndex+1]...)
	turnID := latestModelTurnID(state)
	for _, call := range assistant.ToolCalls {
		if result, ok := results[call.ID]; ok {
			rebuilt = append(rebuilt, result)
			continue
		}
		rebuilt = append(rebuilt, supersededToolMessage(call))
		if !hasToolCallTrace(state.Trace.ToolCalls, turnID, call.ID) {
			state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
				ModelTurnID: turnID, ToolCallID: call.ID, ToolID: modelToolIDs[call.Name], Status: "superseded_before_execution",
			})
		}
	}
	request.Messages = append(rebuilt, trailing...)
	appendCompletedTurnCheckpoint(state, "newer_user_input")
	return true, nil
}

func hasToolCallTrace(calls []toolResultTrace, modelTurnID, toolCallID string) bool {
	for _, call := range calls {
		if call.ModelTurnID == modelTurnID && call.ToolCallID == toolCallID {
			return true
		}
	}
	return false
}

func supersededToolMessage(call ToolCall) Message {
	body, _ := json.Marshal(map[string]any{
		"success": false,
		"status":  "superseded_before_execution",
		"error":   "the tool call was not executed because a newer user message arrived before dispatch",
	})
	return Message{Role: "tool", ToolCallID: call.ID, Content: string(body)}
}

func latestToolFrameAssistantIndex(messages []Message) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "assistant" && len(messages[index].ToolCalls) > 0 {
			return index
		}
	}
	return -1
}

func ensureActiveTurn(task TaskContext, state *loopState) {
	if state.ActiveTurn != nil {
		return
	}
	ordinal := state.TurnsUsed + 1
	trigger := state.NextTurnTrigger
	if trigger == "" {
		trigger = "initial_request"
	}
	state.ActiveTurn = &activeTurnTrace{
		ID: task.InvocationID + ":turn:" + strconv.Itoa(ordinal), Ordinal: ordinal,
		Trigger: trigger, StartedAt: time.Now().UTC(), Checkpoints: []string{}, InputSequence: state.InputSequence,
	}
	state.NextTurnTrigger = ""
}

func appendActiveTurnCheckpoint(state *loopState, phase string) {
	if state.ActiveTurn == nil || contains(state.ActiveTurn.Checkpoints, phase) {
		return
	}
	state.ActiveTurn.Checkpoints = append(state.ActiveTurn.Checkpoints, phase)
}

func appendCompletedTurnCheckpoint(state *loopState, phase string) {
	if len(state.Trace.ModelTurns) == 0 {
		return
	}
	turn := &state.Trace.ModelTurns[len(state.Trace.ModelTurns)-1]
	if !contains(turn.Checkpoints, phase) {
		turn.Checkpoints = append(turn.Checkpoints, phase)
	}
}

func finishActiveTurn(state *loopState, response *ModelResponse, status string) {
	if state.ActiveTurn == nil {
		return
	}
	active := state.ActiveTurn
	turn := modelTurnTrace{
		ID: active.ID, Ordinal: active.Ordinal, Trigger: active.Trigger, Status: status,
		StartedAt: active.StartedAt, EndedAt: time.Now().UTC(), Checkpoints: append([]string(nil), active.Checkpoints...), InputSequence: active.InputSequence, Request: active.Request,
	}
	if response != nil {
		turn.FinishReason = response.FinishReason
		turn.Message = traceSafeMessage(response.Message)
		turn.Usage = response.Usage
	}
	state.Trace.ModelTurns = append(state.Trace.ModelTurns, turn)
	state.ActiveTurn = nil
}

func summarizeModelRequest(request ModelRequest) modelRequestTrace {
	roles := make(map[string]int)
	for _, message := range request.Messages {
		roles[message.Role]++
	}
	tools := make([]string, 0, len(request.Tools))
	for _, definition := range request.Tools {
		tools = append(tools, definition.Name)
	}
	return modelRequestTrace{
		MessageCount: len(request.Messages), MessageRoles: roles, ToolNames: tools,
		MaxOutputTokens: request.MaxOutputTokens, EstimatedInputTokens: estimateModelInputTokens(request),
	}
}

func latestModelTurnID(state *loopState) string {
	if len(state.Trace.ModelTurns) == 0 {
		return ""
	}
	return state.Trace.ModelTurns[len(state.Trace.ModelTurns)-1].ID
}

func updateLoopProgress(state *loopState, calls []ToolCall, observations []Message) int {
	payload := struct {
		Calls        []map[string]any `json:"calls"`
		Observations []string         `json:"observations"`
	}{Calls: make([]map[string]any, 0, len(calls)), Observations: make([]string, 0, len(observations))}
	for _, call := range calls {
		payload.Calls = append(payload.Calls, map[string]any{"name": call.Name, "arguments": json.RawMessage(call.Arguments)})
	}
	for _, observation := range observations {
		payload.Observations = append(payload.Observations, observation.Content)
	}
	encoded, _ := json.Marshal(payload)
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	if digest == state.ProgressDigest {
		state.StagnantTurns++
	} else {
		state.ProgressDigest = digest
		state.StagnantTurns = 1
	}
	return state.StagnantTurns
}

// evaluateAndCommitCandidate is the durable boundary after a model has produced
// a user-facing candidate. Recovery can replay the Judge and final transaction,
// while CommitArtifact remains the idempotent delivery boundary.
func (s *Service) evaluateAndCommitCandidate(
	ctx context.Context,
	task TaskContext,
	request ModelRequest,
	modelToolIDs map[string]string,
	state *loopState,
) (ModelRequest, bool, error) {
	candidateIndex := len(request.Messages) - 1
	findingsStart := len(state.EvalFindings)
	changed, err := s.refreshTaskInputs(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if changed {
		removeMessageAt(&request, candidateIndex)
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded before Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "input_sequence", state.InputSequence)
		state.NextTurnTrigger = "user_input"
		return request, true, nil
	}
	body := strings.TrimSpace(request.Messages[len(request.Messages)-1].Content)
	evaluationStartedAt := time.Now().UTC()
	modelTurnID := latestModelTurnID(state)
	evaluationAttempt := state.EvalAttempts + 1
	confirmedTools := make([]string, 0)
	for _, call := range state.Trace.ToolCalls {
		if call.Status == string(tool.IntentConfirmed) {
			confirmedTools = append(confirmedTools, call.ToolID)
		}
	}
	s.logger.Info("evaluation started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "attempt", evaluationAttempt)
	decision, judgeUsage, judgeErr := s.evaluateCandidate(ctx, task.TaskID, JudgeRequest{
		CandidateContext: state.JudgeContext, Messages: request.Messages, TaskText: state.TaskText,
		SkillIDs: state.SkillIDs, ConfirmedTools: confirmedTools, MaxOutputTokens: s.loop.MaxOutputTokens,
		SoulGuidedResponse: true,
	})
	state.Usage = mergeUsage(state.Usage, judgeUsage)
	s.logger.Info("evaluation finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "attempt", evaluationAttempt, "result", decision.Result, "tier", decision.Tier, "duration_ms", time.Since(evaluationStartedAt).Milliseconds(), "input_tokens", judgeUsage.InputTokens, "output_tokens", judgeUsage.OutputTokens, "cache_hit_tokens", judgeUsage.CacheHitTokens, "cache_miss_tokens", judgeUsage.CacheMissTokens, "error_code", observability.ErrorCode(judgeErr), "error", observability.SafeError(judgeErr))
	if judgeErr != nil {
		state.Trace.RuntimeStop = "llm_judge_unavailable"
		trace := traceWithProviderTranscript(state.Trace, request)
		if state.ConfirmedEffects > 0 {
			return request, false, s.commitFailureAfterEffect(ctx, task, state.Usage, "llm_judge_unavailable", trace)
		}
		return request, false, s.commitFailure(ctx, task, state.Usage, "llm_judge_unavailable", trace)
	}
	changed, err = s.refreshTaskInputs(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if changed {
		removeMessageAt(&request, candidateIndex)
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded during Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "input_sequence", state.InputSequence)
		state.NextTurnTrigger = "user_input"
		return request, true, nil
	}
	state.EvalAttempts++
	state.Trace.Evaluations = append(state.Trace.Evaluations, evaluationTrace{
		ID: modelTurnID + ":eval:" + strconv.Itoa(evaluationAttempt), ModelTurnID: modelTurnID, Attempt: evaluationAttempt,
		StartedAt: evaluationStartedAt, EndedAt: time.Now().UTC(),
		Result: decision.Result, Tier: decision.Tier, Findings: decision.Findings, Usage: judgeUsage,
	})
	if s.evolution != nil {
		if err := s.evolution.Observe(ctx, EvolutionSignal{
			TaskID: task.TaskID, ReleaseID: state.ContextManifest.EvolutionReleaseID, Result: decision.Result, Tier: decision.Tier,
			Findings: append([]string(nil), decision.Findings...),
		}); err != nil {
			return request, false, fmt.Errorf("record evolution signal: %w", err)
		}
	}
	if decision.Result == eval.Repair || decision.Result == eval.Escalate {
		state.EvalFindings = append(state.EvalFindings, decision.Findings...)
		state.Trace.ModelTurns[len(state.Trace.ModelTurns)-1].Message.Content = "[candidate withheld for repair]"
		if state.EvalAttempts >= s.loop.MaxEvalAttempts {
			state.Trace.RuntimeStop = "llm_judge_repair_limit"
			return request, false, s.commitFailure(ctx, task, state.Usage, "llm_judge_repair_limit", traceWithProviderTranscript(state.Trace, request))
		}
		if decision.Result == eval.Escalate {
			state.NextTurnTrigger = "eval_escalation"
		} else {
			state.NextTurnTrigger = "eval_repair"
		}
		return evalRepairRequest(request, decision), true, nil
	}
	if decision.Result != eval.Pass {
		return request, false, s.commitFailure(ctx, task, state.Usage, "task_eval_held", traceWithProviderTranscript(state.Trace, request))
	}
	state.EvalFindings = append(state.EvalFindings, decision.Findings...)
	if state.PendingDeferred != nil {
		return request, false, s.pauseForSubagent(ctx, task, *state, request, modelToolIDs, body, decision.Tier)
	}
	err = s.commitEvaluatedReply(ctx, task, traceWithProviderTranscript(state.Trace, request), state.Usage, body, "text", decision.Tier, state.EvalFindings, state.Attachments, state.InputSequence)
	if errors.Is(err, ErrStaleTaskInput) {
		changed, refreshErr := s.refreshTaskInputs(ctx, task, &request, state)
		if refreshErr != nil {
			return request, false, refreshErr
		}
		if changed {
			if state.EvalAttempts > 0 {
				state.EvalAttempts--
			}
			state.EvalFindings = state.EvalFindings[:findingsStart]
			removeMessageAt(&request, candidateIndex)
			markLatestTurnSuperseded(state)
			s.logger.Info("candidate superseded at durable commit fence", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "input_sequence", state.InputSequence)
			state.NextTurnTrigger = "user_input"
			return request, true, nil
		}
	}
	return request, false, err
}

func findLoopCut(messages []Message, keepTokens int) int {
	used := 0
	for index := len(messages) - 1; index > 0; index-- {
		used += estimateMessageTokens(messages[index])
		if used < keepTokens {
			continue
		}
		message := messages[index]
		if message.Role == "user" || message.Role == "system" || (message.Role == "assistant" && len(message.ToolCalls) > 0) {
			return index
		}
	}
	return -1
}

func splitPinnedRunContext(messages []Message) (ordinary, pinned []Message) {
	ordinary = make([]Message, 0, len(messages))
	pinned = make([]Message, 0, 6)
	for _, message := range messages {
		if isPinnedRunContext(message) {
			pinned = append(pinned, message)
			continue
		}
		ordinary = append(ordinary, message)
	}
	return ordinary, pinned
}

func (s *Service) compactLoopContext(
	ctx context.Context,
	task TaskContext,
	request ModelRequest,
	capabilities ModelCapabilities,
	state *loopState,
) (ModelRequest, Usage, error) {
	before := estimateModelInputTokens(request)
	limit := contextInputLimit(capabilities, request.MaxOutputTokens)
	if before <= limit {
		return request, Usage{}, nil
	}
	ordinary, pinned := splitPinnedRunContext(request.Messages)
	if len(ordinary) == 0 {
		return request, Usage{}, fmt.Errorf("context exceeds %d tokens with only pinned active-task context", limit)
	}
	keepTokens := defaultRecentTokens
	if keepTokens > limit/2 {
		keepTokens = limit / 2
	}
	cut := findLoopCut(ordinary, keepTokens)
	if cut <= 0 {
		cut = len(ordinary)
	}
	summary, usage, err := s.summarizeContext(ctx, task.TaskID, ordinary[:cut], capabilities)
	if err != nil {
		return request, usage, err
	}
	checkpoint := Message{
		Role:    "system",
		Content: "Eri in-run context checkpoint. It summarizes prior native model/tool turns; continue the same task from it.\n\n" + summary,
	}
	request.Messages = append([]Message{checkpoint}, ordinary[cut:]...)
	request.Messages = append(request.Messages, pinned...)
	after := estimateModelInputTokens(request)
	if after > limit && len(request.Messages) > 1 {
		ordinary, pinned = splitPinnedRunContext(request.Messages)
		if len(ordinary) == 0 {
			return request, usage, fmt.Errorf("context exceeds %d tokens with only pinned active-task context", limit)
		}
		summary, nextUsage, err := s.summarizeContext(ctx, task.TaskID, ordinary, capabilities)
		usage = mergeUsage(usage, nextUsage)
		if err != nil {
			return request, usage, err
		}
		request.Messages = []Message{{Role: "system", Content: "Eri in-run context checkpoint.\n\n" + summary}}
		request.Messages = append(request.Messages, pinned...)
		after = estimateModelInputTokens(request)
	}
	if after > limit {
		return request, usage, fmt.Errorf("in-run context remains over limit: %d > %d", after, limit)
	}
	state.ContextManifest.RuntimeCompactions = append(state.ContextManifest.RuntimeCompactions, execution.RuntimeCompaction{
		TokensBefore: before, TokensAfter: after, SummarizedMessages: cut,
	})
	state.ContextManifest.EstimatedInputTokens = after
	encoded, err := json.Marshal(state.ContextManifest)
	if err != nil {
		return request, usage, err
	}
	if err := s.repository.UpdateInvocationContext(ctx, task.InvocationID, string(encoded)); err != nil {
		return request, usage, err
	}
	s.logger.Info("in-run context compacted", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "summarized_messages", cut, "tokens_before", before, "tokens_after", after, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "cache_hit_tokens", usage.CacheHitTokens, "cache_miss_tokens", usage.CacheMissTokens)
	return request, usage, nil
}

func evalRepairInstruction(decision eval.Decision) string {
	action := "Repair only these failed criteria using the remaining Agent Loop budget; preserve verified work and use tools when evidence is missing"
	if decision.Result == eval.Escalate {
		action = "The user has not replied to the withheld candidate, and tools cannot supply the missing user intent. Do not infer or invent their answer. Your entire next candidate must be exactly one focused user-facing question or decision request, with no preamble, answer, bullets, questionnaire, tool call, or claim that the original task is complete"
	}
	return "Eri pre-delivery LLM Judge withheld the candidate. " + action + ":\n- " + strings.Join(decision.Findings, "\n- ")
}

func evalRepairRequest(request ModelRequest, decision eval.Decision) ModelRequest {
	request.Messages = append(request.Messages, Message{Role: "system", Content: evalRepairInstruction(decision)})
	if decision.Result == eval.Escalate {
		// The Judge has established that only the user can supply the missing
		// input. Tools cannot resolve that condition and exposing them here lets
		// the model incorrectly continue as if the user had answered.
		request.Tools = nil
	}
	return request
}

func (s *Service) evaluateCandidate(ctx context.Context, taskID string, request JudgeRequest) (eval.Decision, Usage, error) {
	if s.judge == nil {
		return eval.Decision{}, Usage{}, fmt.Errorf("LLM Judge is unavailable")
	}
	reservationID := ""
	if s.loop.ExternalModel && s.loop.Budget != nil {
		var err error
		reservationID, err = s.loop.Budget.Reserve(ctx, taskID, estimateJudgeTokens(request))
		if err != nil {
			return eval.Decision{}, Usage{}, err
		}
	}
	decision, usage, err := s.judge.Evaluate(ctx, request)
	usage = recordModelCall(usage)
	if reservationID != "" {
		actual := usage.InputTokens + usage.OutputTokens
		if settleErr := s.loop.Budget.Settle(ctx, reservationID, actual, err == nil); settleErr != nil {
			return eval.Decision{}, usage, settleErr
		}
	}
	return decision, usage, err
}

func estimateJudgeTokens(request JudgeRequest) int {
	return estimateSerializedTokens(request, 512)
}

func traceSafeMessage(message Message) Message {
	copy := message
	copy.ReasoningContent = ""
	copy.Images = nil
	copy.ToolCalls = make([]ToolCall, len(message.ToolCalls))
	for index, call := range message.ToolCalls {
		copy.ToolCalls[index] = ToolCall{ID: call.ID, Name: call.Name}
	}
	return copy
}

func traceWithProviderTranscript(trace runTrace, request ModelRequest) runTrace {
	transcript := snapshotModelRequest(request)
	trace.ProviderTranscript = &transcript
	return trace
}

func estimateModelTokens(request ModelRequest) int {
	return estimateSerializedTokens(request, request.MaxOutputTokens)
}

func estimateModelInputTokens(request ModelRequest) int {
	return estimateSerializedTokens(request, 0)
}

func estimateSerializedTokens(value any, outputAllowance int) int {
	if outputAllowance < 0 {
		outputAllowance = 0
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return outputAllowance + 1024
	}
	// JSON framing, tool schemas and English prose are predominantly ASCII.
	// Reserving one token per two ASCII bytes is deliberately more cautious
	// than common model tokenizers without treating every byte as a token.
	// CJK text is commonly close to one token per three UTF-8 bytes; four-byte
	// runes round upward. Provider-reported usage remains the settlement truth.
	asciiBytes := 0
	nonASCIIBytes := 0
	for _, current := range encoded {
		if current < 0x80 {
			asciiBytes++
		} else {
			nonASCIIBytes++
		}
	}
	inputEstimate := (asciiBytes+1)/2 + (nonASCIIBytes+2)/3
	if inputEstimate == 0 {
		inputEstimate = 1
	}
	return inputEstimate + outputAllowance
}

func (s *Service) executeCalls(ctx context.Context, task TaskContext, request *ModelRequest, calls []ToolCall, modelToolIDs map[string]string, state *loopState, grant *tool.Grant) (bool, error) {
	return s.executeCallsWithRecovery(ctx, task, request, calls, modelToolIDs, state, grant, false)
}

// executeRecoveredCalls lets the first pending call reach Tool Gateway before
// refreshing input. The gateway can then replay an already-durable intent; if
// no matching intent exists, the repository input fence rejects a new stale
// effect. Later siblings use the ordinary attention check.
func (s *Service) executeRecoveredCalls(ctx context.Context, task TaskContext, request *ModelRequest, calls []ToolCall, modelToolIDs map[string]string, state *loopState, grant *tool.Grant) (bool, error) {
	return s.executeCallsWithRecovery(ctx, task, request, calls, modelToolIDs, state, grant, true)
}

func (s *Service) executeCallsWithRecovery(ctx context.Context, task TaskContext, request *ModelRequest, calls []ToolCall, modelToolIDs map[string]string, state *loopState, grant *tool.Grant, recoverFirst bool) (bool, error) {
	for index, call := range calls {
		if !recoverFirst || index > 0 {
			changed, err := s.refreshTaskInputs(ctx, task, request, state)
			if err != nil {
				return false, err
			}
			if changed {
				return false, ErrStaleTaskInput
			}
		}
		canceled, err := s.cancelIfRequested(ctx, task, *request, state)
		if err != nil || canceled {
			return canceled, err
		}
		toolID, found := modelToolIDs[call.Name]
		if !found {
			request.Messages = append(request.Messages, toolErrorMessage(call, fmt.Sprintf("tool %q is not available", call.Name)))
			state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{ModelTurnID: latestModelTurnID(state), ToolCallID: call.ID, Status: "rejected_unknown_tool"})
			if err := s.saveToolBatchCheckpoint(ctx, task, *request, calls[index+1:], modelToolIDs, *state); err != nil {
				return false, err
			}
			continue
		}
		callGrant := (*tool.Grant)(nil)
		if index == 0 {
			callGrant = grant
		}
		toolStarted := time.Now()
		s.logger.Info("tool call started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "tool_id", toolID, "tool_call_id", call.ID)
		outcome, invokeErr := s.tools.Invoke(ctx, tool.Request{
			TaskID: task.TaskID, RunID: task.RunID, InvocationID: task.InvocationID,
			ToolCallID: call.ID, BasisInputSequence: state.InputSequence, ToolID: toolID, Input: call.Arguments, Grant: callGrant,
		})
		if errors.Is(invokeErr, tool.ErrStaleTaskInput) {
			if _, err := s.refreshTaskInputs(ctx, task, request, state); err != nil {
				return false, err
			}
			return false, ErrStaleTaskInput
		}
		s.logger.Info("tool call finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "invocation_id", task.InvocationID, "tool_id", toolID, "tool_call_id", call.ID, "intent_id", outcome.Intent.ID, "status", outcome.Intent.Status, "approval_required", outcome.ApprovalRequired, "duration_ms", time.Since(toolStarted).Milliseconds(), "error_code", outcome.Intent.ErrorCode)
		if outcome.ApprovalRequired {
			state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
				ModelTurnID: latestModelTurnID(state), ToolCallID: call.ID, ToolID: toolID, IntentID: outcome.Intent.ID, Status: "approval_required",
			})
			continuation := pendingContinuation{
				Request: *request, ModelToolIDs: modelToolIDs,
				PendingCalls: append([]ToolCall(nil), calls[index:]...), State: *state,
			}
			if err := s.pauseForApproval(ctx, task, outcome, continuation); err != nil {
				return false, err
			}
			return true, nil
		}
		observation := map[string]any{
			"tool_id": toolID, "intent_id": outcome.Intent.ID,
			"status": outcome.Intent.Status, "control": outcome.Control,
		}
		if invokeErr != nil {
			observation["success"] = false
			observation["error_code"] = outcome.Intent.ErrorCode
			observation["error"] = "the tool did not produce a confirmed result"
		} else {
			state.ConfirmedEffects++
			observation["success"] = true
			observation["result"] = outcome.Result
			if outcome.Result.Deferred != nil {
				if state.PendingDeferred != nil && state.PendingDeferred.ID != outcome.Result.Deferred.ID {
					return false, fmt.Errorf("only one deferred subagent delegation may be active in a task")
				}
				state.PendingDeferred = &pendingDeferred{
					ID: outcome.Result.Deferred.ID, Kind: outcome.Result.Deferred.Kind, RoleID: outcome.Result.Deferred.Type,
					ProviderID: outcome.Result.Deferred.ProviderID,
					ToolCallID: call.ID, IntentID: outcome.Intent.ID,
				}
			}
			if toolID == "builtin.skills" {
				if name := activatedSkillName(outcome.Result.Output); name != "" && !contains(state.SkillIDs, name) {
					state.SkillIDs = append(state.SkillIDs, name)
					sort.Strings(state.SkillIDs)
					state.ContextManifest.SkillIDs = append([]string(nil), state.SkillIDs...)
					encodedManifest, err := json.Marshal(state.ContextManifest)
					if err != nil {
						return false, err
					}
					if err := s.repository.UpdateInvocationContext(ctx, task.InvocationID, string(encodedManifest)); err != nil {
						return false, err
					}
				}
			}
			if len(outcome.Result.Attachments) > 0 {
				metadata := make([]map[string]any, 0, len(outcome.Result.Attachments))
				for _, attachment := range outcome.Result.Attachments {
					state.Attachments = append(state.Attachments, ArtifactAttachment{
						ID: attachment.ID, Name: attachment.Name, MediaType: attachment.MediaType,
						SizeBytes: attachment.SizeBytes, ContentRef: attachment.ContentRef,
					})
					metadata = append(metadata, map[string]any{
						"id": attachment.ID, "name": attachment.Name, "media_type": attachment.MediaType, "size_bytes": attachment.SizeBytes,
					})
				}
				observation["attachments"] = metadata
			}
		}
		state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
			ModelTurnID: latestModelTurnID(state), ToolCallID: call.ID, ToolID: toolID, IntentID: outcome.Intent.ID,
			Status: string(outcome.Intent.Status), ResultRef: outcome.Intent.ResultRef,
		})
		encodedObservation, err := json.Marshal(observation)
		if err != nil {
			return false, err
		}
		request.Messages = append(request.Messages, Message{
			Role: "tool", ToolCallID: call.ID, Content: string(encodedObservation),
		})
		if err := s.saveToolBatchCheckpoint(ctx, task, *request, calls[index+1:], modelToolIDs, *state); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *Service) saveToolBatchCheckpoint(ctx context.Context, task TaskContext, request ModelRequest, pending []ToolCall, modelToolIDs map[string]string, state loopState) error {
	if err := s.saveAgentCheckpoint(ctx, task, "model_received", pendingContinuation{
		Request: request, ModelToolIDs: modelToolIDs, PendingCalls: append([]ToolCall(nil), pending...), State: state,
	}); err != nil {
		return fmt.Errorf("save tool-result checkpoint: %w", err)
	}
	return nil
}

func activatedSkillName(output json.RawMessage) string {
	var value struct {
		Operation string `json:"operation"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(output, &value) != nil || value.Operation != "load" {
		return ""
	}
	return strings.TrimSpace(value.Name)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Service) cancelIfRequested(ctx context.Context, task TaskContext, request ModelRequest, state *loopState) (bool, error) {
	requested, err := s.repository.TaskCancelRequested(ctx, task.TaskID)
	if err != nil || !requested {
		return false, err
	}
	state.Trace.RuntimeStop = "user_canceled"
	traceRef, err := s.storeTrace(ctx, task.InvocationID, traceWithProviderTranscript(state.Trace, request))
	if err != nil {
		return false, err
	}
	if err := s.repository.CommitTaskCancellation(ctx, task.TaskID, task.RunID, task.InvocationID, traceRef, state.Usage); err != nil {
		return false, err
	}
	return true, nil
}

func toolErrorMessage(call ToolCall, message string) Message {
	body, _ := json.Marshal(map[string]any{"success": false, "error": message})
	return Message{Role: "tool", ToolCallID: call.ID, Content: string(body)}
}
