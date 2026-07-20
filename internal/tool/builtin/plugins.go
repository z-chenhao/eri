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

	"github.com/z-chenhao/eri/internal/plugin"
	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/tool"
)

type PluginManager interface {
	PrepareInstall(context.Context, string) (plugin.InstallPlan, error)
	Install(context.Context, plugin.InstallPlan) (plugin.Record, error)
	List(context.Context) ([]plugin.Record, error)
}

type Plugins struct{ manager PluginManager }

type pluginInput struct {
	Operation    string              `json:"operation"`
	ManifestPath string              `json:"manifest_path,omitempty"`
	InstallPlan  *plugin.InstallPlan `json:"install_plan,omitempty"`
}

func NewPlugins(manager PluginManager) (*Plugins, error) {
	if manager == nil {
		return nil, fmt.Errorf("plugin manager is required")
	}
	return &Plugins{manager: manager}, nil
}

func (p *Plugins) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		ID: "builtin.plugins", Version: "0.1.0",
		Purpose: "Inspect installed Eri extensions or install/upgrade one local Eri Plugin manifest when an external capability is genuinely missing. Explain the executable source, human capability and exact permission change, not MCP jargon. Every install or upgrade requires strong approval while Plugins run as trusted local code. Never claim the Plugin works until its out-of-process host starts and returns discovered tools.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"operation":     map[string]any{"type": "string", "enum": []string{"list", "install"}},
				"manifest_path": map[string]any{"type": "string"},
			}, "required": []string{"operation"}, "additionalProperties": false,
		},
		OutputSchema:           map[string]any{"type": "object"},
		AllowedEffects:         []policy.EffectClass{policy.ReadOnly, policy.Reversible, policy.Privileged},
		PermissionRequirements: []string{"local_plugin_management"}, Timeout: 90 * time.Second,
		CostPolicy: "local_process", Idempotency: "manifest_hash_and_gateway_key", Reconciliation: "inspect_active_plugin_manifest_and_health",
		Source: tool.BuiltIn,
	}
}

func (p *Plugins) Prepare(ctx context.Context, raw json.RawMessage) (tool.Prepared, error) {
	var input pluginInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return tool.Prepared{}, err
	}
	input.Operation = strings.TrimSpace(input.Operation)
	action := policy.Action{Target: "plugins"}
	switch input.Operation {
	case "list":
		action.Effect = policy.ReadOnly
		input.ManifestPath = ""
	case "install":
		plan, err := p.manager.PrepareInstall(ctx, input.ManifestPath)
		if err != nil {
			return tool.Prepared{}, err
		}
		input.InstallPlan = &plan
		action.Target = "plugin:" + plan.Manifest.ID + "@" + plan.Manifest.Version
		// An MCP plugin is executable code running with the current OS user's
		// authority. Until the macOS sandbox boundary exists, every install or
		// upgrade is privileged even when its declared Eri permissions did not
		// expand.
		action.Effect = policy.Privileged
	default:
		return tool.Prepared{}, fmt.Errorf("unsupported plugin operation %q", input.Operation)
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return tool.Prepared{}, err
	}
	return tool.Prepared{Input: normalized, Action: action}, nil
}

func (p *Plugins) Execute(ctx context.Context, prepared tool.Prepared) (tool.Result, error) {
	var input pluginInput
	if err := json.Unmarshal(prepared.Input, &input); err != nil {
		return tool.Result{}, err
	}
	var output any
	var err error
	switch input.Operation {
	case "list":
		output, err = p.manager.List(ctx)
	case "install":
		if input.InstallPlan == nil {
			return tool.Result{}, fmt.Errorf("approved install plan is missing")
		}
		output, err = p.manager.Install(ctx, *input.InstallPlan)
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
