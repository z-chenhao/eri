// Package agui projects Eri's durable execution facts onto the AG-UI event
// protocol. It intentionally exposes no private prompts, model reasoning,
// memory bodies or ungoverned tool results.
package agui

import (
	"fmt"
	"strings"

	"github.com/z-chenhao/eri/internal/eventlog"
)

const (
	RunStarted    = "RUN_STARTED"
	RunFinished   = "RUN_FINISHED"
	RunError      = "RUN_ERROR"
	StepStarted   = "STEP_STARTED"
	StepFinished  = "STEP_FINISHED"
	ToolCallStart = "TOOL_CALL_START"
	ToolCallEnd   = "TOOL_CALL_END"
	Custom        = "CUSTOM"
)

type Context struct {
	ThreadID string
	RunID    string
	Exposure string
}

// Event is the stable subset of the AG-UI event union emitted by Eri.
type Event struct {
	Type         string `json:"type"`
	Timestamp    int64  `json:"timestamp,omitempty"`
	ThreadID     string `json:"threadId,omitempty"`
	RunID        string `json:"runId,omitempty"`
	StepName     string `json:"stepName,omitempty"`
	ToolCallID   string `json:"toolCallId,omitempty"`
	ToolCallName string `json:"toolCallName,omitempty"`
	Message      string `json:"message,omitempty"`
	Code         string `json:"code,omitempty"`
	Name         string `json:"name,omitempty"`
	Value        any    `json:"value,omitempty"`
	Result       any    `json:"result,omitempty"`
}

// Project maps one committed Eri fact to zero or more AG-UI events. Unknown or
// developer-only facts are omitted rather than mislabeled as standard events.
func Project(fact eventlog.Event, context Context) []Event {
	if ValidateContext(context) != nil {
		return nil
	}
	timestamp := fact.Time.UnixMilli()
	runID := firstString(context.RunID, dataString(fact, "run_id"))
	switch fact.Type {
	case "task.started":
		return []Event{{Type: RunStarted, Timestamp: timestamp, ThreadID: context.ThreadID, RunID: runID}}
	case "task.completed":
		return []Event{{Type: RunFinished, Timestamp: timestamp, ThreadID: context.ThreadID, RunID: runID}}
	case "task.failed":
		return []Event{{Type: RunError, Timestamp: timestamp, ThreadID: context.ThreadID, RunID: runID, Message: "Eri could not complete the task", Code: "task_failed"}}
	case "task.canceled":
		return []Event{{Type: RunFinished, Timestamp: timestamp, ThreadID: context.ThreadID, RunID: runID, Result: map[string]any{"status": "canceled"}}}
	case "invocation.planned":
		return []Event{{Type: StepStarted, Timestamp: timestamp, StepName: "agent-loop:" + fact.AggregateID}}
	case "invocation.succeeded", "invocation.failed", "invocation.canceled":
		return []Event{{Type: StepFinished, Timestamp: timestamp, StepName: "agent-loop:" + fact.AggregateID}}
	case "effect.planned":
		name := dataString(fact, "tool_id")
		toolCallID := dataString(fact, "tool_call_id")
		if name == "" || toolCallID == "" {
			return safeCustom(fact, context, "eri.effect.planned", map[string]any{"intentId": fact.AggregateID})
		}
		return []Event{{Type: ToolCallStart, Timestamp: timestamp, ToolCallID: toolCallID, ToolCallName: name}}
	case "effect.confirmed", "effect.failed", "effect.unknown", "effect.compensated", "effect.rejected":
		toolCallID := dataString(fact, "tool_call_id")
		if toolCallID == "" {
			return safeCustom(fact, context, "eri.effect.status", map[string]any{"intentId": fact.AggregateID, "status": strings.TrimPrefix(fact.Type, "effect.")})
		}
		return []Event{
			{Type: ToolCallEnd, Timestamp: timestamp, ToolCallID: toolCallID},
			{Type: Custom, Timestamp: timestamp, Name: "eri.effect.status", Value: map[string]any{"intentId": fact.AggregateID, "status": strings.TrimPrefix(fact.Type, "effect.")}},
		}
	case "approval.requested":
		return safeCustom(fact, context, "eri.approval.requested", map[string]any{"approvalId": fact.AggregateID})
	case "approval.approved", "approval.denied", "approval.expired":
		return safeCustom(fact, context, "eri.approval.resolved", map[string]any{
			"approvalId": fact.AggregateID,
			"status":     strings.TrimPrefix(fact.Type, "approval."),
		})
	default:
		return nil
	}
}

func safeCustom(fact eventlog.Event, context Context, name string, value map[string]any) []Event {
	if context.Exposure != "developer" && !strings.HasPrefix(name, "eri.approval.") && !strings.HasPrefix(name, "eri.effect.") {
		return nil
	}
	return []Event{{Type: Custom, Timestamp: fact.Time.UnixMilli(), Name: name, Value: value}}
}

func dataString(fact eventlog.Event, key string) string {
	value, _ := fact.Data[key].(string)
	return value
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func ValidateContext(context Context) error {
	if strings.TrimSpace(context.ThreadID) == "" {
		return fmt.Errorf("AG-UI thread ID is required")
	}
	return nil
}
