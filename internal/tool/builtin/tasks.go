package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	assistanttask "github.com/z-chenhao/eri/internal/task"
	"github.com/z-chenhao/eri/internal/tool"
)

type TaskService interface {
	List(context.Context, int) ([]assistanttask.Record, error)
	Inspect(context.Context, string) (assistanttask.Record, error)
	Cancel(context.Context, string) (assistanttask.CancelResult, error)
	Retry(context.Context, string) (assistanttask.RetryResult, error)
}

type Tasks struct{ service TaskService }

type tasksInput struct {
	Operation string `json:"operation"`
	TaskID    string `json:"task_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func NewTasks(service TaskService) (*Tasks, error) {
	if service == nil {
		return nil, fmt.Errorf("task service is required")
	}
	return &Tasks{service: service}, nil
}

func (t *Tasks) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.tasks", Version: "0.1.0",
		Purpose: "Inspect Eri's durable tasks, request cancellation, or safely retry a failed/canceled task only when no side effect was dispatched. Cancellation never claims to undo confirmed side effects and retry never reuses approval.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"list", "inspect", "cancel", "retry"}},
				"task_id":   map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			}, "required": []string{"operation"},
		},
		OutputSchema: map[string]any{"type": "object"}, AllowedEffects: []policy.EffectClass{policy.ReadOnly, policy.Reversible},
		PermissionRequirements: []string{"local_runtime"}, Timeout: 10 * time.Second,
		CostPolicy: "local_only", Idempotency: "gateway_key", Reconciliation: "inspect_task_status", Source: tool.BuiltIn,
	}
}

func (t *Tasks) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input tasksInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	input.TaskID = strings.TrimSpace(input.TaskID)
	action := policy.Action{Effect: policy.ReadOnly, Target: "tasks"}
	switch input.Operation {
	case "list":
		if input.Limit < 0 || input.Limit > 100 {
			return tool.Prepared{}, fmt.Errorf("limit must be between 1 and 100")
		}
	case "inspect":
		if input.TaskID == "" {
			return tool.Prepared{}, fmt.Errorf("task_id is required")
		}
		action.Target = "task:" + input.TaskID
	case "cancel", "retry":
		if input.TaskID == "" {
			return tool.Prepared{}, fmt.Errorf("task_id is required")
		}
		action.Effect = policy.Reversible
		action.Target = "task:" + input.TaskID
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported task operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

func (t *Tasks) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input tasksInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var err error
	switch input.Operation {
	case "list":
		output, err = t.service.List(ctx, input.Limit)
	case "inspect":
		output, err = t.service.Inspect(ctx, input.TaskID)
	case "cancel":
		output, err = t.service.Cancel(ctx, input.TaskID)
	case "retry":
		output, err = t.service.Retry(ctx, input.TaskID)
	}
	if err != nil {
		return tool.Result{}, err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(encoded)
	return tool.Result{Output: encoded, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC()}, nil
}
