package agui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/eventlog"
)

func TestProjectUsesStandardLifecycleAndToolEvents(t *testing.T) {
	now := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	started := eventlog.Event{Type: "task.started", AggregateID: "task-1", Time: now, Data: map[string]any{"run_id": "run-1"}}
	projected := Project(started, Context{ThreadID: "conversation-1", Exposure: "user"})
	if len(projected) != 1 || projected[0].Type != RunStarted || projected[0].ThreadID != "conversation-1" || projected[0].RunID != "run-1" {
		t.Fatalf("run projection = %+v", projected)
	}
	tool := eventlog.Event{Type: "effect.planned", AggregateID: "intent-1", Time: now, Data: map[string]any{"tool_id": "calendar.create", "tool_call_id": "call-1"}}
	projected = Project(tool, Context{ThreadID: "conversation-1", RunID: "run-1", Exposure: "user"})
	if len(projected) != 1 || projected[0].Type != ToolCallStart || projected[0].ToolCallName != "calendar.create" || projected[0].ToolCallID != "call-1" {
		t.Fatalf("tool projection = %+v", projected)
	}
	finished := eventlog.Event{Type: "effect.confirmed", AggregateID: "intent-1", Time: now, Data: map[string]any{"tool_call_id": "call-1"}}
	projected = Project(finished, Context{ThreadID: "conversation-1", RunID: "run-1", Exposure: "user"})
	if len(projected) != 2 || projected[0].Type != ToolCallEnd || projected[0].ToolCallID != "call-1" {
		t.Fatalf("tool completion projection = %+v", projected)
	}
}

func TestProjectDoesNotForgeAGUIToolCallIDFromEffectIntent(t *testing.T) {
	fact := eventlog.Event{Type: "effect.planned", AggregateID: "intent-1", Time: time.Now(), Data: map[string]any{"tool_id": "calendar.create"}}
	projected := Project(fact, Context{ThreadID: "conversation-1", Exposure: "user"})
	if len(projected) != 1 || projected[0].Type != Custom || projected[0].Name != "eri.effect.planned" {
		t.Fatalf("missing native tool_call_id must remain custom, got %+v", projected)
	}
	if projected := Project(fact, Context{}); len(projected) != 0 {
		t.Fatalf("invalid AG-UI context projected events: %+v", projected)
	}
}

func TestProjectNeverPassesThroughPrivateEventData(t *testing.T) {
	fact := eventlog.Event{
		Type: "memory.retrieved", AggregateID: "memory-1", Time: time.Now(),
		Data: map[string]any{"statement": "private preference", "private_prompt": "do not expose"},
	}
	if projected := Project(fact, Context{ThreadID: "conversation-1", Exposure: "user"}); len(projected) != 0 {
		t.Fatalf("private fact projected to AG-UI: %+v", projected)
	}
	approved := eventlog.Event{Type: "approval.requested", AggregateID: "approval-1", Time: time.Now(), Data: fact.Data}
	encoded, err := json.Marshal(Project(approved, Context{ThreadID: "conversation-1", Exposure: "user"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private preference") || strings.Contains(string(encoded), "private_prompt") {
		t.Fatalf("safe custom event leaked data: %s", encoded)
	}
}
