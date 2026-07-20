package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/skill"
	"github.com/z-chenhao/eri/internal/tool"
)

type SkillCatalog interface {
	Names(context.Context) ([]string, error)
	Activate(context.Context, string) (skill.Document, error)
	ReadResource(context.Context, string, string) ([]byte, error)
}

// Skills is a generic progressive-disclosure boundary. It loads standard
// Agent Skills selected by the model; it does not turn each skill into a tool.
type Skills struct {
	catalog SkillCatalog
	names   []string
}

type skillInput struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
}

func NewSkills(ctx context.Context, catalog SkillCatalog) (*Skills, error) {
	if catalog == nil {
		return nil, fmt.Errorf("skill catalog is required")
	}
	names, err := catalog.Names(ctx)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("skill catalog is empty")
	}
	sort.Strings(names)
	return &Skills{catalog: catalog, names: names}, nil
}

func (s *Skills) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.skills", Version: "0.1.0",
		Purpose: "Load the full instructions of an available Agent Skill after selecting it from the catalog, or read one referenced text resource from that skill.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"load", "read_resource"}},
				"name":      map[string]any{"type": "string", "enum": append([]string(nil), s.names...)},
				"path":      map[string]any{"type": "string", "description": "Relative resource path; required only for read_resource."},
			},
			"required": []string{"operation", "name"},
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly},
		PermissionRequirements: []string{"installed_agent_skills"},
		Timeout:                5 * time.Second,
		CostPolicy:             "local_only",
		Idempotency:            "content_read",
		Reconciliation:         "content_digest",
		Source:                 tool.BuiltIn,
	}
}

func (s *Skills) Prepare(ctx context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input skillInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	input.Name = strings.TrimSpace(input.Name)
	input.Path = strings.TrimSpace(input.Path)
	if !containsString(s.names, input.Name) {
		return tool.Prepared{}, fmt.Errorf("skill %q is not in the available catalog", input.Name)
	}
	switch input.Operation {
	case "load":
		if input.Path != "" {
			return tool.Prepared{}, fmt.Errorf("path is not allowed for load")
		}
		if _, err := s.catalog.Activate(ctx, input.Name); err != nil {
			return tool.Prepared{}, err
		}
	case "read_resource":
		if input.Path == "" {
			return tool.Prepared{}, fmt.Errorf("path is required for read_resource")
		}
		if _, err := s.catalog.ReadResource(ctx, input.Name, input.Path); err != nil {
			return tool.Prepared{}, err
		}
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{
		Input:  normalized,
		Action: policy.Action{Effect: policy.ReadOnly, Target: "skill://" + input.Name + "/" + input.Path},
	}, nil
}

func (s *Skills) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input skillInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output map[string]any
	switch input.Operation {
	case "load":
		document, err := s.catalog.Activate(ctx, input.Name)
		if err != nil {
			return tool.Result{}, err
		}
		output = map[string]any{
			"operation": "load", "name": document.Name,
			"content": skill.Render(document), "directory": document.Directory,
			"resources": document.Resources,
		}
	case "read_resource":
		body, err := s.catalog.ReadResource(ctx, input.Name, input.Path)
		if err != nil {
			return tool.Result{}, err
		}
		output = map[string]any{
			"operation": "read_resource", "name": input.Name, "path": input.Path,
			"content": string(body), "size_bytes": len(body),
		}
	default:
		return tool.Result{}, fmt.Errorf("unsupported operation %q", input.Operation)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(output); err != nil {
		return tool.Result{}, err
	}
	body := bytes.TrimSpace(encoded.Bytes())
	digest := sha256.Sum256(body)
	return tool.Result{
		Output: body, Receipt: "sha256:" + hex.EncodeToString(digest[:]), FreshAt: time.Now().UTC(),
	}, nil
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}
