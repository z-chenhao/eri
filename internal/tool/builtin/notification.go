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

	"github.com/z-chenhao/eri/internal/notification"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

type Notification struct {
	sender notification.Sender
}

type notificationInput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func NewNotification(sender notification.Sender) (*Notification, error) {
	if sender == nil {
		return nil, fmt.Errorf("notification sender is required")
	}
	return &Notification{sender: sender}, nil
}

func (n *Notification) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.notification", Version: "0.1.0",
		Purpose: "Display one important local operating-system notification to Eri's owner. Do not use for Working status or routine chat replies.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"},
			}, "required": []string{"title", "body"},
		},
		OutputSchema:   map[string]any{"type": "object"},
		AllowedEffects: []policy.EffectClass{policy.Reversible}, PermissionRequirements: []string{"local_notifications"},
		Timeout: 6 * time.Second, CostPolicy: "local_only", Idempotency: "gateway_key",
		Reconciliation: "operating_system_acceptance_only", Source: tool.BuiltIn,
	}
}

func (n *Notification) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input notificationInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Body = strings.TrimSpace(input.Body)
	if input.Title == "" || input.Body == "" {
		return tool.Prepared{}, fmt.Errorf("notification title and body are required")
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: policy.Action{Effect: policy.Reversible, Target: "local_user_notification"}}, nil
}

func (n *Notification) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input notificationInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	receipt, err := n.sender.Send(ctx, input.Title, input.Body)
	if err != nil {
		return tool.Result{}, err
	}
	output, err := json.Marshal(map[string]any{"accepted": true, "receipt": receipt})
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(output)
	return tool.Result{Output: output, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC()}, nil
}
