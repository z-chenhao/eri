// Package a2a projects Eri task facts onto the Agent2Agent task lifecycle.
// It is a transport-neutral edge model; HTTP, JSON-RPC or gRPC bindings remain
// gateway concerns.
package a2a

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/eventlog"
)

const Version = "1.0"

type TaskState string

const (
	TaskSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskWorking       TaskState = "TASK_STATE_WORKING"
	TaskCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskFailed        TaskState = "TASK_STATE_FAILED"
	TaskCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
)

type Part struct {
	Text      string         `json:"text,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	URL       string         `json:"url,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	MediaType string         `json:"mediaType,omitempty"`
}

type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type TaskStatus struct {
	State     TaskState `json:"state"`
	Timestamp string    `json:"timestamp,omitempty"`
}

type TaskStatusUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type TaskArtifactUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Artifact  Artifact       `json:"artifact"`
	Index     int            `json:"index"`
	Append    bool           `json:"append,omitempty"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// StreamResponse follows the A2A 1.0 HTTP/protobuf oneof JSON shape.
type StreamResponse struct {
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

type Context struct {
	ContextID string
	TaskID    string
}

type ArtifactResolver interface {
	ResolveA2AArtifact(context.Context, string) (Artifact, bool, error)
}

type Projector struct {
	Artifacts ArtifactResolver
}

func (p Projector) Project(ctx context.Context, fact eventlog.Event, scope Context) ([]StreamResponse, error) {
	if strings.TrimSpace(scope.ContextID) == "" {
		return nil, fmt.Errorf("A2A context ID is required")
	}
	taskID := scope.TaskID
	if taskID == "" && fact.AggregateType == "task" {
		taskID = fact.AggregateID
	}
	state, mapped := taskState(fact.Type)
	if mapped {
		if taskID == "" {
			return nil, fmt.Errorf("A2A task ID is required for %s", fact.Type)
		}
		return []StreamResponse{{StatusUpdate: &TaskStatusUpdateEvent{
			TaskID: taskID, ContextID: scope.ContextID,
			Status: TaskStatus{State: state, Timestamp: fact.Time.UTC().Format(time.RFC3339Nano)},
		}}}, nil
	}
	if fact.Type != "delivery.sent" || p.Artifacts == nil {
		return nil, nil
	}
	artifactID, _ := fact.Data["artifact_id"].(string)
	if artifactID == "" || taskID == "" {
		return nil, nil
	}
	artifact, found, err := p.Artifacts.ResolveA2AArtifact(ctx, artifactID)
	if err != nil || !found {
		return nil, err
	}
	return []StreamResponse{{ArtifactUpdate: &TaskArtifactUpdateEvent{
		TaskID: taskID, ContextID: scope.ContextID, Artifact: artifact, Index: 0, LastChunk: true,
	}}}, nil
}

func taskState(eventType string) (TaskState, bool) {
	switch eventType {
	case "task.created":
		return TaskSubmitted, true
	case "task.started", "task.resumed", "task.recovered":
		return TaskWorking, true
	case "task.waiting":
		return TaskInputRequired, true
	case "task.completed":
		return TaskCompleted, true
	case "task.failed":
		return TaskFailed, true
	case "task.canceled":
		return TaskCanceled, true
	default:
		return "", false
	}
}
