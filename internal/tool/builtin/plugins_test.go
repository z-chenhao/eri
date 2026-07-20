package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/z-chenhao/eri/internal/plugin"
	"github.com/z-chenhao/eri/internal/policy"
)

func TestPluginToolRequiresStrongApprovalForAllExecutableInstalls(t *testing.T) {
	manager := &fakePluginManager{plan: plugin.InstallPlan{Manifest: plugin.Manifest{ID: "calendar", Version: "0.1.0"}}}
	candidate, err := NewPlugins(manager)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"exact permission change", "requires strong approval", "Never claim the Plugin works"} {
		if !strings.Contains(candidate.Descriptor().Purpose, required) {
			t.Fatalf("Plugin tool description is missing %q", required)
		}
	}
	prepared, err := candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"install","manifest_path":"calendar.json"}`))
	if err != nil || prepared.Action.Effect != policy.Privileged {
		t.Fatalf("initial install=%+v err=%v", prepared, err)
	}
	manager.plan.PermissionExpanded = true
	prepared, err = candidate.Prepare(context.Background(), json.RawMessage(`{"operation":"install","manifest_path":"calendar.json"}`))
	if err != nil || prepared.Action.Effect != policy.Privileged {
		t.Fatalf("expanded install=%+v err=%v", prepared, err)
	}
}

type fakePluginManager struct{ plan plugin.InstallPlan }

func (m *fakePluginManager) PrepareInstall(context.Context, string) (plugin.InstallPlan, error) {
	return m.plan, nil
}
func (m *fakePluginManager) Install(context.Context, plugin.InstallPlan) (plugin.Record, error) {
	return plugin.Record{ID: "calendar", Version: "0.1.0"}, nil
}
func (m *fakePluginManager) List(context.Context) ([]plugin.Record, error) { return nil, nil }
