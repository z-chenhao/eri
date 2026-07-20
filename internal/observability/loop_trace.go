package observability

import (
	"context"
	"encoding/json"
	"time"

	"github.com/z-chenhao/eri/internal/content"
)

// These mirrors decode the encrypted Runtime trace inside the authorized
// observability service. RunSpan projection below never emits assistant text,
// prompts, tool arguments from model messages, or private reasoning.
type persistedRunTrace struct {
	ModelTurns   []persistedModelTurn  `json:"model_turns"`
	ToolCalls    []persistedToolCall   `json:"tool_calls"`
	Evaluations  []persistedEvaluation `json:"evaluations"`
	Progress     []persistedProgress   `json:"progress,omitempty"`
	RuntimeStop  string                `json:"runtime_stop,omitempty"`
	FailureCause string                `json:"failure_cause,omitempty"`
}

type persistedModelTurn struct {
	ID            string                `json:"id"`
	Ordinal       int                   `json:"ordinal"`
	Trigger       string                `json:"trigger"`
	Status        string                `json:"status"`
	StartedAt     time.Time             `json:"started_at"`
	EndedAt       time.Time             `json:"ended_at"`
	Checkpoints   []string              `json:"checkpoints,omitempty"`
	InputSequence int64                 `json:"input_sequence"`
	FinishReason  string                `json:"finish_reason,omitempty"`
	Request       persistedModelRequest `json:"request"`
	Assistant     persistedAssistant    `json:"assistant"`
	Usage         persistedUsage        `json:"usage"`
}

type persistedModelRequest struct {
	MessageCount         int            `json:"message_count"`
	MessageRoles         map[string]int `json:"message_roles"`
	ToolNames            []string       `json:"tool_names,omitempty"`
	MaxOutputTokens      int            `json:"max_output_tokens"`
	EstimatedInputTokens int            `json:"estimated_input_tokens"`
}

type persistedAssistant struct {
	Content   string              `json:"content,omitempty"`
	ToolCalls []persistedToolName `json:"tool_calls,omitempty"`
}

type persistedToolName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type persistedUsage struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheHitTokens  int    `json:"cache_hit_tokens"`
	CacheMissTokens int    `json:"cache_miss_tokens"`
	ReasoningTokens int    `json:"reasoning_tokens"`
	DurationMillis  int64  `json:"duration_ms"`
}

type persistedActiveTurn struct {
	ID            string    `json:"id"`
	Ordinal       int       `json:"ordinal"`
	Trigger       string    `json:"trigger"`
	StartedAt     time.Time `json:"started_at"`
	Checkpoints   []string  `json:"checkpoints,omitempty"`
	InputSequence int64     `json:"input_sequence"`
}

type persistedToolCall struct {
	ModelTurnID string `json:"model_turn_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolID      string `json:"tool_id,omitempty"`
	IntentID    string `json:"intent_id,omitempty"`
	Status      string `json:"status"`
}

type persistedEvaluation struct {
	ID          string    `json:"id"`
	ModelTurnID string    `json:"model_turn_id"`
	Attempt     int       `json:"attempt"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	Result      string    `json:"result"`
	Tier        string    `json:"tier"`
	Findings    []string  `json:"findings,omitempty"`
}

type persistedProgress struct {
	ID          string    `json:"id"`
	ModelTurnID string    `json:"model_turn_id"`
	DeliveryID  string    `json:"delivery_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Service) hydrateLoopTrace(ctx context.Context, detail *RunDetail) error {
	if s.content == nil {
		return nil
	}
	var ref content.Ref
	active, found, err := s.repository.LoadActiveRunTrace(ctx, detail.Run.ID)
	if err != nil {
		return err
	}
	if found {
		ref = active
	}
	if ref.ObjectID == "" {
		for index := len(detail.Artifacts) - 1; index >= 0; index-- {
			if detail.Artifacts[index].TraceRef.ObjectID != "" {
				ref = detail.Artifacts[index].TraceRef
				break
			}
		}
	}
	if ref.ObjectID == "" {
		return nil
	}
	body, err := s.content.Get(ctx, ref)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return err
	}
	if _, wrapped := root["state"]; wrapped {
		var continuation struct {
			State struct {
				Trace      persistedRunTrace    `json:"trace"`
				ActiveTurn *persistedActiveTurn `json:"active_turn,omitempty"`
			} `json:"state"`
		}
		if err := json.Unmarshal(body, &continuation); err != nil {
			return err
		}
		detail.loopTrace = continuation.State.Trace
		detail.activeTurn = continuation.State.ActiveTurn
		return nil
	}
	return json.Unmarshal(body, &detail.loopTrace)
}

func (detail RunDetail) hasExplicitLoopTrace() bool {
	if len(detail.loopTrace.ModelTurns) == 0 && detail.activeTurn == nil {
		return false
	}
	for _, turn := range detail.loopTrace.ModelTurns {
		if turn.ID == "" || turn.Ordinal <= 0 {
			return false
		}
	}
	return detail.activeTurn == nil || (detail.activeTurn.ID != "" && detail.activeTurn.Ordinal > 0)
}
