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

	"github.com/z-chenhao/eri/internal/feedback"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

type FeedbackService interface {
	Capture(context.Context, string, feedback.Kind, feedback.OutcomeStatus, string, string) (feedback.Record, error)
}

type Feedback struct {
	service FeedbackService
}

type feedbackInput struct {
	Kind       feedback.Kind          `json:"kind"`
	Outcome    feedback.OutcomeStatus `json:"outcome,omitempty"`
	Statement  string                 `json:"statement"`
	DeliveryID string                 `json:"delivery_id,omitempty"`
}

func NewFeedback(service FeedbackService) (*Feedback, error) {
	if service == nil {
		return nil, fmt.Errorf("feedback service is required")
	}
	return &Feedback{service: service}, nil
}

func (f *Feedback) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.feedback", Version: "0.1.0",
		Purpose: "Record explicit user correction, acceptance, rejection, or real-world outcome for a previously delivered Eri answer. This Tool Call is required before acknowledging clear post-delivery feedback in text; a verbal promise to improve is not a feedback record. Omit delivery_id to link the latest prior delivered answer. When the feedback also states a durable personal preference, record that preference separately with memory.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":        map[string]any{"type": "string", "enum": []string{"correction", "accepted", "rejected", "outcome"}},
				"outcome":     map[string]any{"type": "string", "enum": []string{"success", "failure", "mixed", "unknown"}},
				"statement":   map[string]any{"type": "string"},
				"delivery_id": map[string]any{"type": "string"},
			},
			"required": []string{"kind", "statement"}, "additionalProperties": false,
		},
		OutputSchema:   map[string]any{"type": "object"},
		AllowedEffects: []policy.EffectClass{policy.Reversible}, PermissionRequirements: []string{"local_feedback"},
		Timeout: 10 * time.Second, CostPolicy: "local_only", Idempotency: "gateway_key",
		Reconciliation: "inspect_feedback_record", Source: tool.BuiltIn,
	}
}

func (f *Feedback) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input feedbackInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Statement = strings.TrimSpace(input.Statement)
	input.DeliveryID = strings.TrimSpace(input.DeliveryID)
	if input.Statement == "" {
		return tool.Prepared{}, fmt.Errorf("statement is required")
	}
	switch input.Kind {
	case feedback.Correction, feedback.Accepted, feedback.Rejected:
		if input.Outcome != "" {
			return tool.Prepared{}, fmt.Errorf("outcome is only valid for outcome feedback")
		}
	case feedback.Outcome:
		switch input.Outcome {
		case feedback.OutcomeSuccess, feedback.OutcomeFailure, feedback.OutcomeMixed, feedback.OutcomeUnknown:
		default:
			return tool.Prepared{}, fmt.Errorf("outcome feedback requires success, failure, mixed or unknown")
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported feedback kind %q", input.Kind)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	target := "delivery:latest"
	if input.DeliveryID != "" {
		target = "delivery:" + input.DeliveryID
	}
	return tool.Prepared{Input: normalized, Action: policy.Action{Effect: policy.Reversible, Target: target}}, nil
}

func (f *Feedback) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input feedbackInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	record, err := f.service.Capture(ctx, prepared.TaskID, input.Kind, input.Outcome, input.Statement, input.DeliveryID)
	if err != nil {
		return tool.Result{}, err
	}
	output, err := json.Marshal(map[string]any{
		"feedback_id": record.ID, "feedback_task_id": record.FeedbackTaskID,
		"source_task_id": record.SourceTaskID, "artifact_id": record.ArtifactID,
		"delivery_id": record.DeliveryID, "kind": record.Kind, "outcome": record.Outcome,
		"dataset_policy": "corrective feedback episode becomes a candidate after delivery; formal eval-set admission still requires authorization",
	})
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(output)
	return tool.Result{Output: output, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC()}, nil
}
