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

	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

type MemoryService interface {
	Capture(context.Context, memory.CaptureRequest) (memory.Entry, error)
	Recall(context.Context, memory.RecallRequest) (memory.Bundle, error)
	Inspect(context.Context, int) (memory.Bundle, error)
	InspectForRun(context.Context, memory.RecallRequest) (memory.Bundle, error)
	SetUsagePolicy(context.Context, string, string) error
	Delete(context.Context, string) (memory.DeletePlan, error)
	Export(context.Context) ([]byte, error)
}

type Memory struct {
	service MemoryService
	content interface {
		Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	}
}

type memoryInput struct {
	Operation    string `json:"operation"`
	Query        string `json:"query,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	MemoryID     string `json:"memory_id,omitempty"`
	ReplacesID   string `json:"replaces_memory_id,omitempty"`
	Statement    string `json:"statement,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExplicitUser bool   `json:"explicit_user_memory,omitempty"`
}

func NewMemory(service MemoryService, contentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
}) (*Memory, error) {
	if service == nil || contentStore == nil {
		return nil, fmt.Errorf("memory service and content store are required")
	}
	return &Memory{service: service, content: contentStore}, nil
}

func (m *Memory) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.memory", Version: "0.2.0",
		Purpose: "Manage user-owned long-term Memory. Record a directly stated stable fact, durable preference, relationship, or recurring constraint when it is likely to help later; the user does not need to say remember. For record, statement must be one exact useful excerpt from the current user message: never paraphrase or infer it. Set explicit_user_memory=true only when they explicitly ask. Never record transient task details, scheduled work, guesses, inferred emotion, generic knowledge, secrets, or duplicates. Use search or list before correcting an existing item, then record the new fact with replaces_memory_id set to the old memory_id; the old item remains archived for history. Use restrict, allow, forget, or export only when the user asks to manage their Memory.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"search", "list", "record", "restrict", "allow", "forget", "export"}},
				"query":     map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"},
				"memory_id":            map[string]any{"type": "string"},
				"replaces_memory_id":   map[string]any{"type": "string", "description": "Old memory_id returned by search or list when this new fact corrects or replaces it"},
				"statement":            map[string]any{"type": "string"},
				"kind":                 map[string]any{"type": "string", "enum": []string{"semantic", "preference", "relationship"}},
				"scope":                map[string]any{"type": "string"},
				"explicit_user_memory": map[string]any{"type": "boolean"},
			},
			"required": []string{"operation"}, "additionalProperties": false,
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Reversible, policy.Destructive},
		PermissionRequirements: []string{"local_memory"}, Timeout: 10 * time.Second,
		CostPolicy: "local_only", Idempotency: "evidence_key_or_gateway_key",
		Reconciliation: "inspect_memory_state", Source: tool.BuiltIn,
	}
}

func (m *Memory) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input memoryInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	action := policy.Action{Target: "memory"}
	switch input.Operation {
	case "search", "list", "export":
		action.Effect = policy.ReadOnly
	case "record":
		action.Effect = policy.Reversible
		if strings.TrimSpace(input.Statement) == "" {
			return tool.Prepared{}, fmt.Errorf("statement is required for record")
		}
	case "restrict", "allow":
		action.Effect = policy.Reversible
		action.Target = "memory:" + input.MemoryID
		if input.MemoryID == "" {
			return tool.Prepared{}, fmt.Errorf("memory_id is required")
		}
	case "forget":
		action.Effect = policy.Destructive
		action.Irreversible = true
		action.Target = "memory:" + input.MemoryID
		if input.MemoryID == "" {
			return tool.Prepared{}, fmt.Errorf("memory_id is required")
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported memory operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	prepared := tool.Prepared{Input: normalized, Action: action}
	switch input.Operation {
	case "search", "list":
		prepared.SourceBinding = tool.SourceBindingInteraction
	case "record":
		prepared.SourceBinding = tool.SourceBindingDirectUserExcerpt
		prepared.SourceExcerpt = input.Statement
	}
	return prepared, nil
}

func (m *Memory) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input memoryInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var attachments []tool.Attachment
	var err error
	switch input.Operation {
	case "search":
		if !prepared.SourceContextValidated || prepared.RunID == "" || prepared.SourceInteractionID == "" || prepared.InvocationID == "" {
			return tool.Result{}, fmt.Errorf("run, source interaction and invocation are required for Memory read provenance")
		}
		output, err = m.service.Recall(ctx, memory.RecallRequest{
			Query: input.Query, Limit: input.Limit, RunID: prepared.RunID, SourceInteractionID: prepared.SourceInteractionID,
			RetrievalID: prepared.InvocationID,
		})
	case "list":
		if !prepared.SourceContextValidated || prepared.RunID == "" || prepared.SourceInteractionID == "" || prepared.InvocationID == "" {
			return tool.Result{}, fmt.Errorf("run, source interaction and invocation are required for Memory read provenance")
		}
		output, err = m.service.InspectForRun(ctx, memory.RecallRequest{
			Limit: input.Limit, RunID: prepared.RunID, SourceInteractionID: prepared.SourceInteractionID,
			RetrievalID: prepared.InvocationID,
		})
	case "record":
		if !prepared.SourceContextValidated || prepared.SourceInteractionRole != "user" || prepared.SourceInteractionKind == "internal_trigger" {
			return tool.Result{}, fmt.Errorf("Memory record provenance must be a real user text interaction")
		}
		if strings.TrimSpace(prepared.SourceInteractionID) == "" {
			return tool.Result{}, fmt.Errorf("source interaction id is required for user memory provenance")
		}
		output, err = m.service.Capture(ctx, memory.CaptureRequest{
			ReplacesMemoryID: input.ReplacesID, Statement: input.Statement, Evidence: input.Statement,
			Kind: input.Kind, Scope: input.Scope, Relation: memory.Supports,
			SourceType: "user", SourceRef: "interaction:" + prepared.SourceInteractionID, IndependenceGroup: "user:self",
			DirectUserStatement: true, ExplicitUserMemory: input.ExplicitUser,
		})
	case "restrict":
		err = m.service.SetUsagePolicy(ctx, input.MemoryID, "do_not_use")
		output = map[string]any{"memory_id": input.MemoryID, "usage_policy": "do_not_use"}
	case "allow":
		err = m.service.SetUsagePolicy(ctx, input.MemoryID, "allow")
		output = map[string]any{"memory_id": input.MemoryID, "usage_policy": "allow"}
	case "forget":
		output, err = m.service.Delete(ctx, input.MemoryID)
	case "export":
		var body []byte
		body, err = m.service.Export(ctx)
		if err == nil {
			attachmentID, idErr := identifier.New()
			if idErr != nil {
				return tool.Result{}, idErr
			}
			ref, storeErr := m.content.Put(ctx, body, content.Metadata{
				MediaType: "application/json", EncryptionDomain: "attachment", PrivacyClass: "private",
				RetentionPolicy: "user_owned", ProvenanceRef: attachmentID,
			})
			if storeErr != nil {
				return tool.Result{}, storeErr
			}
			attachments = []tool.Attachment{{
				ID: attachmentID, Name: "eri-memory-export.json", MediaType: "application/json",
				SizeBytes: int64(len(body)), ContentRef: ref,
			}}
			output = map[string]any{"media_type": "application/json", "attachment_id": attachmentID, "size_bytes": len(body)}
		}
	}
	if err != nil {
		return tool.Result{}, err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(encoded)
	return tool.Result{Output: encoded, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC(), Attachments: attachments}, nil
}
