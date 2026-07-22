package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/scheduler"
	"github.com/z-chenhao/eri/internal/tool"
)

type SchedulerService interface {
	Create(context.Context, string, scheduler.CreateRequest) (scheduler.Commitment, error)
	Update(context.Context, string, string, scheduler.CreateRequest) (scheduler.Commitment, error)
	List(context.Context, int) ([]scheduler.Commitment, error)
	SetStatus(context.Context, string, string) error
}

type Scheduler struct {
	service SchedulerService
}

type schedulerInput struct {
	Operation     string             `json:"operation"`
	ID            string             `json:"id,omitempty"`
	Task          string             `json:"task,omitempty"`
	Schedule      scheduler.Schedule `json:"schedule,omitempty"`
	Importance    string             `json:"importance,omitempty"`
	DeliveryRoute string             `json:"delivery_route,omitempty"`
	Limit         int                `json:"limit,omitempty"`
}

func NewScheduler(service SchedulerService) (*Scheduler, error) {
	if service == nil {
		return nil, fmt.Errorf("scheduler service is required")
	}
	return &Scheduler{service: service}, nil
}

func (s *Scheduler) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.commitments", Version: "0.5.0",
		Purpose: "Schedule, update, list, pause, resume, or cancel a durable reminder or recurring assignment. Store what Eri must do when the schedule fires, not wording to send later; Eri will compose the response from that task and the current conversation. Use the returned id when the user refines an existing schedule. Use origin_channel for a user-requested reminder and recent_channel only for ongoing proactive work first proposed by Eri and accepted by the user.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation":  map[string]any{"type": "string", "enum": []string{"create", "update", "list", "pause", "resume", "cancel"}},
				"id":         map[string]any{"type": "string"},
				"task":       map[string]any{"type": "string", "maxLength": 16384, "description": "The assignment to carry out when due, not a prewritten future reply"},
				"importance": map[string]any{"type": "string", "enum": []string{"normal", "important"}},
				"delivery_route": map[string]any{
					"type": "string", "enum": []string{scheduler.DeliveryRouteOrigin, scheduler.DeliveryRouteRecent},
					"description": "origin_channel for a user-requested reminder; recent_channel for Eri-proposed proactive work accepted by the user",
				},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
				"schedule": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":             map[string]any{"type": "string", "enum": []string{"once", "interval", "daily"}},
						"at":               map[string]any{"type": "string", "description": "RFC3339 timestamp for once"},
						"after_seconds":    map[string]any{"type": "integer", "minimum": 1, "description": "Runtime-relative delay for once; prefer this for requests such as in one minute"},
						"interval_seconds": map[string]any{"type": "integer", "minimum": 60},
						"daily_time":       map[string]any{"type": "string", "description": "HH:MM"},
						"timezone":         map[string]any{"type": "string", "description": "IANA timezone name"},
					},
				},
			},
			"required": []string{"operation"},
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Reversible},
		PermissionRequirements: []string{"local_scheduler"}, Timeout: 10 * time.Second,
		CostPolicy: "background_tasks_use_runtime_budget", Idempotency: "gateway_key_and_commitment_fire",
		Reconciliation: "inspect_commitment_and_fire", Source: tool.BuiltIn,
	}
}

func (s *Scheduler) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input schedulerInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	action := policy.Action{Target: "commitments", Effect: policy.ReadOnly}
	switch input.Operation {
	case "list":
	case "create", "update":
		action.Effect = policy.Reversible
		if input.Task == "" || input.Schedule.Type == "" {
			return tool.Prepared{}, fmt.Errorf("task and schedule are required")
		}
		if input.Operation == "update" {
			action.Target = "commitment:" + input.ID
			if input.ID == "" {
				return tool.Prepared{}, fmt.Errorf("id is required")
			}
			if input.Importance == "" || input.DeliveryRoute == "" {
				return tool.Prepared{}, fmt.Errorf("importance and delivery_route are required for update")
			}
		}
	case "pause", "resume", "cancel":
		action.Effect = policy.Reversible
		action.Target = "commitment:" + input.ID
		if input.ID == "" {
			return tool.Prepared{}, fmt.Errorf("id is required")
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported schedule operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

func (s *Scheduler) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input schedulerInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var err error
	switch input.Operation {
	case "create":
		output, err = s.service.Create(ctx, prepared.TaskID, scheduler.CreateRequest{
			Task: input.Task, Schedule: input.Schedule, Importance: input.Importance, DeliveryRoute: input.DeliveryRoute,
		})
	case "update":
		output, err = s.service.Update(ctx, prepared.TaskID, input.ID, scheduler.CreateRequest{
			Task: input.Task, Schedule: input.Schedule, Importance: input.Importance, DeliveryRoute: input.DeliveryRoute,
		})
	case "list":
		output, err = s.service.List(ctx, input.Limit)
	case "pause":
		err = s.service.SetStatus(ctx, input.ID, "paused")
		output = map[string]any{"id": input.ID, "status": "paused"}
	case "resume":
		err = s.service.SetStatus(ctx, input.ID, "active")
		output = map[string]any{"id": input.ID, "status": "active"}
	case "cancel":
		err = s.service.SetStatus(ctx, input.ID, "canceled")
		output = map[string]any{"id": input.ID, "status": "canceled"}
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
