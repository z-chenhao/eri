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
	Retrieve(context.Context, string, int) (memory.Bundle, error)
	MarkUsed(context.Context, string, []string) error
	Inspect(context.Context, int) (memory.Bundle, error)
	Promote(context.Context, string) error
	Consolidate(context.Context) (memory.ConsolidationReport, error)
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
	Operation         string          `json:"operation"`
	Query             string          `json:"query,omitempty"`
	Limit             int             `json:"limit,omitempty"`
	MemoryID          string          `json:"memory_id,omitempty"`
	MemoryIDs         []string        `json:"memory_ids,omitempty"`
	RetrievalID       string          `json:"retrieval_id,omitempty"`
	ClaimID           string          `json:"claim_id,omitempty"`
	Statement         string          `json:"statement,omitempty"`
	Evidence          string          `json:"evidence,omitempty"`
	Kind              string          `json:"kind,omitempty"`
	Scope             string          `json:"scope,omitempty"`
	Relation          memory.Relation `json:"relation,omitempty"`
	SourceType        string          `json:"source_type,omitempty"`
	SourceRef         string          `json:"source_ref,omitempty"`
	IndependenceGroup string          `json:"independence_group,omitempty"`
	Reliability       float64         `json:"reliability,omitempty"`
	Directness        float64         `json:"directness,omitempty"`
	Verifiability     float64         `json:"verifiability,omitempty"`
	ObservedAt        string          `json:"observed_at,omitempty"`
	ValidUntil        string          `json:"valid_until,omitempty"`
	DirectUser        bool            `json:"direct_user_statement,omitempty"`
	ExplicitUser      bool            `json:"explicit_user_memory,omitempty"`
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
		ID: "builtin.memory", Version: "0.1.0",
		Purpose: "Recall, inspect, record, promote, consolidate, restrict, export or forget one item in Eri's provenance-aware memory. Use it when the user explicitly asks to remember, correct, inspect, restrict, export or forget a memory. Also record a directly stated stable fact, durable preference, relationship, recurring constraint or prospective commitment when it will help later; set direct_user_statement=true, and set explicit_user_memory=true only for an explicit remember request. To correct or qualify an existing memory, first recall or inspect it, then record the new statement with the exact returned claim_id and relation=contradicts or relation=qualifies; never overwrite history. Never record transient task detail, guesses, inferred emotion, generic knowledge, secrets or a duplicate of the conversation.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"recall", "mark_used", "inspect", "record", "promote", "consolidate", "do_not_use", "allow", "forget", "export"}},
				"query":     map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"},
				"memory_id": map[string]any{"type": "string"}, "memory_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"retrieval_id": map[string]any{"type": "string"}, "claim_id": map[string]any{"type": "string", "description": "Exact claim_id returned by recall or inspect when correcting or qualifying an existing memory."},
				"statement": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"},
				"kind":        map[string]any{"type": "string", "enum": []string{"episodic", "semantic", "preference", "relationship", "prospective", "procedural"}},
				"scope":       map[string]any{"type": "string"},
				"relation":    map[string]any{"type": "string", "enum": []string{"supports", "contradicts", "qualifies"}, "description": "Use contradicts or qualifies with the existing claim_id for a correction; never silently overwrite a claim."},
				"source_type": map[string]any{"type": "string"}, "source_ref": map[string]any{"type": "string"},
				"independence_group": map[string]any{"type": "string"},
				"reliability":        map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"directness":         map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"verifiability":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"observed_at":        map[string]any{"type": "string"}, "valid_until": map[string]any{"type": "string"},
				"direct_user_statement": map[string]any{"type": "boolean"},
				"explicit_user_memory":  map[string]any{"type": "boolean"},
			},
			"required": []string{"operation"},
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
	case "recall", "inspect", "consolidate", "export":
		action.Effect = policy.ReadOnly
	case "record":
		action.Effect = policy.Reversible
		if strings.TrimSpace(input.Statement) == "" {
			return tool.Prepared{}, fmt.Errorf("statement is required for record")
		}
		if input.Relation == "" {
			input.Relation = memory.Supports
		}
		if input.DirectUser || input.ExplicitUser {
			if input.SourceType == "" {
				input.SourceType = "user"
			}
			if input.SourceRef == "" {
				input.SourceRef = "current_user_statement"
			}
			if input.IndependenceGroup == "" {
				input.IndependenceGroup = "user:self"
			}
		}
		if input.SourceType == "" || input.SourceRef == "" || input.IndependenceGroup == "" {
			return tool.Prepared{}, fmt.Errorf("source_type, source_ref and independence_group are required")
		}
	case "mark_used":
		action.Effect = policy.Reversible
		action.Target = "memory-retrieval:" + input.RetrievalID
		if input.RetrievalID == "" || len(input.MemoryIDs) == 0 {
			return tool.Prepared{}, fmt.Errorf("retrieval_id and memory_ids are required")
		}
	case "promote", "do_not_use", "allow":
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
	return tool.Prepared{Input: normalized, Action: action}, nil
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
	case "recall":
		output, err = m.service.Retrieve(ctx, input.Query, input.Limit)
	case "mark_used":
		err = m.service.MarkUsed(ctx, input.RetrievalID, input.MemoryIDs)
		output = map[string]any{"retrieval_id": input.RetrievalID, "memory_ids": input.MemoryIDs, "recorded": err == nil}
	case "inspect":
		output, err = m.service.Inspect(ctx, input.Limit)
	case "record":
		if input.SourceRef == "current_user_statement" {
			if strings.TrimSpace(prepared.TaskID) == "" {
				return tool.Result{}, fmt.Errorf("task id is required for explicit user memory provenance")
			}
			input.SourceRef = "task:" + prepared.TaskID
		}
		observedAt, parseErr := optionalTime(input.ObservedAt)
		if parseErr != nil {
			return tool.Result{}, parseErr
		}
		validUntil, parseErr := optionalTime(input.ValidUntil)
		if parseErr != nil {
			return tool.Result{}, parseErr
		}
		output, err = m.service.Capture(ctx, memory.CaptureRequest{
			ClaimID: input.ClaimID, Statement: input.Statement, Evidence: input.Evidence,
			Kind: input.Kind, Scope: input.Scope, Relation: input.Relation,
			SourceType: input.SourceType, SourceRef: input.SourceRef, IndependenceGroup: input.IndependenceGroup,
			Reliability: input.Reliability, Directness: input.Directness, Verifiability: input.Verifiability,
			ObservedAt: observedAt, ValidUntil: validUntil, DirectUserStatement: input.DirectUser, ExplicitUserMemory: input.ExplicitUser,
		})
	case "promote":
		err = m.service.Promote(ctx, input.MemoryID)
		output = map[string]any{"memory_id": input.MemoryID, "lifecycle_status": "active", "pinned": true}
	case "consolidate":
		output, err = m.service.Consolidate(ctx)
	case "do_not_use":
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

func optionalTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("time must use RFC3339: %w", err)
	}
	return parsed, nil
}
