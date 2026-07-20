package observability

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/execution"
)

// RunSpan is a committed runtime fact, not a model thought. DependsOn is a
// causal edge list: consumers must never replace it with chronological chaining.
type RunSpan struct {
	ID          string                 `json:"id"`
	ParentID    string                 `json:"parent_id,omitempty"`
	Kind        string                 `json:"kind"`
	Lane        string                 `json:"lane"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	Status      string                 `json:"status"`
	StartedAt   time.Time              `json:"started_at,omitempty"`
	EndedAt     time.Time              `json:"ended_at,omitempty"`
	DependsOn   []string               `json:"depends_on"`
	Links       []RunSpanLink          `json:"links,omitempty"`
	Metadata    map[string]any         `json:"metadata,omitempty"`
	Memory      *MemoryRetrievalRecord `json:"memory,omitempty"`
	Exchange    *CallExchange          `json:"exchange,omitempty"`
}

// CallExchange exposes governed request/response facts for a selected Model
// or Tool node. It never contains a full model prompt, private reasoning,
// credentials, or an ungoverned tool result.
type CallExchange struct {
	Request    any    `json:"request,omitempty"`
	Response   any    `json:"response,omitempty"`
	Disclosure string `json:"disclosure"`
}

type RunSpanLink struct {
	SpanID string `json:"span_id"`
	Kind   string `json:"kind"`
}

func (s *Service) buildRunSpans(ctx context.Context, detail RunDetail, exposure memoryExposure) ([]RunSpan, error) {
	startID := "run:" + detail.Run.ID
	spans := []RunSpan{{
		ID: startID, Kind: "runtime", Lane: "runtime", Title: "Durable Runtime accepted the task",
		Description: "The task entered the recoverable runtime. This projection only exposes committed facts.", Status: "succeeded",
		StartedAt: detail.Run.StartedAt, Metadata: map[string]any{"run_id": detail.Run.ID, "task_id": detail.Run.TaskID},
	}}
	terminalInputs := []string{startID}
	loopParents := []string{}
	explicitLoop := detail.hasExplicitLoopTrace()
	usedEffects := map[string]bool{}
	effectSpans := make(map[string]RunSpan, len(detail.Effects))
	for _, effect := range detail.Effects {
		effectSpans[effect.ID] = RunSpan{ID: "effect:" + effect.ID}
	}

	for invocationIndex, invocation := range detail.Invocations {
		contextID := "context:" + invocation.ID
		memoryID := "memory:" + invocation.ID
		loopID := "loop:" + invocation.ID
		memoryRecord, err := s.memoryRetrieval(ctx, invocation.ContextManifest, exposure)
		if err != nil {
			return nil, err
		}
		spans = append(spans,
			RunSpan{
				ID: contextID, ParentID: startID, Kind: "context", Lane: "context", Title: "Assemble task context",
				Description: "Load the Soul, conversation, active skills, and available tool schemas.", Status: "succeeded",
				StartedAt: invocation.CreatedAt, DependsOn: []string{startID},
				Metadata: safeContextMetadata(invocation.ContextManifest),
			},
			RunSpan{
				ID: memoryID, ParentID: startID, Kind: "memory", Lane: "memory", Title: memorySpanTitle(memoryRecord),
				Description: "Memory is retrieved independently before model input. Stored does not mean retrieved for this run.", Status: "succeeded",
				StartedAt: invocation.CreatedAt, DependsOn: []string{startID}, Memory: &memoryRecord,
			},
			RunSpan{
				ID: loopID, ParentID: startID, Kind: "agent_loop", Lane: "agent", Title: invocationTitle(invocation),
				Description: "Agent Loop is an inspectable execution boundary. It exposes control-flow facts without private reasoning.", Status: invocation.Status,
				StartedAt: invocation.CreatedAt, EndedAt: invocation.UpdatedAt, DependsOn: []string{contextID, memoryID},
				Metadata: safeInvocationMetadata(invocation),
			},
		)
		trace := loopTraceForInvocation(detail, invocation, invocationIndex)
		if trace.explicit {
			loopSpanIndex := len(spans) - 1
			spans[loopSpanIndex].Title = loopSummaryTitle(trace)
			spans[loopSpanIndex].Metadata = loopSummaryMetadata(invocation, trace)
			children, linked := buildLoopDetailSpans(loopID, contextID, memoryID, trace, detail.Effects)
			spans = append(spans, children...)
			for effectID, span := range linked {
				usedEffects[effectID] = true
				effectSpans[effectID] = span
			}
		}
		loopParents = append(loopParents, loopID)
	}
	if len(loopParents) > 0 {
		terminalInputs = append([]string(nil), loopParents...)
	} else {
		loopParents = append([]string(nil), terminalInputs...)
	}
	effectEnds := make([]string, 0, len(detail.Effects))
	for _, effect := range detail.Effects {
		if explicitLoop && usedEffects[effect.ID] {
			continue
		}
		parentIDs := append([]string(nil), loopParents...)
		parentID := ""
		metadata := map[string]any{"effect_id": effect.ID, "parent_intent_id": effect.ParentIntentID, "tool_id": effect.ToolID, "effect_class": effect.Effect, "target": effect.Target, "control": effect.Control, "error_code": effect.ErrorCode}
		description := "Durable state for a native tool effect. Parallel branches are not presented as a sequence."
		if effect.ParentIntentID != "" {
			if parent, found := effectSpans[effect.ParentIntentID]; found && parent.ID != "" && parent.ID != "effect:"+effect.ID {
				parentIDs = []string{parent.ID}
				parentID = parent.ParentID
				for key, value := range inheritedIterationMetadata(parent.Metadata) {
					metadata[key] = value
				}
				description = "Durable state for a subagent tool effect. Its parent edge comes from the committed parent_intent_id."
			} else {
				parentIDs = nil
				description = "This child tool declares parent_intent_id, but the parent intent is absent from this projection. It remains isolated."
			}
		} else if explicitLoop {
			parentIDs = nil
			description = "This durable effect has no explicit turn link, so it remains an independent fact."
		}
		if effect.ApprovalID != "" {
			approvalID := "approval:" + effect.ApprovalID
			spans = append(spans, RunSpan{
				ID: approvalID, ParentID: parentID, Kind: "approval", Lane: "policy", Title: "User approval",
				Description: "This effect requires user authorization bound to the exact operation.", Status: approvalStatus(effect.Status),
				StartedAt: effect.CreatedAt, DependsOn: append([]string(nil), parentIDs...),
				Metadata: mergeMetadata(inheritedIterationMetadata(metadata), map[string]any{"approval_id": effect.ApprovalID, "control": effect.Control}),
			})
			parentIDs = uniqueStrings(append([]string{approvalID}, parentIDs...))
		}
		effectID := "effect:" + effect.ID
		spans = append(spans, RunSpan{
			ID: effectID, ParentID: parentID, Kind: "tool", Lane: "effects", Title: effectTitle(effect),
			Description: description, Status: effect.Status,
			StartedAt: effect.CreatedAt, EndedAt: effect.UpdatedAt, DependsOn: parentIDs,
			Metadata: metadata, Exchange: effect.Exchange,
		})
		effectEnds = append(effectEnds, effectID)
	}
	if len(effectEnds) > 0 && !explicitLoop {
		terminalInputs = uniqueStrings(append(append([]string(nil), loopParents...), effectEnds...))
	}

	deliveryEnds := make([]string, 0, len(detail.Artifacts))
	for _, artifact := range detail.Artifacts {
		if artifact.Kind == "approval_request" {
			continue
		}
		evalID := "eval:" + artifact.ID
		if artifact.EvalID != "" {
			evalID = "eval:" + artifact.EvalID
		}
		artifactInputs := append([]string(nil), terminalInputs...)
		evalTitle := fmt.Sprintf("Pre-delivery eval · v%d", artifact.Version)
		if artifact.Kind == "progress" {
			if turnID := progressTurnID(detail.loopTrace.Progress, artifact.ID); turnID != "" {
				artifactInputs = []string{"model:" + turnID}
			}
			evalTitle = fmt.Sprintf("Progress eval · v%d", artifact.Version)
		}
		spans = append(spans, RunSpan{
			ID: evalID, Kind: "eval", Lane: "quality", Title: evalTitle,
			Description: "The candidate receives a risk-matched evaluation before delivery.", Status: defaultStatus(artifact.Eval, artifact.Status),
			DependsOn: artifactInputs,
			Metadata:  map[string]any{"artifact_id": artifact.ID, "version": artifact.Version, "tier": artifact.EvalTier, "evaluator": artifact.EvalEvaluator, "finding_count": artifact.EvalFindingCount},
		})
		endID := evalID
		if artifact.Delivery != "" || artifact.DeliveryID != "" {
			deliveryID := "delivery:" + artifact.ID
			if artifact.DeliveryID != "" {
				deliveryID = "delivery:" + artifact.DeliveryID
			}
			deliveryTitle := "Deliver to the authoritative conversation"
			if artifact.Kind == "progress" {
				deliveryTitle = "Send progress without ending the task"
			}
			spans = append(spans, RunSpan{
				ID: deliveryID, Kind: "delivery", Lane: "delivery", Title: deliveryTitle,
				Description: "Only an evaluated version can enter delivery and receive a channel receipt.", Status: defaultStatus(artifact.Delivery, artifact.Status),
				DependsOn: []string{evalID}, Metadata: map[string]any{"artifact_id": artifact.ID, "receipt": artifact.Receipt},
			})
			endID = deliveryID
		}
		deliveryEnds = append(deliveryEnds, endID)
	}
	if len(deliveryEnds) > 0 {
		terminalInputs = deliveryEnds
	}

	finishID := "finish:" + detail.Run.ID
	spans = append(spans, RunSpan{
		ID: finishID, ParentID: startID, Kind: "finish", Lane: "runtime", Title: runFinishTitle(detail.Run.Status),
		Description: "The terminal run state is determined by committed runtime facts.", Status: detail.Run.Status,
		StartedAt: detail.Run.EndedAt, EndedAt: detail.Run.EndedAt, DependsOn: uniqueStrings(terminalInputs),
	})
	return spans, nil
}

type invocationLoopTrace struct {
	turns        []persistedModelTurn
	toolCalls    []persistedToolCall
	evaluations  []persistedEvaluation
	progress     []persistedProgress
	active       *persistedActiveTurn
	runtimeStop  string
	failureCause string
	explicit     bool
}

func loopTraceForInvocation(detail RunDetail, invocation Invocation, invocationIndex int) invocationLoopTrace {
	if !detail.hasExplicitLoopTrace() {
		return invocationLoopTrace{}
	}
	matchAll := len(detail.Invocations) == 1
	result := invocationLoopTrace{runtimeStop: detail.loopTrace.RuntimeStop, failureCause: detail.loopTrace.FailureCause}
	turnIDs := map[string]bool{}
	for _, turn := range detail.loopTrace.ModelTurns {
		if matchAll || strings.HasPrefix(turn.ID, invocation.ID+":turn:") {
			result.turns = append(result.turns, turn)
			turnIDs[turn.ID] = true
		}
	}
	if detail.activeTurn != nil && (matchAll || strings.HasPrefix(detail.activeTurn.ID, invocation.ID+":turn:")) {
		active := *detail.activeTurn
		result.active = &active
		turnIDs[active.ID] = true
	}
	for _, call := range detail.loopTrace.ToolCalls {
		if turnIDs[call.ModelTurnID] {
			result.toolCalls = append(result.toolCalls, call)
		}
	}
	for _, evaluation := range detail.loopTrace.Evaluations {
		if turnIDs[evaluation.ModelTurnID] {
			result.evaluations = append(result.evaluations, evaluation)
		}
	}
	for _, progress := range detail.loopTrace.Progress {
		if turnIDs[progress.ModelTurnID] {
			result.progress = append(result.progress, progress)
		}
	}
	result.explicit = len(result.turns) > 0 || result.active != nil
	_ = invocationIndex
	return result
}

func progressTurnID(progress []persistedProgress, artifactID string) string {
	for _, item := range progress {
		if item.ID == artifactID {
			return item.ModelTurnID
		}
	}
	return ""
}

func loopSummaryTitle(trace invocationLoopTrace) string {
	turnCount := len(trace.turns)
	if trace.active != nil {
		turnCount++
	}
	repairs := 0
	for _, evaluation := range trace.evaluations {
		if evaluation.Result == "repair" || evaluation.Result == "escalate" {
			repairs++
		}
	}
	return fmt.Sprintf("Agent Loop · %d Turns · %d Tools · %d Repair", turnCount, uniqueToolCallCount(trace.toolCalls), repairs)
}

func loopSummaryMetadata(invocation Invocation, trace invocationLoopTrace) map[string]any {
	metadata := safeInvocationMetadata(invocation)
	metadata["compound"] = true
	metadata["focusable"] = true
	metadata["turn_count"] = len(trace.turns)
	if trace.active != nil {
		metadata["turn_count"] = len(trace.turns) + 1
		metadata["current_turn"] = trace.active.Ordinal
		metadata["current_phase"] = lastCheckpoint(trace.active.Checkpoints, "ready_for_model")
	} else if len(trace.turns) > 0 {
		metadata["current_turn"] = trace.turns[len(trace.turns)-1].Ordinal
		metadata["current_phase"] = lastCheckpoint(trace.turns[len(trace.turns)-1].Checkpoints, trace.runtimeStop)
	}
	metadata["tool_count"] = uniqueToolCallCount(trace.toolCalls)
	metadata["eval_attempts"] = len(trace.evaluations)
	metadata["repair_count"] = evaluationResultCount(trace.evaluations, "repair") + evaluationResultCount(trace.evaluations, "escalate")
	metadata["runtime_stop"] = trace.runtimeStop
	metadata["failure_cause"] = trace.failureCause
	return metadata
}

func buildLoopDetailSpans(loopID, contextID, memoryID string, trace invocationLoopTrace, effects []Effect) ([]RunSpan, map[string]RunSpan) {
	spans := []RunSpan{}
	linkedEffects := map[string]RunSpan{}
	effectsByID := make(map[string]Effect, len(effects))
	for _, effect := range effects {
		effectsByID[effect.ID] = effect
	}
	previousExit := []string{contextID, memoryID}
	for _, turn := range trace.turns {
		turnMeta := iterationMetadata(loopID, turn.ID, turn.Ordinal, turn.Trigger)
		turnMeta["input_sequence"] = turn.InputSequence
		spans = append(spans, RunSpan{
			ID: "iteration:" + turn.ID, ParentID: loopID, Kind: "agent_iteration", Lane: "agent",
			Title: fmt.Sprintf("Turn %d", turn.Ordinal), Description: triggerDescription(turn.Trigger), Status: turn.Status,
			StartedAt: turn.StartedAt, EndedAt: turn.EndedAt, Metadata: turnMeta,
		})
		entryParents := append([]string(nil), previousExit...)
		if hasCheckpoint(turn.Checkpoints, "ready_for_model") {
			checkpointID := "checkpoint:" + turn.ID + ":ready"
			spans = append(spans, loopChildSpan(checkpointID, loopID, "checkpoint", "runtime", "Context ready", "A recoverable checkpoint was committed before the model call.", "succeeded", turn.StartedAt, time.Time{}, entryParents, turnMeta))
			entryParents = []string{checkpointID}
		}
		modelID := "model:" + turn.ID
		modelSpan := loopChildSpan(modelID, loopID, "model", "agent", "Model call", modelTurnDescription(turn), turn.Status, turn.StartedAt, turn.EndedAt, entryParents, turnMeta)
		modelSpan.Exchange = modelExchange(turn)
		spans = append(spans, modelSpan)
		if turn.Status == "superseded" {
			attentionID := "attention:" + turn.ID
			spans = append(spans, loopChildSpan(
				attentionID, loopID, "checkpoint", "runtime", "Newer user input admitted",
				"Runtime fenced the older model result and preserved the newer user message for the next Turn.",
				"succeeded", turn.EndedAt, time.Time{}, []string{modelID}, turnMeta,
			))
			previousExit = []string{attentionID}
			continue
		}
		turnCalls := toolCallsForTurn(trace.toolCalls, turn.ID)
		turnEvals := evaluationsForTurn(trace.evaluations, turn.ID)
		if len(turnCalls) > 0 {
			branchRoot := modelID
			if hasCheckpoint(turn.Checkpoints, "model_received") {
				checkpointID := "checkpoint:" + turn.ID + ":model"
				spans = append(spans, loopChildSpan(checkpointID, loopID, "checkpoint", "runtime", "Tool request committed", "The model tool call entered the durable recovery boundary.", "succeeded", turn.EndedAt, time.Time{}, []string{modelID}, turnMeta))
				branchRoot = checkpointID
			}
			terminals := []string{}
			approvalByCall := map[string]string{}
			for index, call := range turnCalls {
				callKey := call.ToolCallID
				if callKey == "" {
					callKey = fmt.Sprintf("call-%d", index+1)
				}
				if call.Status == "approval_required" {
					approvalID := "loop-approval:" + turn.ID + ":" + callKey
					spans = append(spans, loopChildSpan(approvalID, loopID, "approval", "policy", "Waiting for user approval", "Authorization is bound to the exact tool, target, and parameters. Raw parameters stay hidden.", "waiting", turn.EndedAt, time.Time{}, []string{branchRoot}, withToolMetadata(turnMeta, call, Effect{})))
					approvalByCall[callKey] = approvalID
					terminals = append(terminals, approvalID)
					continue
				}
				parents := []string{branchRoot}
				if approvalID := approvalByCall[callKey]; approvalID != "" {
					parents = []string{approvalID}
				}
				effect := effectsByID[call.IntentID]
				toolID := "loop-tool:" + turn.ID + ":" + callKey + ":" + fmt.Sprint(index+1)
				toolSpan := loopChildSpan(toolID, loopID, "tool", "effects", toolTraceTitle(call, effect), "Request and confirmed response are available after credential redaction and size limits.", call.Status, effect.CreatedAt, effect.UpdatedAt, parents, withToolMetadata(turnMeta, call, effect))
				toolSpan.Exchange = effect.Exchange
				spans = append(spans, toolSpan)
				if effect.ID != "" {
					linkedEffects[effect.ID] = toolSpan
				}
				terminals = append(terminals, toolID)
			}
			observationID := "observation:" + turn.ID
			spans = append(spans, loopChildSpan(observationID, loopID, "observation", "agent", "Governed observation merge", "Parallel tool branches merge into a governed observation for the next turn.", turn.Status, turn.EndedAt, time.Time{}, uniqueStrings(terminals), turnMeta))
			previousExit = []string{observationID}
		} else {
			candidateID := "candidate:" + turn.ID
			spans = append(spans, loopChildSpan(candidateID, loopID, "candidate", "agent", "Candidate response", "A candidate exists, but it cannot leave Agent Loop until it passes evaluation.", turn.Status, turn.EndedAt, time.Time{}, []string{modelID}, turnMeta))
			candidateExit := candidateID
			if hasCheckpoint(turn.Checkpoints, "candidate_received") {
				checkpointID := "checkpoint:" + turn.ID + ":candidate"
				spans = append(spans, loopChildSpan(checkpointID, loopID, "checkpoint", "runtime", "Candidate committed", "The candidate and loop state entered the recoverable boundary.", "succeeded", turn.EndedAt, time.Time{}, []string{candidateID}, turnMeta))
				candidateExit = checkpointID
			}
			previousExit = []string{candidateExit}
			for _, evaluation := range turnEvals {
				evalID := "loop-eval:" + evaluation.ID
				spans = append(spans, loopChildSpan(evalID, loopID, "eval", "quality", evaluationTitle(evaluation), evaluationDescription(evaluation), evaluation.Result, evaluation.StartedAt, evaluation.EndedAt, previousExit, turnMeta))
				previousExit = []string{evalID}
				if evaluation.Result == "repair" || evaluation.Result == "escalate" {
					repairID := "repair:" + evaluation.ID
					repairStatus := "waiting"
					if loopHasTurnAfter(trace, turn.Ordinal) {
						repairStatus = "succeeded"
					}
					spans = append(spans, loopChildSpan(repairID, loopID, "repair", "agent", repairTitle(evaluation.Result), "Governed eval findings become repair input for the next turn. Private reasoning stays hidden.", repairStatus, evaluation.EndedAt, time.Time{}, []string{evalID}, turnMeta))
					previousExit = []string{repairID}
				}
			}
		}
		if turn.Status != "superseded" && hasCheckpoint(turn.Checkpoints, "newer_user_input") {
			attentionID := "attention:" + turn.ID
			spans = append(spans, loopChildSpan(
				attentionID, loopID, "checkpoint", "runtime", "Newer user input admitted",
				"Runtime closed the native Tool Call frame, skipped unstarted sibling calls, and admitted the newer user message for the next Turn.",
				"succeeded", turn.EndedAt, time.Time{}, previousExit, turnMeta,
			))
			previousExit = []string{attentionID}
		}
	}
	if trace.active != nil {
		active := trace.active
		meta := iterationMetadata(loopID, active.ID, active.Ordinal, active.Trigger)
		meta["input_sequence"] = active.InputSequence
		spans = append(spans, RunSpan{
			ID: "iteration:" + active.ID, ParentID: loopID, Kind: "agent_iteration", Lane: "agent",
			Title: fmt.Sprintf("Turn %d", active.Ordinal), Description: triggerDescription(active.Trigger), Status: "running",
			StartedAt: active.StartedAt, Metadata: meta,
		})
		checkpointID := "checkpoint:" + active.ID + ":ready"
		spans = append(spans, loopChildSpan(checkpointID, loopID, "checkpoint", "runtime", "Context ready", "The current turn is committed and waiting for or executing the model call.", "running", active.StartedAt, time.Time{}, previousExit, meta))
	}
	return spans, linkedEffects
}

func loopChildSpan(id, loopID, kind, lane, title, description, status string, startedAt, endedAt time.Time, dependsOn []string, metadata map[string]any) RunSpan {
	return RunSpan{
		ID: id, ParentID: loopID, Kind: kind, Lane: lane, Title: title, Description: description,
		Status: status, StartedAt: startedAt, EndedAt: endedAt, DependsOn: append([]string(nil), dependsOn...),
		Metadata: cloneMetadata(metadata),
	}
}

func iterationMetadata(loopID, turnID string, ordinal int, trigger string) map[string]any {
	return map[string]any{
		"loop_id": loopID, "iteration_id": turnID, "iteration_ordinal": ordinal, "trigger": trigger,
	}
}

func cloneMetadata(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func inheritedIterationMetadata(source map[string]any) map[string]any {
	inherited := map[string]any{}
	for _, key := range []string{"loop_id", "iteration_id", "iteration_ordinal", "trigger"} {
		if value, found := source[key]; found {
			inherited[key] = value
		}
	}
	return inherited
}

func mergeMetadata(base map[string]any, additional map[string]any) map[string]any {
	merged := cloneMetadata(base)
	for key, value := range additional {
		merged[key] = value
	}
	return merged
}

func withToolMetadata(base map[string]any, call persistedToolCall, effect Effect) map[string]any {
	metadata := cloneMetadata(base)
	metadata["tool_call_id"] = call.ToolCallID
	metadata["tool_id"] = call.ToolID
	metadata["intent_id"] = call.IntentID
	if effect.ID != "" {
		metadata["effect_id"] = effect.ID
		metadata["effect_class"] = effect.Effect
		metadata["target"] = effect.Target
		metadata["control"] = effect.Control
		metadata["error_code"] = effect.ErrorCode
	}
	return metadata
}

func toolCallsForTurn(calls []persistedToolCall, turnID string) []persistedToolCall {
	result := []persistedToolCall{}
	for _, call := range calls {
		if call.ModelTurnID == turnID {
			result = append(result, call)
		}
	}
	return result
}

func evaluationsForTurn(evaluations []persistedEvaluation, turnID string) []persistedEvaluation {
	result := []persistedEvaluation{}
	for _, evaluation := range evaluations {
		if evaluation.ModelTurnID == turnID {
			result = append(result, evaluation)
		}
	}
	return result
}

func uniqueToolCallCount(calls []persistedToolCall) int {
	seen := map[string]bool{}
	for index, call := range calls {
		key := call.ToolCallID
		if key == "" {
			key = fmt.Sprintf("anonymous-%d", index)
		}
		seen[key] = true
	}
	return len(seen)
}

func evaluationResultCount(evaluations []persistedEvaluation, result string) int {
	count := 0
	for _, evaluation := range evaluations {
		if evaluation.Result == result {
			count++
		}
	}
	return count
}

func loopHasTurnAfter(trace invocationLoopTrace, ordinal int) bool {
	for _, turn := range trace.turns {
		if turn.Ordinal > ordinal {
			return true
		}
	}
	return trace.active != nil && trace.active.Ordinal > ordinal
}

func hasCheckpoint(checkpoints []string, phase string) bool {
	for _, checkpoint := range checkpoints {
		if checkpoint == phase {
			return true
		}
	}
	return false
}

func lastCheckpoint(checkpoints []string, fallback string) string {
	if len(checkpoints) > 0 {
		return checkpoints[len(checkpoints)-1]
	}
	return fallback
}

func triggerDescription(trigger string) string {
	switch trigger {
	case "tool_observations":
		return "Triggered by the governed observation from the previous turn."
	case "strategy_reflection":
		return "Triggered by policy reflection after a repeated observation."
	case "eval_repair":
		return "Triggered by an eval repair from the previous candidate."
	case "eval_escalation":
		return "Triggered by an eval escalation from the previous candidate."
	case "user_input":
		return "Triggered by a newer user message admitted at an Agent Loop attention boundary."
	default:
		return "Triggered by the user request and assembled context."
	}
}

func modelTurnDescription(turn persistedModelTurn) string {
	if turn.Status == "superseded" {
		return "A newer user message arrived before this result could act or deliver. The result was retained only as a governed runtime record."
	}
	if turn.FinishReason == "tool_calls" {
		return "The model requested native tool calls. Prompts, parameters, and private reasoning stay hidden."
	}
	if turn.Status == "failed" {
		return "The model call produced no usable response. Only the failure boundary is retained."
	}
	return "The model produced a candidate response. The canvas only shows control flow and verifiable state."
}

func toolTraceTitle(call persistedToolCall, effect Effect) string {
	if effect.ID != "" {
		return effectTitle(effect)
	}
	if call.ToolID != "" {
		return call.ToolID
	}
	return "Tool Call"
}

func evaluationTitle(evaluation persistedEvaluation) string {
	label := strings.ToUpper(evaluation.Result)
	if label == "" {
		label = "UNKNOWN"
	}
	return fmt.Sprintf("Eval %d · %s", evaluation.Attempt, label)
}

func evaluationDescription(evaluation persistedEvaluation) string {
	if len(evaluation.Findings) == 0 {
		return "The candidate completed a risk-matched evaluation."
	}
	return fmt.Sprintf("The candidate completed evaluation with %d governed findings.", len(evaluation.Findings))
}

func repairTitle(result string) string {
	if result == "escalate" {
		return "Retry after escalation"
	}
	return "Repair from eval"
}

func safeContextMetadata(manifest execution.ContextManifest) map[string]any {
	return map[string]any{
		"soul_version":       manifest.SoulVersion,
		"skill_ids":          append([]string(nil), manifest.SkillIDs...),
		"tool_count":         len(manifest.ToolIDs),
		"external_data_sent": manifest.ExternalDataSent,
	}
}

func safeInvocationMetadata(invocation Invocation) map[string]any {
	return map[string]any{
		"invocation_id": invocation.ID,
		"target":        invocation.Target,
		"model_calls":   intValue(invocation.Usage["model_calls"]),
		"input_tokens":  intValue(invocation.Usage["input_tokens"]),
		"output_tokens": intValue(invocation.Usage["output_tokens"]),
		"error_code":    invocation.ErrorCode,
	}
}

func invocationTitle(invocation Invocation) string {
	calls := intValue(invocation.Usage["model_calls"])
	if calls > 1 {
		return fmt.Sprintf("Agent Loop · %d model calls", calls)
	}
	return "Agent Loop · model processing"
}

func memorySpanTitle(record MemoryRetrievalRecord) string {
	if record.RetrievedCount == 0 {
		return "Memory · none retrieved"
	}
	return fmt.Sprintf("Memory · %d retrieved", record.RetrievedCount)
}

func effectTitle(effect Effect) string {
	title := strings.TrimSpace(effect.ToolID)
	if title == "" {
		title = "Tool Effect"
	}
	if target := strings.TrimSpace(effect.Target); target != "" {
		return title + " · " + target
	}
	return title
}

func approvalStatus(effectStatus string) string {
	switch effectStatus {
	case "planned":
		return "waiting"
	case "failed", "unknown", "compensated":
		return effectStatus
	default:
		return "approved"
	}
}

func runFinishTitle(status string) string {
	switch status {
	case "succeeded", "completed":
		return "Run completed"
	case "failed":
		return "Run failed"
	case "canceled":
		return "Run canceled"
	default:
		return "Run in progress"
	}
}

func defaultStatus(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func intValue(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		return 0
	}
}
