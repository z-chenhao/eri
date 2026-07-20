package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/z-chenhao/eri/internal/policy"
	"github.com/z-chenhao/eri/internal/subagent"
)

type fakeSubagentProvider struct {
	descriptor subagent.ProviderDescriptor
	request    subagent.Request
}

func (f *fakeSubagentProvider) Descriptor() subagent.ProviderDescriptor { return f.descriptor }
func (f *fakeSubagentProvider) Prepare(_ context.Context, request subagent.Request) (subagent.Request, policy.Action, error) {
	if request.Access == "" {
		request.Access = f.descriptor.DefaultAccess
	}
	action := policy.Action{Effect: policy.ReadOnly, Target: "provider:" + f.descriptor.ID + ":" + string(request.Access), SendsDataExternally: f.descriptor.SendsDataExternally}
	if request.Access == subagent.WorkspaceWrite {
		action.Effect = policy.Reversible
		action.OverwritesExisting = true
	}
	return request, action, nil
}
func (f *fakeSubagentProvider) Invoke(_ context.Context, request subagent.Request) (subagent.Outcome, error) {
	f.request = request
	ticket := &subagent.Ticket{
		DelegationID: request.DelegationID, RoleID: request.RoleID, ProviderID: request.ProviderID,
		Status: "queued", Execution: subagent.Background, Access: request.Access,
	}
	return subagent.Outcome{
		Ticket: ticket, ExternalObjectID: request.DelegationID, Receipt: "done:" + request.DelegationID,
		FreshAt: time.Now().UTC(), Deferred: true,
	}, nil
}
func (f *fakeSubagentProvider) Inspect(context.Context, string) (subagent.Inspection, error) {
	return subagent.Inspection{Status: subagent.InspectionUnknown}, nil
}

func fakeProviderDescriptor(id string, external bool, access ...subagent.AccessMode) subagent.ProviderDescriptor {
	roleID := "engineering_team"
	if id == "eri_native" {
		roleID = "intern"
	}
	return subagent.ProviderDescriptor{
		ID: id, SupportedRoles: []string{roleID}, Execution: subagent.Background,
		Capabilities: []subagent.Capability{{ID: "bounded_work", Description: "Bounded work"}},
		AccessModes:  access, DefaultAccess: access[0], SendsDataExternally: external,
		Boundaries: []subagent.Boundary{{ID: "no_delivery", Description: "Cannot deliver to the user"}},
	}
}

func newDelegateForTest(t *testing.T, roles []subagent.RoleDescriptor, bindings []subagent.Binding, providers ...subagent.Provider) *Delegate {
	t.Helper()
	registry, err := subagent.NewRegistry(roles, bindings, providers...)
	if err != nil {
		t.Fatal(err)
	}
	delegation, err := NewDelegate(registry)
	if err != nil {
		t.Fatal(err)
	}
	return delegation
}

func TestDelegateExposesRolesWithoutProviderEngineeringTerms(t *testing.T) {
	native := &fakeSubagentProvider{descriptor: fakeProviderDescriptor("eri_native", false, subagent.ReadOnly)}
	codex := &fakeSubagentProvider{descriptor: fakeProviderDescriptor("codex", true, subagent.ReadOnly, subagent.WorkspaceWrite)}
	delegation := newDelegateForTest(t, subagent.DefaultRoles(), []subagent.Binding{
		{RoleID: "intern", ProviderID: "eri_native"}, {RoleID: "engineering_team", ProviderID: "codex"},
	}, native, codex)

	property := delegation.Descriptor().InputSchema["properties"].(map[string]any)["assignee"].(map[string]any)
	got := property["enum"].([]string)
	if len(got) != 2 || got[0] != "engineering_team" || got[1] != "intern" {
		t.Fatalf("assignees = %v", got)
	}
	encoded, _ := json.Marshal(delegation.Descriptor())
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"eri_native", "codex", "provider", "foreground", `"background"`, "tool allowlist", "boundary"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("delegate model surface leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestDelegateFreezesRoleProviderBindingAndReturnsRoleDeferredFact(t *testing.T) {
	codex := &fakeSubagentProvider{descriptor: fakeProviderDescriptor("codex", true, subagent.ReadOnly)}
	delegation := newDelegateForTest(t, subagent.DefaultRoles(), []subagent.Binding{{RoleID: "engineering_team", ProviderID: "codex"}}, codex)
	prepared, err := delegation.Prepare(context.Background(), json.RawMessage(`{"objective":"inspect the workspace","assignee":"engineering_team","access":"read_only"}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared.TaskID, prepared.RunID, prepared.InvocationID = "task-1", "run-1", "intent-1"
	result, err := delegation.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deferred == nil || result.Deferred.Type != "engineering_team" || result.Deferred.ProviderID != "codex" {
		t.Fatalf("deferred result = %+v", result.Deferred)
	}
	if codex.request.RoleID != "engineering_team" || codex.request.ProviderID != "codex" || codex.request.TaskID != "task-1" {
		t.Fatalf("provider request = %+v", codex.request)
	}
}

func TestDelegateRequiresConfirmationForEngineeringWorkspaceWrites(t *testing.T) {
	codex := &fakeSubagentProvider{descriptor: fakeProviderDescriptor("codex", true, subagent.ReadOnly, subagent.WorkspaceWrite)}
	delegation := newDelegateForTest(t, subagent.DefaultRoles(), []subagent.Binding{{RoleID: "engineering_team", ProviderID: "codex"}}, codex)
	prepared, err := delegation.Prepare(context.Background(), json.RawMessage(`{"objective":"implement the change","assignee":"engineering_team","access":"workspace_write"}`))
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := policy.Floor(prepared.Action)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Control != policy.OrdinaryConfirm {
		t.Fatalf("workspace-write control = %q", assessment.Control)
	}
}
