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
	CommitmentID  string             `json:"commitment_id,omitempty"`
	Message       string             `json:"message,omitempty"`
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
		ID: "builtin.commitments", Version: "0.3.0",
		Purpose: "Create, update, inspect, pause, resume or cancel durable reminders and recurring commitments. A due commitment wakes the same Eri Agent Loop; do not create one unless the user has agreed to the ongoing work. When the user refines or corrects a commitment just created, update that commitment_id instead of creating an overlapping commitment. Use origin_channel when the user explicitly asks in the current conversation for a reminder. Use recent_channel for ongoing proactive work first proposed by Eri and then accepted by the user, so each future delivery follows the user's latest trusted conversation channel.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation":     map[string]any{"type": "string", "enum": []string{"create", "update", "list", "pause", "resume", "cancel"}},
				"commitment_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
				"importance": map[string]any{"type": "string", "enum": []string{"normal", "important"}},
				"delivery_route": map[string]any{
					"type": "string", "enum": []string{scheduler.DeliveryRouteOrigin, scheduler.DeliveryRouteRecent},
					"description": "origin_channel for a user-requested reminder; recent_channel for Eri-proposed proactive work accepted by the user",
				},
				"limit": map[string]any{"type": "integer"},
				"schedule": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":             map[string]any{"type": "string", "enum": []string{"once", "interval", "daily"}},
						"at":               map[string]any{"type": "string", "description": "RFC3339 timestamp for once"},
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
		if input.Message == "" || input.Schedule.Type == "" {
			return tool.Prepared{}, fmt.Errorf("message and schedule are required")
		}
		if input.Operation == "update" {
			action.Target = "commitment:" + input.CommitmentID
			if input.CommitmentID == "" {
				return tool.Prepared{}, fmt.Errorf("commitment_id is required")
			}
			if input.Importance == "" || input.DeliveryRoute == "" {
				return tool.Prepared{}, fmt.Errorf("importance and delivery_route are required for update")
			}
		}
	case "pause", "resume", "cancel":
		action.Effect = policy.Reversible
		action.Target = "commitment:" + input.CommitmentID
		if input.CommitmentID == "" {
			return tool.Prepared{}, fmt.Errorf("commitment_id is required")
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported commitment operation %q", input.Operation)
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
			Message: input.Message, Schedule: input.Schedule, Importance: input.Importance, DeliveryRoute: input.DeliveryRoute,
		})
	case "update":
		output, err = s.service.Update(ctx, prepared.TaskID, input.CommitmentID, scheduler.CreateRequest{
			Message: input.Message, Schedule: input.Schedule, Importance: input.Importance, DeliveryRoute: input.DeliveryRoute,
		})
	case "list":
		output, err = s.service.List(ctx, input.Limit)
	case "pause":
		err = s.service.SetStatus(ctx, input.CommitmentID, "paused")
		output = map[string]any{"commitment_id": input.CommitmentID, "status": "paused"}
	case "resume":
		err = s.service.SetStatus(ctx, input.CommitmentID, "active")
		output = map[string]any{"commitment_id": input.CommitmentID, "status": "active"}
	case "cancel":
		err = s.service.SetStatus(ctx, input.CommitmentID, "canceled")
		output = map[string]any{"commitment_id": input.CommitmentID, "status": "canceled"}
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
