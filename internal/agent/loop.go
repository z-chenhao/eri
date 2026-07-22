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
	"github.com/z-chenhao/eri/internal/memory"
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
		compact: s.compactLoopContext, refresh: s.refreshAuthoritativeContext, cancel: s.cancelIfRequested, checkpoint: s.saveAgentCheckpoint,
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
			driver.logger.Warn("in-run context compaction failed", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
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
			driver.logger.Warn("model request withheld because its tool protocol is invalid", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "error_code", "invalid_model_transcript", "error", err.Error())
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
		driver.logger.Info("model call started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "provider", driver.loop.ModelTarget)
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
			response.Message.SendTime = time.Now().UTC()
			response.Message.TemporalContext = true
			finishActiveTurn(&state, &response, "succeeded")
		}
		driver.logger.Info("model call finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "duration_ms", time.Since(modelStarted).Milliseconds(), "input_tokens", response.Usage.InputTokens, "output_tokens", response.Usage.OutputTokens, "cache_hit_tokens", response.Usage.CacheHitTokens, "cache_miss_tokens", response.Usage.CacheMissTokens, "error_code", observability.ErrorCode(callErr), "error", observability.SafeError(callErr))
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
				driver.logger.Info("model result superseded by newer user input", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "input_sequence", state.InputSequence)
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
			// Keep the just-produced assistant message out of refresh assembly.
			// Joined input may remove older transient overlays, so an index captured
			// before refresh is not stable.
			assistantMessage := request.Messages[len(request.Messages)-1]
			request.Messages = request.Messages[:len(request.Messages)-1]
			changed, err := driver.refresh(ctx, task, &request, &state)
			if err != nil {
				return driver.fail(ctx, task, request, state.Usage, "task_input_unavailable", state.Trace)
			}
			if changed {
				markLatestTurnSuperseded(&state)
				driver.logger.Info("model result superseded by newer user input", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "input_sequence", state.InputSequence)
				state.NextTurnTrigger = "user_input"
				continue
			}
			request.Messages = append(request.Messages, assistantMessage)
			assistantIndex = len(request.Messages) - 1
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
				assistantIndex = findAssistantToolFrameIndex(request.Messages, response.Message.ToolCalls)
				if _, err := closeInterruptedToolFrame(&request, assistantIndex, modelToolIDs, &state); err != nil {
					return driver.fail(ctx, task, request, state.Usage, "invalid_model_transcript", state.Trace)
				}
				driver.logger.Info("tool turn superseded before effect dispatch", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "input_sequence", state.InputSequence)
				state.NextTurnTrigger = "user_input"
				if err := driver.checkpoint(ctx, task, "ready_for_model", pendingContinuation{
					Request: request, ModelToolIDs: modelToolIDs, State: state,
				}); err != nil {
					return fmt.Errorf("save superseded-tool checkpoint: %w", err)
				}
				continue
			}
			assistantIndex = findAssistantToolFrameIndex(request.Messages, response.Message.ToolCalls)
		}
		observationStart := len(request.Messages)
		paused, err := driver.execute(ctx, task, &request, response.Message.ToolCalls, modelToolIDs, &state, nil)
		if errors.Is(err, ErrStaleTaskInput) || errors.Is(err, ErrStaleConversationContext) {
			assistantIndex = findAssistantToolFrameIndex(request.Messages, response.Message.ToolCalls)
			retained, closeErr := closeInterruptedToolFrame(&request, assistantIndex, modelToolIDs, &state)
			if closeErr != nil {
				return driver.fail(ctx, task, request, state.Usage, "invalid_model_transcript", state.Trace)
			}
			driver.logger.Info("tool turn interrupted at effect fence", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "input_sequence", state.InputSequence, "protocol_frame_retained", retained)
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
				driver.logger.Warn("progress message was withheld", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "turn", turnOrdinal, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
		observations := []Message(nil)
		if observationStart < len(request.Messages) {
			observations = request.Messages[observationStart:]
		}
		stagnant := updateLoopProgress(&state, response.Message.ToolCalls, observations)
		if stagnant >= 4 {
			if state.ConfirmedEffects > 0 && !state.SynthesisOnly {
				request.Tools = nil
				request.Messages = replaceSystemOverlay(request.Messages, "runtime_control", "The governed no-progress check found repeated tool work. Stop calling tools. Use only confirmed observations already in this transcript to give the user the best supported result now, state any material limitation plainly, and do not claim missing work succeeded.")
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
			request.Messages = replaceSystemOverlay(request.Messages, "runtime_control", "The last native tool action and its observation repeated without new information. Reflect on the unmet success criteria and change strategy, tool, query, or scope; do not repeat the same call again.")
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

func findAssistantToolFrameIndex(messages []Message, calls []ToolCall) int {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != "assistant" || len(message.ToolCalls) != len(calls) {
			continue
		}
		matched := true
		for callIndex := range calls {
			if message.ToolCalls[callIndex].ID != calls[callIndex].ID || message.ToolCalls[callIndex].Name != calls[callIndex].Name {
				matched = false
				break
			}
		}
		if matched {
			return index
		}
	}
	return -1
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
		ID: task.ExecutionKey() + ":turn:" + strconv.Itoa(ordinal), Ordinal: ordinal,
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
	if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != "assistant" || len(request.Messages[len(request.Messages)-1].ToolCalls) != 0 {
		return request, false, fmt.Errorf("candidate boundary requires one final assistant message")
	}
	candidate := request.Messages[len(request.Messages)-1]
	request.Messages = request.Messages[:len(request.Messages)-1]
	findingsStart := len(state.EvalFindings)
	changed, err := s.refreshTaskInputs(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if changed {
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded before Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "input_sequence", state.InputSequence)
		state.NextTurnTrigger = "user_input"
		return request, true, nil
	}
	conversationChanged, err := s.refreshConversationUpdates(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if conversationChanged {
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded by newer Conversation context before Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "conversation_sequence", state.ConversationSequence)
		state.NextTurnTrigger = "conversation_update"
		return request, true, nil
	}
	request.Messages = append(request.Messages, candidate)
	body := strings.TrimSpace(candidate.Content)
	evaluationStartedAt := time.Now().UTC()
	modelTurnID := latestModelTurnID(state)
	evaluationAttempt := state.EvalAttempts + 1
	confirmedTools := make([]string, 0)
	for _, call := range state.Trace.ToolCalls {
		if call.Status == string(tool.IntentConfirmed) {
			confirmedTools = append(confirmedTools, call.ToolID)
		}
	}
	s.logger.Info("evaluation started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "attempt", evaluationAttempt)
	decision, judgeUsage, judgeErr := s.evaluateCandidate(ctx, task.TaskID, JudgeRequest{
		CandidateContext: state.JudgeContext, Messages: request.Messages, TaskText: state.TaskText,
		SkillIDs: state.SkillIDs, ConfirmedTools: confirmedTools, MaxOutputTokens: s.loop.MaxOutputTokens,
		MemoryClaimIDs:     judgeMemoryClaimIDs(state.ContextManifest),
		SoulGuidedResponse: true,
	})
	state.Usage = mergeUsage(state.Usage, judgeUsage)
	s.logger.Info("evaluation finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "attempt", evaluationAttempt, "result", decision.Result, "tier", decision.Tier, "duration_ms", time.Since(evaluationStartedAt).Milliseconds(), "input_tokens", judgeUsage.InputTokens, "output_tokens", judgeUsage.OutputTokens, "cache_hit_tokens", judgeUsage.CacheHitTokens, "cache_miss_tokens", judgeUsage.CacheMissTokens, "error_code", observability.ErrorCode(judgeErr), "error", observability.SafeError(judgeErr))
	if judgeErr != nil {
		state.Trace.RuntimeStop = "llm_judge_unavailable"
		trace := traceWithProviderTranscript(state.Trace, request)
		if state.ConfirmedEffects > 0 {
			return request, false, s.commitFailureAfterEffect(ctx, task, state.Usage, "llm_judge_unavailable", trace)
		}
		return request, false, s.commitFailure(ctx, task, state.Usage, "llm_judge_unavailable", trace)
	}
	request.Messages = request.Messages[:len(request.Messages)-1]
	changed, err = s.refreshTaskInputs(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if changed {
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded during Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "input_sequence", state.InputSequence)
		state.NextTurnTrigger = "user_input"
		return request, true, nil
	}
	conversationChanged, err = s.refreshConversationUpdates(ctx, task, &request, state)
	if err != nil {
		return request, false, err
	}
	if conversationChanged {
		markLatestTurnSuperseded(state)
		s.logger.Info("candidate superseded by newer Conversation context during Eval", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "conversation_sequence", state.ConversationSequence)
		state.NextTurnTrigger = "conversation_update"
		return request, true, nil
	}
	request.Messages = append(request.Messages, candidate)
	state.EvalAttempts++
	state.Trace.Evaluations = append(state.Trace.Evaluations, evaluationTrace{
		ID: modelTurnID + ":eval:" + strconv.Itoa(evaluationAttempt), ModelTurnID: modelTurnID, Attempt: evaluationAttempt,
		StartedAt: evaluationStartedAt, EndedAt: time.Now().UTC(),
		Result: decision.Result, Tier: decision.Tier, Findings: decision.Findings, Usage: judgeUsage,
	})
	if s.evolution != nil {
		if err := s.evolution.Observe(ctx, EvolutionSignal{
			RunID: task.RunID, ExperienceReleaseID: state.ContextManifest.ExperienceReleaseID, Result: decision.Result, Tier: decision.Tier,
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
	appliedMemoryUses, err := resolveAppliedMemoryUses(state.ContextManifest, decision.AppliedMemoryClaims)
	if err != nil {
		return request, false, s.commitFailure(ctx, task, state.Usage, "llm_judge_invalid_memory_use", traceWithProviderTranscript(state.Trace, request))
	}
	err = s.commitEvaluatedReply(ctx, task, traceWithProviderTranscript(state.Trace, request), state.Usage, body, "text", decision.Tier, state.EvalFindings, state.Attachments, state.InputSequence, state.ConversationSequence, appliedMemoryUses)
	if errors.Is(err, ErrStaleTaskInput) {
		request.Messages = request.Messages[:len(request.Messages)-1]
		changed, refreshErr := s.refreshTaskInputs(ctx, task, &request, state)
		if refreshErr != nil {
			return request, false, refreshErr
		}
		if changed {
			if state.EvalAttempts > 0 {
				state.EvalAttempts--
			}
			state.EvalFindings = state.EvalFindings[:findingsStart]
			markLatestTurnSuperseded(state)
			s.logger.Info("candidate superseded at durable commit fence", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "input_sequence", state.InputSequence)
			state.NextTurnTrigger = "user_input"
			return request, true, nil
		}
		request.Messages = append(request.Messages, candidate)
	}
	if errors.Is(err, ErrStaleConversationContext) {
		request.Messages = request.Messages[:len(request.Messages)-1]
		changed, refreshErr := s.refreshConversationUpdates(ctx, task, &request, state)
		if refreshErr != nil {
			return request, false, refreshErr
		}
		if changed {
			if state.EvalAttempts > 0 {
				state.EvalAttempts--
			}
			state.EvalFindings = state.EvalFindings[:findingsStart]
			markLatestTurnSuperseded(state)
			s.logger.Info("candidate superseded at Conversation commit fence", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "conversation_sequence", state.ConversationSequence)
			state.NextTurnTrigger = "conversation_update"
			return request, true, nil
		}
		request.Messages = append(request.Messages, candidate)
	}
	return request, false, err
}

func judgeMemoryClaimIDs(manifest execution.ContextManifest) []string {
	claims := make([]string, 0, len(manifest.MemoryBindings))
	for _, binding := range manifest.MemoryBindings {
		appendUnique(&claims, binding.ClaimID)
	}
	if len(claims) == 0 {
		for _, claimID := range manifest.MemoryClaimIDs {
			appendUnique(&claims, claimID)
		}
	}
	return claims
}

func resolveAppliedMemoryUses(manifest execution.ContextManifest, appliedClaims []string) ([]MemoryUse, error) {
	if len(appliedClaims) == 0 {
		return nil, nil
	}
	bindings := append([]execution.MemoryBinding(nil), manifest.MemoryBindings...)
	if len(bindings) == 0 && manifest.MemoryRetrievalID != "" && len(manifest.MemoryIDs) == len(manifest.MemoryClaimIDs) {
		for index, claimID := range manifest.MemoryClaimIDs {
			bindings = append(bindings, execution.MemoryBinding{
				RetrievalID: manifest.MemoryRetrievalID, MemoryID: manifest.MemoryIDs[index], ClaimID: claimID,
			})
		}
	}
	if len(bindings) == 0 {
		return nil, fmt.Errorf("applied Memory lacks a complete retrieval manifest")
	}
	byClaim := make(map[string][]execution.MemoryBinding, len(bindings))
	for _, binding := range bindings {
		binding.ClaimID = strings.TrimSpace(binding.ClaimID)
		binding.MemoryID = strings.TrimSpace(binding.MemoryID)
		binding.RetrievalID = strings.TrimSpace(binding.RetrievalID)
		if binding.ClaimID == "" || binding.MemoryID == "" || binding.RetrievalID == "" {
			return nil, fmt.Errorf("Memory retrieval manifest contains an empty identity")
		}
		claimBindings := byClaim[binding.ClaimID]
		duplicate := false
		for _, existing := range claimBindings {
			if existing.RetrievalID == binding.RetrievalID && existing.MemoryID == binding.MemoryID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			byClaim[binding.ClaimID] = append(claimBindings, binding)
		}
	}
	byRetrieval := make(map[string][]string)
	retrievalOrder := make([]string, 0)
	for _, claimID := range appliedClaims {
		claimBindings, ok := byClaim[strings.TrimSpace(claimID)]
		if !ok {
			return nil, fmt.Errorf("Judge applied unknown Memory claim %q", claimID)
		}
		for _, binding := range claimBindings {
			if _, exists := byRetrieval[binding.RetrievalID]; !exists {
				retrievalOrder = append(retrievalOrder, binding.RetrievalID)
			}
			memoryIDs := byRetrieval[binding.RetrievalID]
			appendUnique(&memoryIDs, binding.MemoryID)
			byRetrieval[binding.RetrievalID] = memoryIDs
		}
	}
	uses := make([]MemoryUse, 0, len(retrievalOrder))
	for _, retrievalID := range retrievalOrder {
		uses = append(uses, MemoryUse{RetrievalID: retrievalID, MemoryIDs: byRetrieval[retrievalID]})
	}
	return uses, nil
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

func latestUserTurnIndex(messages []Message) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			return index
		}
	}
	return -1
}

func protectedSourceIndex(messages []Message, state *loopState) int {
	if state != nil && state.ProtectedSourceMessage > 0 {
		index := state.ProtectedSourceMessage - 1
		if index >= 0 && index < len(messages) && (messages[index].Role == "user" || messages[index].Role == "system") {
			if taskText := strings.TrimSpace(state.TaskText); taskText == "" || strings.TrimSpace(messages[index].Content) == taskText {
				return index
			}
		}
	}
	if state != nil {
		taskText := strings.TrimSpace(state.TaskText)
		if taskText != "" {
			for index := len(messages) - 1; index >= 0; index-- {
				if (messages[index].Role == "user" || messages[index].Role == "system") && strings.TrimSpace(messages[index].Content) == taskText {
					return index
				}
			}
		}
	}
	return latestUserTurnIndex(messages)
}

func protectedContextStart(messages []Message, sourceIndex int) int {
	if sourceIndex > 0 && sourceIndex < len(messages) {
		previous := messages[sourceIndex-1]
		if previous.Role == "system" && strings.HasPrefix(strings.TrimSpace(previous.Content), "<relevant_memory>") {
			return sourceIndex - 1
		}
	}
	return sourceIndex
}

func (s *Service) compactLoopContext(
	ctx context.Context,
	task TaskContext,
	request ModelRequest,
	capabilities ModelCapabilities,
	state *loopState,
) (ModelRequest, Usage, error) {
	if len(request.Messages) == 0 {
		limit := contextInputLimit(capabilities, request.MaxOutputTokens)
		return request, Usage{}, fmt.Errorf("context exceeds %d tokens without messages to compact", limit)
	}
	protectedSource := protectedSourceIndex(request.Messages, state)
	if protectedSource < 0 {
		limit := contextInputLimit(capabilities, request.MaxOutputTokens)
		return request, Usage{}, fmt.Errorf("context exceeds %d tokens without a current source turn to preserve", limit)
	}
	protectedStart := protectedContextStart(request.Messages, protectedSource)
	// A newer joined source can leave Memory or Runtime feedback from the old
	// decision point earlier in the transcript. Remove those transient overlays
	// before either the Agent or the checkpoint summarizer can consume them.
	filtered := make([]Message, 0, len(request.Messages))
	removedBeforeSource := 0
	for index, message := range request.Messages {
		if index < protectedStart && isTransientSystemOverlay(message) {
			removedBeforeSource++
			continue
		}
		filtered = append(filtered, message)
	}
	request.Messages = filtered
	protectedSource -= removedBeforeSource
	protectedStart -= removedBeforeSource
	state.ProtectedSourceMessage = protectedSource + 1

	before := estimateModelInputTokens(request)
	limit := contextInputLimit(capabilities, request.MaxOutputTokens)
	if before <= limit {
		return request, Usage{}, nil
	}
	keepTokens := defaultRecentTokens
	if keepTokens > limit/2 {
		keepTokens = limit / 2
	}
	cut := findLoopCut(request.Messages, keepTokens)
	if cut <= 0 || cut > protectedStart {
		// The current source turn, including a trusted system_reminder, is the
		// authoritative cause of this Run. Its immediately preceding dynamic
		// Memory selection is part of the protected suffix as well: summarizing
		// it would make deleted or replaced Memory survive into later Runs.
		cut = protectedStart
	}
	if cut <= 0 {
		return request, Usage{}, fmt.Errorf("current source turn and relevant Memory exceed the %d-token input limit and cannot be summarized", limit)
	}
	originalProtectedStart := protectedStart
	summary, usage, err := s.summarizeContext(ctx, task.TaskID, durableSummaryMessages(request.Messages[:cut]), capabilities)
	if err != nil {
		return request, usage, err
	}
	checkpoint := Message{
		Role:    "system",
		Content: "Eri in-run context checkpoint. It summarizes prior native model/tool turns; continue the same task from it.\n\n" + summary,
	}
	request.Messages = append([]Message{checkpoint}, request.Messages[cut:]...)
	protectedSource = 1 + protectedSource - cut
	protectedStart = protectedContextStart(request.Messages, protectedSource)
	after := estimateModelInputTokens(request)
	if after > limit && protectedStart > 1 {
		// The first pass preserves a recent closed prefix for continuity. If
		// that is still too large, fold only the remaining history before the
		// protected suffix into a second checkpoint. Dynamic Memory, the current
		// user turn, and subsequent native Tool frames remain byte-for-byte
		// messages.
		summary, nextUsage, err := s.summarizeContext(ctx, task.TaskID, durableSummaryMessages(request.Messages[:protectedStart]), capabilities)
		usage = mergeUsage(usage, nextUsage)
		if err != nil {
			return request, usage, err
		}
		request.Messages = append([]Message{{
			Role:    "system",
			Content: "Eri in-run context checkpoint. It summarizes prior closed history; continue the same task from the preserved user turn.\n\n" + summary,
		}}, request.Messages[protectedStart:]...)
		protectedSource = 1 + protectedSource - protectedStart
		protectedStart = protectedContextStart(request.Messages, protectedSource)
		after = estimateModelInputTokens(request)
		cut = originalProtectedStart
	}
	if after > limit {
		return request, usage, fmt.Errorf("current source turn and subsequent native Tool frames remain over limit after safe compaction: %d > %d", after, limit)
	}
	state.ProtectedSourceMessage = protectedSource + 1
	state.ContextManifest.RuntimeCompactions = append(state.ContextManifest.RuntimeCompactions, execution.RuntimeCompaction{
		TokensBefore: before, TokensAfter: after, SummarizedMessages: cut,
	})
	state.ContextManifest.EstimatedInputTokens = after
	encoded, err := json.Marshal(state.ContextManifest)
	if err != nil {
		return request, usage, err
	}
	if err := s.repository.UpdateRunContext(ctx, task.RunID, string(encoded)); err != nil {
		return request, usage, err
	}
	s.logger.Info("in-run context compacted", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "summarized_messages", cut, "tokens_before", before, "tokens_after", after, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "cache_hit_tokens", usage.CacheHitTokens, "cache_miss_tokens", usage.CacheMissTokens)
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
	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == "assistant" && len(request.Messages[len(request.Messages)-1].ToolCalls) == 0 {
		request.Messages = append([]Message(nil), request.Messages[:len(request.Messages)-1]...)
	}
	request.Messages = replaceSystemOverlay(request.Messages, "evaluation_feedback", evalRepairInstruction(decision))
	if decision.Result == eval.Escalate {
		// The Judge has established that only the user can supply the missing
		// input. Tools cannot resolve that condition and exposing them here lets
		// the model incorrectly continue as if the user had answered.
		request.Tools = nil
	}
	return request
}

func replaceSystemOverlay(messages []Message, kind, body string) []Message {
	prefix := "<" + kind + ">"
	message := Message{Role: "system", Content: prefix + "\n" + strings.TrimSpace(body) + "\n</" + kind + ">"}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "system" && strings.HasPrefix(strings.TrimSpace(messages[index].Content), prefix) {
			messages[index] = message
			return messages
		}
	}
	return append(messages, message)
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
	if len(state.PendingMemoryMutations) > 0 {
		for _, call := range calls {
			request.Messages = append(request.Messages, toolErrorMessage(call, "the call was not executed because a governed Memory mutation invalidated this Tool frame; reconsider it in a new model turn"))
			state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
				ModelTurnID: latestModelTurnID(state), ToolCallID: call.ID, ToolID: modelToolIDs[call.Name], Status: "skipped_after_memory_mutation",
			})
		}
		if err := s.finalizePendingMemoryMutations(ctx, task, request, modelToolIDs, state); err != nil {
			return false, err
		}
		return false, nil
	}
	exclusiveMemoryMutation := -1
	for index, call := range calls {
		if modelToolIDs[call.Name] != "builtin.memory" {
			continue
		}
		if _, _, ok := proposedMemoryMutation(call.Arguments); ok {
			exclusiveMemoryMutation = index
			break
		}
	}
	for index, call := range calls {
		if exclusiveMemoryMutation >= 0 && index != exclusiveMemoryMutation {
			request.Messages = append(request.Messages, toolErrorMessage(call, "the call was not executed because a governed Memory mutation must run alone; reconsider it in a new model turn"))
			state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
				ModelTurnID: latestModelTurnID(state), ToolCallID: call.ID, ToolID: modelToolIDs[call.Name], Status: "skipped_for_memory_mutation",
			})
			continue
		}
		if !recoverFirst || index > 0 {
			changed, err := s.refreshAuthoritativeContext(ctx, task, request, state)
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
		s.logger.Info("tool call started", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "tool_id", toolID, "tool_call_id", call.ID)
		sourceInteractionID := state.SourceInteractionID
		if sourceInteractionID == "" {
			sourceInteractionID = task.CurrentTask.SourceInteractionID
		}
		outcome, invokeErr := s.tools.Invoke(ctx, tool.Request{
			TaskID: task.TaskID, RunID: task.RunID, InvocationID: task.ExecutionKey(),
			SourceInteractionID: sourceInteractionID, SourceInteractionText: state.SourceInteractionText,
			SourceInteractionRole: state.SourceInteractionRole, SourceInteractionKind: state.SourceInteractionKind,
			ToolCallID: call.ID, BasisInputSequence: state.InputSequence,
			BasisConversationSequence: state.ConversationSequence, ToolID: toolID, Input: call.Arguments, Grant: callGrant,
		})
		if errors.Is(invokeErr, tool.ErrStaleTaskInput) {
			if _, err := s.refreshTaskInputs(ctx, task, request, state); err != nil {
				return false, err
			}
			return false, ErrStaleTaskInput
		}
		if errors.Is(invokeErr, tool.ErrStaleConversationContext) {
			if _, err := s.refreshConversationUpdates(ctx, task, request, state); err != nil {
				return false, err
			}
			return false, ErrStaleConversationContext
		}
		s.logger.Info("tool call finished", "component", "agent", "task_id", task.TaskID, "run_id", task.RunID, "execution_id", task.ExecutionKey(), "tool_id", toolID, "tool_call_id", call.ID, "intent_id", outcome.Intent.ID, "status", outcome.Intent.Status, "approval_required", outcome.ApprovalRequired, "duration_ms", time.Since(toolStarted).Milliseconds(), "error_code", outcome.Intent.ErrorCode)
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
			"tool_id": call.Name, "intent_id": outcome.Intent.ID,
			"status": outcome.Intent.Status, "control": outcome.Control,
		}
		memoryMutation := pendingMemoryMutation{}
		if invokeErr != nil {
			observation["success"] = false
			observation["error_code"] = outcome.Intent.ErrorCode
			observation["error"] = "the tool did not produce a confirmed result"
			if toolID == "builtin.memory" {
				if operation, targetID, mutationAttempted := proposedMemoryMutation(call.Arguments); mutationAttempted {
					memoryMutation = pendingMemoryMutation{
						Operation: operation, TargetID: targetID, Status: "uncertain",
						IntentID: outcome.Intent.ID,
					}
					appendPendingMemoryMutation(state, memoryMutation)
				}
			}
		} else {
			state.ConfirmedEffects++
			observation["success"] = true
			observation["result"] = modelVisibleToolResult(outcome.Result)
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
					if err := s.repository.UpdateRunContext(ctx, task.RunID, string(encodedManifest)); err != nil {
						return false, err
					}
				}
			}
			if toolID == "builtin.memory" {
				changed := mergeMemoryReadManifest(&state.ContextManifest, outcome.Result.Output, s.loop.ExternalModel)
				if mutation, ok := confirmedMemoryMutation(call.Arguments, outcome.Result.Receipt); ok {
					memoryMutation = mutation
					appendPendingMemoryMutation(state, mutation)
				}
				if changed {
					encodedManifest, err := json.Marshal(state.ContextManifest)
					if err != nil {
						return false, err
					}
					if err := s.repository.UpdateRunContext(ctx, task.RunID, string(encodedManifest)); err != nil {
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
		if memoryMutation.TargetID != "" {
			for _, skipped := range calls[index+1:] {
				request.Messages = append(request.Messages, toolErrorMessage(skipped, "the call was not executed because a governed Memory mutation invalidated this Tool frame; reconsider it in a new model turn"))
				state.Trace.ToolCalls = append(state.Trace.ToolCalls, toolResultTrace{
					ModelTurnID: latestModelTurnID(state), ToolCallID: skipped.ID, ToolID: modelToolIDs[skipped.Name], Status: "skipped_after_memory_mutation",
				})
			}
			if err := s.finalizePendingMemoryMutations(ctx, task, request, modelToolIDs, state); err != nil {
				return false, err
			}
			return false, nil
		}
		if err := s.saveToolBatchCheckpoint(ctx, task, *request, calls[index+1:], modelToolIDs, *state); err != nil {
			return false, err
		}
	}
	return false, nil
}

func modelVisibleToolResult(result tool.Result) map[string]any {
	visible := map[string]any{
		"output": result.Output, "receipt": result.Receipt,
		"fresh_at": result.FreshAt, "uncertainty": result.Uncertainty,
	}
	if strings.TrimSpace(result.ExternalObjectID) != "" {
		visible["external_object_id"] = result.ExternalObjectID
	}
	if result.Deferred != nil {
		visible["deferred"] = result.Deferred
	}
	return visible
}

func confirmedMemoryMutation(arguments json.RawMessage, receipt string) (pendingMemoryMutation, bool) {
	operation, targetID, ok := proposedMemoryMutation(arguments)
	if targetID == "" || strings.TrimSpace(receipt) == "" {
		return pendingMemoryMutation{}, false
	}
	if !ok {
		return pendingMemoryMutation{}, false
	}
	return pendingMemoryMutation{Operation: operation, TargetID: targetID, Receipt: strings.TrimSpace(receipt), Status: "confirmed"}, true
}

func proposedMemoryMutation(arguments json.RawMessage) (string, string, bool) {
	var input struct {
		Operation  string `json:"operation"`
		MemoryID   string `json:"memory_id"`
		ReplacesID string `json:"replaces_memory_id"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		return "", "", false
	}
	operation := strings.TrimSpace(input.Operation)
	if operation != "forget" && operation != "restrict" && !(operation == "record" && strings.TrimSpace(input.ReplacesID) != "") {
		return "", "", false
	}
	targetID := strings.TrimSpace(input.MemoryID)
	if targetID == "" {
		targetID = strings.TrimSpace(input.ReplacesID)
	}
	return operation, targetID, targetID != ""
}

func appendPendingMemoryMutation(state *loopState, mutation pendingMemoryMutation) {
	for _, existing := range state.PendingMemoryMutations {
		if existing == mutation {
			return
		}
	}
	state.PendingMemoryMutations = append(state.PendingMemoryMutations, mutation)
}

func (s *Service) finalizePendingMemoryMutations(ctx context.Context, task TaskContext, request *ModelRequest, modelToolIDs map[string]string, state *loopState) error {
	if request == nil || state == nil || len(state.PendingMemoryMutations) == 0 {
		return nil
	}
	mutations := append([]pendingMemoryMutation(nil), state.PendingMemoryMutations...)
	targets := make(map[string]struct{}, len(mutations))
	for _, mutation := range mutations {
		targets[mutation.TargetID] = struct{}{}
	}
	request.Messages = sanitizeMemoryExposedTranscript(request.Messages)
	filtered := make([]Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		content := strings.TrimSpace(message.Content)
		if message.Role == "system" && (strings.HasPrefix(content, "<relevant_memory>") || strings.HasPrefix(content, "<relevant_memory_context>")) {
			continue
		}
		filtered = append(filtered, message)
	}
	request.Messages = filtered
	for _, mutation := range mutations {
		request.Messages = append(request.Messages, Message{Role: "system", Content: formatMemoryMutationEvent(mutation)})
	}
	state.PendingMemoryMutations = nil
	state.ContextManifest.MemoryRetrievalID = ""
	state.ContextManifest.MemoryIDs = nil
	state.ContextManifest.MemoryClaimIDs = nil
	manualRetrievals := make(map[string]struct{}, len(state.ContextManifest.MemoryToolRetrievalIDs))
	for _, retrievalID := range state.ContextManifest.MemoryToolRetrievalIDs {
		manualRetrievals[retrievalID] = struct{}{}
	}
	bindings := state.ContextManifest.MemoryBindings[:0]
	for _, binding := range state.ContextManifest.MemoryBindings {
		_, manual := manualRetrievals[binding.RetrievalID]
		_, affected := targets[binding.MemoryID]
		if manual && !affected {
			bindings = append(bindings, binding)
		}
	}
	state.ContextManifest.MemoryBindings = bindings
	state.JudgeContext = candidateEvaluationContext(s.identity, memory.Bundle{}, state.ContextManifest.SourceChannel, time.Now())
	if sourceIndex := protectedSourceIndex(request.Messages, state); sourceIndex >= 0 {
		state.ProtectedSourceMessage = sourceIndex + 1
	}
	state.NextTurnTrigger = "memory_mutation"
	encodedManifest, err := json.Marshal(state.ContextManifest)
	if err != nil {
		return err
	}
	if err := s.repository.UpdateRunContext(ctx, task.RunID, string(encodedManifest)); err != nil {
		return err
	}
	if err := s.saveAgentCheckpoint(ctx, task, "ready_for_model", pendingContinuation{
		Request: *request, ModelToolIDs: modelToolIDs, State: *state,
	}); err != nil {
		return fmt.Errorf("save Memory-mutation checkpoint: %w", err)
	}
	return nil
}

func sanitizeMemoryExposedTranscript(messages []Message) []Message {
	boundary := len(messages)
	toolNames := make(map[string]string)
	for index, message := range messages {
		content := strings.TrimSpace(message.Content)
		if message.Role == "system" && (strings.HasPrefix(content, "<relevant_memory>") ||
			strings.HasPrefix(content, "<relevant_memory_context>") ||
			isOwnedContextCheckpoint(content)) {
			if index < boundary {
				boundary = index
			}
		}
		if message.Role != "assistant" {
			continue
		}
		// A prior non-Memory Tool turn may still have reasoned over automatically
		// injected Memory whose volatile overlay was intentionally omitted from a
		// carried transcript. At a governed mutation boundary, the only provable
		// safe rebuild is therefore to drop the whole retained assistant/tool
		// suffix, not only frames whose Tool name or arguments mention Memory.
		if index < boundary {
			boundary = index
		}
		for _, call := range message.ToolCalls {
			toolNames[call.ID] = call.Name
		}
	}
	if boundary == len(messages) {
		return append([]Message(nil), messages...)
	}
	filtered := make([]Message, 0, len(messages))
	seenReceipts := make(map[string]struct{})
	for index, message := range messages {
		if index < boundary {
			filtered = append(filtered, message)
			continue
		}
		switch message.Role {
		case "assistant":
			if len(message.ToolCalls) == 0 && strings.TrimSpace(message.Content) != "" {
				message.ReasoningContent = ""
				filtered = append(filtered, message)
			}
			continue
		case "tool":
			name := toolNames[message.ToolCallID]
			if !isMemoryToolCall(name) {
				if receipt, key, ok := safeToolReceiptEvent(name, message.Content); ok {
					if _, duplicate := seenReceipts[key]; !duplicate {
						seenReceipts[key] = struct{}{}
						filtered = append(filtered, Message{Role: "system", Content: receipt})
					}
				}
			}
			continue
		case "system":
			content := strings.TrimSpace(message.Content)
			if strings.HasPrefix(content, "<relevant_memory>") || strings.HasPrefix(content, "<relevant_memory_context>") ||
				isOwnedContextCheckpoint(content) {
				continue
			}
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func isMemoryToolCall(name string) bool {
	switch strings.TrimSpace(name) {
	case "memory", "builtin.memory", "builtin_memory":
		return true
	default:
		return false
	}
}

func safeToolReceiptEvent(name, body string) (string, string, bool) {
	var observation struct {
		Success  bool   `json:"success"`
		IntentID string `json:"intent_id"`
		Status   string `json:"status"`
		ToolID   string `json:"tool_id"`
		Result   struct {
			Receipt string `json:"receipt"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &observation) != nil || !observation.Success || strings.TrimSpace(observation.Result.Receipt) == "" {
		return "", "", false
	}
	if strings.TrimSpace(observation.ToolID) != "" {
		name = observation.ToolID
	}
	status := strings.TrimSpace(observation.Status)
	if status == "" {
		status = "confirmed"
	}
	key := strings.TrimSpace(observation.IntentID) + "\x00" + strings.TrimSpace(observation.Result.Receipt)
	event := "<runtime_event type=\"tool.receipt\">\n" +
		"  <tool>" + escapeXMLText(strings.TrimSpace(name)) + "</tool>\n" +
		"  <intent_id>" + escapeXMLText(strings.TrimSpace(observation.IntentID)) + "</intent_id>\n" +
		"  <status>" + escapeXMLText(status) + "</status>\n" +
		"  <receipt>" + escapeXMLText(strings.TrimSpace(observation.Result.Receipt)) + "</receipt>\n" +
		"</runtime_event>"
	return event, key, true
}

func formatMemoryMutationEvent(mutation pendingMemoryMutation) string {
	if mutation.Status != "confirmed" || strings.TrimSpace(mutation.Receipt) == "" {
		return "<runtime_event type=\"memory.mutation_uncertain\">\n" +
			"  <operation>" + escapeXMLText(mutation.Operation) + "</operation>\n" +
			"  <memory_id>" + escapeXMLText(mutation.TargetID) + "</memory_id>\n" +
			"  <intent_id>" + escapeXMLText(mutation.IntentID) + "</intent_id>\n" +
			"  <status>uncertain</status>\n" +
			"</runtime_event>"
	}
	return "<runtime_event type=\"memory.mutated\">\n" +
		"  <operation>" + escapeXMLText(mutation.Operation) + "</operation>\n" +
		"  <memory_id>" + escapeXMLText(mutation.TargetID) + "</memory_id>\n" +
		"  <receipt>" + escapeXMLText(mutation.Receipt) + "</receipt>\n" +
		"</runtime_event>"
}

func mergeMemoryReadManifest(manifest *execution.ContextManifest, output json.RawMessage, external bool) bool {
	if manifest == nil {
		return false
	}
	var bundle memory.Bundle
	if json.Unmarshal(output, &bundle) != nil || strings.TrimSpace(bundle.RetrievalID) == "" {
		return false
	}
	changed := appendUnique(&manifest.MemoryToolRetrievalIDs, bundle.RetrievalID)
	for _, entry := range bundle.Entries {
		changed = appendUnique(&manifest.RetrievedMemoryIDs, entry.MemoryID) || changed
		if external {
			changed = appendUnique(&manifest.ExternalMemoryIDs, entry.MemoryID) || changed
			if manifest.ExternalData != nil {
				changed = appendUnique(&manifest.ExternalData.MemoryIDs, entry.MemoryID) || changed
			}
		}
		binding := execution.MemoryBinding{RetrievalID: bundle.RetrievalID, MemoryID: entry.MemoryID, ClaimID: entry.ClaimID}
		bindingExists := false
		for _, existing := range manifest.MemoryBindings {
			if existing == binding {
				bindingExists = true
				break
			}
		}
		if !bindingExists && binding.MemoryID != "" && binding.ClaimID != "" {
			manifest.MemoryBindings = append(manifest.MemoryBindings, binding)
			changed = true
		}
	}
	return changed
}

func appendUnique(values *[]string, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || contains(*values, value) {
		return false
	}
	*values = append(*values, value)
	return true
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
	traceRef, err := s.storeTrace(ctx, task.ExecutionKey(), traceWithProviderTranscript(state.Trace, request))
	if err != nil {
		return false, err
	}
	if err := s.repository.CommitTaskCancellation(ctx, task.TaskID, task.RunID, traceRef, state.Usage); err != nil {
		return false, err
	}
	return true, nil
}

func toolErrorMessage(call ToolCall, message string) Message {
	body, _ := json.Marshal(map[string]any{"success": false, "error": message})
	return Message{Role: "tool", ToolCallID: call.ID, Content: string(body)}
}
