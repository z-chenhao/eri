package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/subagent"
	"github.com/z-chenhao/eri/internal/tool"
)

type SubagentRegistry interface {
	Descriptors() []subagent.RoleDescriptor
	Roles() []string
	AccessModes() []string
	RoutingGuide() string
	Prepare(context.Context, subagent.Request) (subagent.Request, policy.Action, error)
	Invoke(context.Context, subagent.Request) (subagent.Outcome, error)
	Inspect(context.Context, string, string, string) (subagent.Inspection, error)
}

type Delegate struct {
	registry SubagentRegistry
}

type delegateInput struct {
	Objective  string              `json:"objective"`
	Context    string              `json:"context,omitempty"`
	Assignee   string              `json:"assignee"`
	ProviderID string              `json:"provider_id,omitempty"`
	Access     subagent.AccessMode `json:"access,omitempty"`
}

func NewDelegate(registry SubagentRegistry) (*Delegate, error) {
	if registry == nil || len(registry.Descriptors()) == 0 {
		return nil, fmt.Errorf("subagent registry is required")
	}
	return &Delegate{registry: registry}, nil
}

func (d *Delegate) Descriptor() tool.Descriptor {
	routing := d.registry.RoutingGuide()
	return tool.Descriptor{
		ID: "builtin.delegate", Version: "0.4.0",
		Purpose: "Assign one bounded objective to an available colleague. Choose the assignee from the job descriptions below and only the workspace authority the assignment needs. The colleague continues independently; give the user a truthful progress update, then wait for the completed work to return. You remain responsible for reviewing the work and answering the user. Available colleagues: " + routing,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"objective": map[string]any{"type": "string", "minLength": 1, "maxLength": 32768},
				"context":   map[string]any{"type": "string", "maxLength": 65536},
				"assignee": map[string]any{
					"type": "string", "enum": d.registry.Roles(), "description": routing,
				},
				"access": map[string]any{
					"type": "string", "enum": d.registry.AccessModes(),
					"description": "Requested authority. The selected subagent descriptor may further restrict it.",
				},
			}, "required": []string{"objective", "assignee"}, "additionalProperties": false,
		},
		OutputSchema:           subagentOutputSchema(),
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Reversible},
		PermissionRequirements: []string{"registered_subagent"}, Timeout: 15 * time.Minute,
		// External-data handling is selected per provider in Prepare. Marking the
		// whole Tool external would incorrectly classify native invocations.
		SendsDataExternally: false,
		CostPolicy:          "subagent_descriptor_budget", Idempotency: "gateway_key", Reconciliation: "inspect_selected_subagent", Source: tool.BuiltIn,
	}
}

func subagentOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"oneOf": []any{
			map[string]any{
				"type": "object", "properties": map[string]any{
					"delegation_id": map[string]any{"type": "string"}, "assignee": map[string]any{"type": "string"},
					"status": map[string]any{"type": "string"},
					"access": map[string]any{"type": "string"},
				}, "required": []string{"delegation_id", "assignee", "status", "access"}, "additionalProperties": false,
			},
			map[string]any{
				"type": "object", "properties": map[string]any{
					"delegation_id": map[string]any{"type": "string"}, "assignee": map[string]any{"type": "string"},
					"status": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"},
					"evidence": stringArray, "changes": stringArray, "tests": stringArray, "remaining_risks": stringArray,
					"error_code": map[string]any{"type": "string"},
				}, "required": []string{"delegation_id", "assignee", "status", "summary", "evidence", "changes", "tests", "remaining_risks"}, "additionalProperties": false,
			},
		},
	}
}

func (d *Delegate) Prepare(ctx context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input delegateInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	request, action, err := d.registry.Prepare(ctx, subagent.Request{
		Objective: input.Objective, Context: input.Context, RoleID: input.Assignee, Access: input.Access,
	})
	if err != nil {
		return tool.Prepared{}, err
	}
	normalized, err := json.Marshal(delegateInput{
		Objective: request.Objective, Context: request.Context, Assignee: request.RoleID,
		ProviderID: request.ProviderID, Access: request.Access,
	})
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

func (d *Delegate) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input delegateInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	outcome, err := d.registry.Invoke(ctx, subagent.Request{
		DelegationID: prepared.InvocationID, TaskID: prepared.TaskID, RunID: prepared.RunID,
		Objective: input.Objective, Context: input.Context, RoleID: input.Assignee,
		ProviderID: input.ProviderID, Access: input.Access,
	})
	if err != nil {
		return tool.Result{}, err
	}
	payload, err := outcome.Payload()
	if err != nil {
		return tool.Result{}, err
	}
	result := tool.Result{
		Output: payload, ExternalObjectID: outcome.ExternalObjectID,
		Receipt: outcome.Receipt, FreshAt: outcome.FreshAt,
	}
	if outcome.Deferred {
		result.Deferred = &tool.Deferred{ID: prepared.InvocationID, Kind: "subagent", Type: input.Assignee, ProviderID: input.ProviderID}
	}
	return result, nil
}

func (d *Delegate) Reconcile(ctx context.Context, request tool.ReconcileRequest) (tool.ReconcileResult, error) {
	var input delegateInput
	if err := json.Unmarshal(request.Payload, &input); err != nil {
		return tool.ReconcileResult{Status: tool.IntentFailed, ErrorCode: "subagent_payload_invalid"}, nil
	}
	inspection, err := d.registry.Inspect(ctx, strings.TrimSpace(input.Assignee), strings.TrimSpace(input.ProviderID), request.Intent.ID)
	if err != nil {
		return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: "subagent_inspection_failed", Retry: inspection.Retry}, err
	}
	switch inspection.Status {
	case subagent.InspectionConfirmed:
		payload, err := inspection.Outcome.Payload()
		if err != nil {
			return tool.ReconcileResult{}, err
		}
		result := tool.Result{
			Output: payload, ExternalObjectID: inspection.Outcome.ExternalObjectID,
			Receipt: inspection.Outcome.Receipt, FreshAt: inspection.Outcome.FreshAt,
		}
		if inspection.Outcome.Deferred {
			result.Deferred = &tool.Deferred{ID: request.Intent.ID, Kind: "subagent", Type: input.Assignee, ProviderID: input.ProviderID}
		}
		return tool.ReconcileResult{Status: tool.IntentConfirmed, Result: result}, nil
	case subagent.InspectionFailed:
		return tool.ReconcileResult{Status: tool.IntentFailed, ErrorCode: inspection.ErrorCode, Retry: inspection.Retry}, nil
	default:
		return tool.ReconcileResult{Status: tool.IntentUnknown, ErrorCode: inspection.ErrorCode, Retry: inspection.Retry}, nil
	}
}
