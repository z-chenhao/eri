package observability

import (
	"context"
	"time"
)

// ConversationActivity is deliberately smaller than the developer Run model.
// It is safe for the user-facing Conversation Workspace session.
type ConversationActivity struct {
	Active []ConversationRun `json:"active"`
	Recent []ConversationRun `json:"recent"`
}

type ConversationRun struct {
	RunID      string    `json:"run_id"`
	TaskID     string    `json:"task_id"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	ModelCalls int       `json:"model_calls"`
	ToolCalls  int       `json:"tool_calls"`
	Errors     int       `json:"errors"`
}

type ConversationTrace struct {
	TaskID       string    `json:"task_id"`
	RunID        string    `json:"run_id"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	ModelCalls   int       `json:"model_calls"`
	ToolCalls    int       `json:"tool_calls"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Steps        []RunSpan `json:"steps"`
}

func (s *Service) ConversationActivity(ctx context.Context, limit int) (ConversationActivity, error) {
	runs, err := s.Runs(ctx, limit)
	if err != nil {
		return ConversationActivity{}, err
	}
	activity := ConversationActivity{Active: []ConversationRun{}, Recent: []ConversationRun{}}
	for _, run := range runs {
		item := ConversationRun{
			RunID: run.ID, TaskID: run.TaskID, Status: run.Status, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
			ModelCalls: run.ModelCalls, ToolCalls: run.ToolCalls, Errors: run.Errors,
		}
		if run.Status == "active" || run.Status == "running" {
			activity.Active = append(activity.Active, item)
		} else {
			activity.Recent = append(activity.Recent, item)
		}
	}
	return activity, nil
}

func (s *Service) ConversationTrace(ctx context.Context, taskID string) (ConversationTrace, bool, error) {
	runs, err := s.Runs(ctx, 500)
	if err != nil {
		return ConversationTrace{}, false, err
	}
	for _, run := range runs {
		if run.TaskID != taskID {
			continue
		}
		detail, found, err := s.repository.LoadRun(ctx, run.ID)
		if err != nil || !found {
			return ConversationTrace{}, found, err
		}
		if err := s.hydrateLoopTrace(ctx, &detail); err != nil {
			return ConversationTrace{}, false, err
		}
		if err := s.hydrateEffectExchanges(ctx, &detail); err != nil {
			return ConversationTrace{}, false, err
		}
		spans, err := s.buildRunSpans(ctx, detail, memoryExposureConversation)
		if err != nil {
			return ConversationTrace{}, false, err
		}
		return ConversationTrace{
			TaskID: taskID, RunID: run.ID, Status: run.Status, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
			ModelCalls: run.ModelCalls, ToolCalls: run.ToolCalls, InputTokens: run.InputTokens, OutputTokens: run.OutputTokens,
			Steps: spans,
		}, true, nil
	}
	return ConversationTrace{}, false, nil
}
