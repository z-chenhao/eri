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
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
	"github.com/z-chenhao/eri/internal/userdata"
)

type UserDataService interface {
	Export(context.Context) ([]byte, error)
	Schedule(context.Context, string) (userdata.ErasureJob, error)
}

type UserData struct {
	service UserDataService
	content interface {
		Put(context.Context, []byte, content.Metadata) (content.Ref, error)
	}
}

type userDataInput struct {
	Operation string `json:"operation"`
}

func NewUserData(service UserDataService, contentStore interface {
	Put(context.Context, []byte, content.Metadata) (content.Ref, error)
}) (*UserData, error) {
	if service == nil || contentStore == nil {
		return nil, fmt.Errorf("user data service and content store are required")
	}
	return &UserData{service: service, content: contentStore}, nil
}

func (d *UserData) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.user_data", Version: "0.1.0",
		Purpose: "Export all user-owned Eri data as a portable ZIP, or permanently erase all user content and derived data after strong approval. Use this for broad requests such as 'export/delete all my data'; use the memory tool for one memory. Full deletion is irreversible. After approval, this Tool schedules the deletion request now in an awaiting-delivery state; actual erasure begins only after the local channel accepts the final content-free confirmation delivery. State that sequence plainly.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"export", "delete_all"}},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Destructive},
		PermissionRequirements: []string{"local_user_data"}, Timeout: 2 * time.Minute,
		CostPolicy: "local_only", Idempotency: "gateway_key", Reconciliation: "inspect_data_erasure_job",
		Source: tool.BuiltIn,
	}
}

func (d *UserData) Prepare(_ context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input userDataInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	action := policy.Action{Target: "user_data:all"}
	switch input.Operation {
	case "export":
		action.Effect = policy.ReadOnly
	case "delete_all":
		action.Effect = policy.Destructive
		action.Irreversible = true
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported user data operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

func (d *UserData) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input userDataInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var attachments []tool.Attachment
	switch input.Operation {
	case "export":
		body, err := d.service.Export(ctx)
		if err != nil {
			return tool.Result{}, err
		}
		attachmentID, err := identifier.New()
		if err != nil {
			return tool.Result{}, err
		}
		ref, err := d.content.Put(ctx, body, content.Metadata{
			MediaType: "application/zip", EncryptionDomain: "attachment", PrivacyClass: "private",
			RetentionPolicy: "user_owned", ProvenanceRef: attachmentID,
		})
		if err != nil {
			return tool.Result{}, err
		}
		attachments = []tool.Attachment{{
			ID: attachmentID, Name: "eri-user-data-export.zip", MediaType: "application/zip",
			SizeBytes: int64(len(body)), ContentRef: ref,
		}}
		output = map[string]any{
			"format": userdata.ExportFormat, "attachment_id": attachmentID,
			"size_bytes": len(body), "scope": "all_user_owned_data",
		}
	case "delete_all":
		job, err := d.service.Schedule(ctx, prepared.TaskID)
		if err != nil {
			return tool.Result{}, err
		}
		output = map[string]any{
			"erasure_job_id": job.ID,
			"status":         job.Status,
			"scope":          "all_user_content_and_derived_data",
			"next":           "erasure begins after this task's final confirmation is accepted by the local channel",
		}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return tool.Result{}, err
	}
	digest := sha256.Sum256(encoded)
	return tool.Result{
		Output: encoded, Receipt: "sha256:" + hex.EncodeToString(digest[:]),
		FreshAt: time.Now().UTC(), Attachments: attachments,
	}, nil
}
